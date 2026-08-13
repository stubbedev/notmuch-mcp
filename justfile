# justfile for notmuch-mcp
# Run `just` to see all available commands

BINARY := "notmuch-mcp"

# Default recipe - list all available commands
default:
    @just --list

# Check if required tools are installed
check-tools:
    #!/usr/bin/env bash
    command -v go >/dev/null 2>&1 || { echo "go is required but not installed." >&2; exit 1; }
    command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint is required but not installed. See: https://golangci-lint.run/usage/install/" >&2; exit 1; }
    command -v gh >/dev/null 2>&1 || { echo "gh (GitHub CLI) is required but not installed. See: https://cli.github.com/" >&2; exit 1; }
    command -v notmuch >/dev/null 2>&1 || { echo "notmuch is a runtime requirement (nix profile install nixpkgs#notmuch / apt install notmuch)." >&2; exit 1; }
    @echo "All required tools are installed!"

# =============================================================================
# Build
# =============================================================================

# Build the binary
build:
    go build -o {{BINARY}} .

# Build with version info embedded
build-release:
    #!/usr/bin/env bash
    VERSION=$(git describe --tags --always --dirty)
    COMMIT=$(git rev-parse --short HEAD)
    BUILD_DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    go build -trimpath -ldflags "-s -w -X main.Version=$VERSION -X main.Commit=$COMMIT -X main.BuildDate=$BUILD_DATE" -o {{BINARY}} .

# Install binary to GOPATH/bin
install: build
    go install .
    @echo "Installed to $(go env GOPATH)/bin/{{BINARY}}"

# Remove build artifacts
clean:
    rm -f {{BINARY}} coverage.out coverage.html

# Download and tidy Go modules
mod:
    go mod download
    go mod tidy

# =============================================================================
# Lint & test
# =============================================================================

# Format Go code (golangci-lint v2 formatters — gofmt + goimports, per .golangci.yml)
fmt:
    golangci-lint fmt ./...

# Run go vet
vet:
    go vet ./...

# Format, vet, then run the linters. The mutating local-dev gate.
lint: fmt
    go vet ./...
    golangci-lint run ./...

# Auto-fix everything mechanically fixable (formatting + golangci --fix).
lint-fix:
    golangci-lint fmt ./...
    golangci-lint run --fix ./...

# Strict read-only check — same logic CI runs, for local pre-push verification.
lint-check:
    #!/usr/bin/env bash
    set -euo pipefail
    out=$(golangci-lint fmt --diff ./...)
    if [ -n "$out" ]; then
        echo "code is not formatted; run 'just fmt':"
        printf '%s\n' "$out"
        exit 1
    fi
    go vet ./...
    golangci-lint run ./...

# Run tests. -timeout 60s so a hung test shows up in seconds, not 10 minutes.
test:
    go test -race -timeout 60s ./...

# Run tests with coverage
test-cover:
    go test -coverprofile=coverage.out ./...
    go tool cover -html=coverage.out -o coverage.html
    @echo "Coverage report: coverage.html"

# Drive the server over stdio against a throwaway notmuch db (never real mail)
smoke: build
    ./scripts/smoke.sh ./{{BINARY}}

