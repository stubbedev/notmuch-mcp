package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// corpus is the fixture set. Point CORPUS at a directory of dumped HTML parts
// to re-run the parity check against real mail, which can't live in the repo.
func corpus() string {
	if d := os.Getenv("CORPUS"); d != "" {
		return filepath.Join(d, "*.html")
	}
	return "testdata/*.html"
}

// update rewrites the golden files instead of comparing against them.
var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// Ported verbatim from html-to-md's only unit test (src/main.rs,
// `decodes_literal_unicode_escapes`), so both implementations are held to the
// same assertions.
func TestDecodesLiteralUnicodeEscapes(t *testing.T) {
	cases := [][2]string{
		// 4-hex, 8-hex, brace form, and a UTF-16 surrogate pair (🧠).
		{`June 13 – June 20`, "June 13 – June 20"},
		{`\U0001f9e0 Stop`, "🧠 Stop"},
		{`x\u{1F9E0}y`, "x🧠y"},
		{`🧠`, "🧠"},
		// Non-escapes and malformed sequences pass through untouched.
		{`the \understood plan`, `the \understood plan`},
		{`\uZZZZ`, `\uZZZZ`},
		{"no escapes here", "no escapes here"},
		// A lone high surrogate is invalid → left verbatim.
		{`\uD83E!`, `\uD83E!`},
	}
	for _, c := range cases {
		if got := decodeUnicodeEscapes(c[0]); got != c[1] {
			t.Errorf("decodeUnicodeEscapes(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestHTMLToMarkdownBasics(t *testing.T) {
	got := htmlToMarkdown(`<h1>Title</h1><p>Body <b>text</b>.</p>`, 80)
	if !strings.Contains(got, "# Title") || !strings.Contains(got, "**text**") {
		t.Errorf("got %q", got)
	}
}

// Fixtures are synthetic — real mail can't be committed — but each targets a
// pass in the pipeline: layout vs data tables, flex rows, stat headings, IE
// conditionals, hidden responsive duplicates, empty/decorative/garbage anchors,
// punctuation emphasis, invisible padding, heading compression, link rows, wrap.
func fixtures(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(corpus())
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	return files
}

// TestGolden pins the rendered output. Regenerate with `just golden` and review
// the diff when a change is intentional.
func TestGolden(t *testing.T) {
	for _, f := range fixtures(t) {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			got := htmlToMarkdown(string(raw), 80)
			golden := strings.TrimSuffix(f, ".html") + ".golden"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("output drifted:\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

// TestParityWithRust holds this port byte-for-byte against the upstream
// stubbedev/html-to-md binary it was ported from. It skips where that binary
// isn't installed (CI, release machines); the golden files guard behaviour there.
func TestParityWithRust(t *testing.T) {
	bin, err := exec.LookPath("html-to-md")
	if err != nil {
		t.Skip("html-to-md not installed")
	}
	for _, f := range fixtures(t) {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin)
			cmd.Stdin = strings.NewReader(string(raw))
			cmd.Env = append(os.Environ(), "AERC_FILTER_WIDTH=80")
			want, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := htmlToMarkdown(string(raw), 80); strings.TrimRight(got, "\n") != strings.TrimRight(string(want), "\n") {
				t.Errorf("diverged from html-to-md:\n--- go ---\n%s\n--- rust ---\n%s", got, want)
			}
		})
	}
}
