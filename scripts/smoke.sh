#!/usr/bin/env bash
# End-to-end smoke test: drive the built binary over stdio JSON-RPC against a
# throwaway notmuch database, so it never touches real mail. Called by
# `just smoke` and by CI.
set -euo pipefail

BINARY=${1:-./notmuch-mcp}
DIR=$(mktemp -d)
trap 'rm -rf "$DIR"' EXIT

mkdir -p "$DIR/mail/cur" "$DIR/mail/new" "$DIR/mail/tmp"
cat > "$DIR/mail/cur/1:2,S" <<'EOF'
From: Alice <alice@example.com>
To: me@example.com
Subject: Test invoice
Message-ID: <smoke@example.com>
Content-Type: text/plain

hello from the smoke test
EOF

cat > "$DIR/nmconf" <<EOF
[database]
path=$DIR/mail
[user]
name=Test
primary_email=me@example.com
[new]
tags=inbox
EOF

export NOTMUCH_CONFIG="$DIR/nmconf"
notmuch new >/dev/null

OUT=$(
  {
    echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}'
    echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'
    echo '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"notmuch_show","arguments":{"query":"tag:inbox"}}}'
    echo '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"notmuch_tag","arguments":{"query":"subject:invoice","add":["smoked"]}}}'
    # Hold stdin open: the server exits on EOF and would drop pending replies.
    sleep 3
  } | "$BINARY"
)

fail() {
  echo "smoke: $1" >&2
  printf '%s\n' "$OUT" >&2
  exit 1
}

if ! grep -q 'hello from the smoke test' <<<"$OUT"; then fail "notmuch_show did not return the message body"; fi
if grep -q '"isError":true' <<<"$OUT"; then fail "a call returned an error"; fi
if ! notmuch search --output=tags '*' | grep -qx smoked; then fail "notmuch_tag did not write the tag"; fi

echo "smoke: OK"
