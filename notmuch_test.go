package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A trimmed but structurally real `notmuch show --format=json` payload: a
// thread whose top-level message has one reply, a multipart/alternative body,
// an attachment, and an HTML part with no inline content.
const showJSON = `[[[
{"id":"a@example.com","match":true,"tags":["inbox","unread"],
 "body":[{"id":1,"content-type":"multipart/mixed","content":[
   {"id":2,"content-type":"multipart/alternative","content":[
     {"id":3,"content-type":"text/plain","content":"hello there"},
     {"id":4,"content-type":"text/html","content":"<p>hello there</p>"}]},
   {"id":5,"content-type":"application/pdf","filename":"invoice.pdf","content-length":9001}]}],
 "headers":{"Subject":"Invoice","From":"Alice <a@example.com>","To":"me@example.com","Date":"Thu, 13 Aug 2026 05:23:27 +0000"}},
[[{"id":"b@example.com","match":true,"tags":["inbox"],
   "body":[{"id":1,"content-type":"text/plain","content":"thanks"}],
   "headers":{"Subject":"Re: Invoice","From":"me@example.com"}},[]]]]]]`

func TestFlatten(t *testing.T) {
	msgs, err := flatten([]byte(showJSON), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	m := msgs[0]
	if m.ID != "a@example.com" || m.Subject != "Invoice" || m.From != "Alice <a@example.com>" {
		t.Errorf("headers not flattened: %+v", m)
	}
	if m.Body != "hello there" {
		t.Errorf("body = %q, want %q (html part must be skipped)", m.Body, "hello there")
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments = %v, want 1", m.Attachments)
	}
	if a := m.Attachments[0]; a.Filename != "invoice.pdf" || a.Part != 5 || a.ContentType != "application/pdf" || a.Bytes != 9001 {
		t.Errorf("attachment = %+v, want invoice.pdf part 5 application/pdf 9001 bytes", a)
	}
	if len(m.Tags) != 2 || m.Tags[0] != "inbox" {
		t.Errorf("tags = %v", m.Tags)
	}
	if msgs[1].ID != "b@example.com" {
		t.Errorf("reply not collected: %+v", msgs[1])
	}
}

func TestFlattenIncludeHTML(t *testing.T) {
	msgs, err := flatten([]byte(showJSON), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	// include_html appends the rendered HTML after the plain body, never raw markup.
	body := msgs[0].Body
	if !strings.HasPrefix(body, "hello there\n\n--- rendered HTML ---\n\n") {
		t.Errorf("body = %q, want plain body then rendered HTML", body)
	}
	if strings.Contains(body, "<p>") {
		t.Errorf("body leaked raw HTML: %q", body)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 0); got != "abcdef" {
		t.Errorf("max=0 must not truncate, got %q", got)
	}
	if got := truncate("abcdef", 3); got[:3] != "abc" || len(got) == 6 {
		t.Errorf("got %q, want truncated", got)
	}
}

func TestCheckTag(t *testing.T) {
	for _, bad := range []string{"", "-flag", "+flag", "two words"} {
		if err := checkTag(bad); err == nil {
			t.Errorf("checkTag(%q) = nil, want error", bad)
		}
	}
	if err := checkTag("to-read/2026"); err != nil {
		t.Errorf("checkTag rejected a valid tag: %v", err)
	}
}

// MIME filenames come from whoever sent the mail, so they are hostile input.
func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"invoice.pdf":                "invoice.pdf",
		"../../.ssh/authorized_keys": "authorized_keys",
		"/etc/passwd":                "passwd",
		`..\..\windows\system32`:     "system32",
		"":                           "part-3",
		"..":                         "part-3",
		".":                          "part-3",
		".bashrc":                    "part-3.bashrc", // no writing dotfiles
		"a\x00b/c.txt":               "c.txt",
		"with\nnewline.txt":          "with_newline.txt",
	}
	for in, want := range cases {
		if got := safeName(in, 3); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
	// Whatever the input, the result is always a single path element.
	for in := range cases {
		got := safeName(in, 3)
		if filepath.Dir(filepath.Join("/base", got)) != "/base" {
			t.Errorf("safeName(%q) = %q escapes its directory", in, got)
		}
	}
}

func TestExtractPartRejectsBadInput(t *testing.T) {
	if _, _, err := extractPart(t.Context(), "", 1, ""); err == nil {
		t.Error("empty message_id must be rejected")
	}
	if _, _, err := extractPart(t.Context(), "a@b", 0, ""); err == nil {
		t.Error("part 0 must be rejected")
	}
}

func TestCheckQuery(t *testing.T) {
	if err := checkQuery("  "); err == nil {
		t.Error("blank query must be rejected")
	}
	if err := checkQuery("tag:inbox"); err != nil {
		t.Errorf("valid query rejected: %v", err)
	}
}

// A message whose only body is HTML: without rendering, its body is empty.
const htmlOnlyJSON = `[[[
{"id":"c@example.com","tags":["inbox"],
 "body":[{"id":1,"content-type":"text/html",
   "content":"<html><head><style>p{color:red}</style></head><body><h1>Invoice 42</h1><p>Due <b>friday</b>.</p></body></html>"}],
 "headers":{"Subject":"Invoice 42","From":"billing@example.com"}},[]]]]`

func TestFlattenHTMLOnlyIsRendered(t *testing.T) {
	msgs, err := flatten([]byte(htmlOnlyJSON), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := msgs[0].Body
	if body == "" {
		t.Fatal("HTML-only message rendered an empty body")
	}
	for _, want := range []string{"Invoice 42", "friday"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q is missing %q", body, want)
		}
	}
	// Markup and CSS must not survive into the model's context.
	for _, bad := range []string{"<h1", "<p>", "color:red", "<style"} {
		if strings.Contains(body, bad) {
			t.Errorf("body %q leaked %q", body, bad)
		}
	}
}