# Keep flake.nix's `vendorHash` aligned with the current go.sum.
#
# A sha256 of go.sum is cached as a `# go-sum:` line in flake.nix. When it
# matches go.sum on disk, sync-flake returns immediately without running
# `nix build` — cheap enough to run on every `just check`, so a `go get` can
# never push a commit that breaks the nix CI job. Pass `--force` to bypass.
sync-flake force="":
    #!/usr/bin/env bash
    set -euo pipefail
    GO_SUM_HASH=$(sha256sum go.sum | awk '{print $1}')
    CACHED_HASH=$(awk -F': ' '/^[[:space:]]*#[[:space:]]*go-sum:/ {print $2; exit}' flake.nix | tr -d ' ')
    if [ "{{force}}" != "--force" ] && [ "$GO_SUM_HASH" = "$CACHED_HASH" ]; then
        echo "sync-flake: up-to-date (go.sum=$GO_SUM_HASH)"
        exit 0
    fi
    echo "sync-flake: refreshing vendorHash"
    SENTINEL="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
    sed -i -E 's|^(\s*vendorHash = )"sha256-[^"]*";|\1"'"$SENTINEL"'";|' flake.nix
    set +e
    OUT=$(nix build .#notmuch-mcp --no-link 2>&1)
    BUILD_STATUS=$?
    set -e
    NEW_HASH=$(printf '%s\n' "$OUT" | awk '/got:[[:space:]]*sha256-/ {print $2; exit}')
    if [ -z "$NEW_HASH" ]; then
        if [ "$BUILD_STATUS" = "0" ]; then
            echo "sync-flake: unexpected nix build success with sentinel hash" >&2
            echo "$OUT" >&2
            exit 1
        fi
        echo "$OUT" >&2
        echo "sync-flake: nix build did not print 'got: sha256-…'" >&2
        exit 1
    fi
    sed -i -E 's|^(\s*vendorHash = )"sha256-[^"]*";|\1"'"$NEW_HASH"'";|' flake.nix
    sed -i -E 's|^(\s*# go-sum:).*|\1 '"$GO_SUM_HASH"'|' flake.nix
    echo "sync-flake: vendorHash=$NEW_HASH go-sum=$GO_SUM_HASH"
    # Hard guard: never leave the sentinel behind — CI would fail on hash mismatch.
    if grep -q 'vendorHash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="' flake.nix; then
        echo "sync-flake: refusing to leave sentinel vendorHash in flake.nix" >&2
        exit 1
    fi
    nix build .#notmuch-mcp --no-link

# Run everything CI runs
check: lint test sync-flake

# =============================================================================
# Release
# =============================================================================

# Pre-release checks (on default branch, clean tree after check)
_release-checks:
    #!/usr/bin/env bash
    set -euo pipefail
    BRANCH=$(git rev-parse --abbrev-ref HEAD)
    DEFAULT_BRANCH=$(git rev-parse --abbrev-ref origin/HEAD 2>/dev/null | sed 's|^origin/||' || echo master)
    if [ "$BRANCH" != "$DEFAULT_BRANCH" ]; then
        echo "Error: Not on default branch '$DEFAULT_BRANCH' (currently on '$BRANCH')." >&2
        exit 1
    fi
    just check
    if [ -n "$(git status --porcelain)" ]; then
        echo "Changes detected (formatting / vendorHash). Staging and committing..."
        git add -A
        git commit -m "chore: sync generated artifacts for release"
    fi
    echo "Updating flake.lock..."
    nix flake update
    if [ -n "$(git status --porcelain flake.lock)" ]; then
        git add flake.lock
        git commit -m "chore: update flake.lock for release"
    fi
    echo "Re-validating nix build against the new lock..."
    just sync-flake --force
    if [ -n "$(git status --porcelain flake.nix)" ]; then
        git add flake.nix
        git commit -m "chore: update vendorHash for release"
    fi

_release level: _release-checks
    #!/usr/bin/env bash
    set -euo pipefail
    CURRENT_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
    CURRENT_VERSION=${CURRENT_TAG#v}
    MAJOR=$(echo "$CURRENT_VERSION" | cut -d. -f1)
    MINOR=$(echo "$CURRENT_VERSION" | cut -d. -f2)
    PATCH=$(echo "$CURRENT_VERSION" | cut -d. -f3)
    case "{{level}}" in
        major) NEW_VERSION="v$((MAJOR + 1)).0.0" ;;
        minor) NEW_VERSION="v${MAJOR}.$((MINOR + 1)).0" ;;
        patch) NEW_VERSION="v${MAJOR}.${MINOR}.$((PATCH + 1))" ;;
        *) echo "unknown level {{level}}" >&2; exit 1 ;;
    esac
    echo "Bumping from $CURRENT_TAG to $NEW_VERSION"
    git tag -a "$NEW_VERSION" -m "Release $NEW_VERSION"
    git push origin HEAD
    git push origin "$NEW_VERSION"
    gh release create "$NEW_VERSION" --generate-notes
    echo "Released $NEW_VERSION"

# Release a new major version (X.y.z -> X+1.0.0)
release-major: (_release "major")

# Release a new minor version (x.Y.z -> x.Y+1.0)
release-minor: (_release "minor")

# Release a new patch version (x.y.Z -> x.y.Z+1)
release-patch: (_release "patch")

# Preview what versions would be created (dry-run)
release-preview:
    #!/usr/bin/env bash
    CURRENT_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
    CURRENT_VERSION=${CURRENT_TAG#v}
    MAJOR=$(echo "$CURRENT_VERSION" | cut -d. -f1)
    MINOR=$(echo "$CURRENT_VERSION" | cut -d. -f2)
    PATCH=$(echo "$CURRENT_VERSION" | cut -d. -f3)
    echo "Current: $CURRENT_TAG"
    echo "  major: v$((MAJOR + 1)).0.0"
    echo "  minor: v${MAJOR}.$((MINOR + 1)).0"
    echo "  patch: v${MAJOR}.${MINOR}.$((PATCH + 1))"
