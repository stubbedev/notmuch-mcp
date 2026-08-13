package main

import "testing"

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
	if len(m.Attachments) != 1 || m.Attachments[0] != "invoice.pdf" {
		t.Errorf("attachments = %v, want [invoice.pdf]", m.Attachments)
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
	if msgs[0].Body != "hello there\n<p>hello there</p>" {
		t.Errorf("body = %q, want plain + html", msgs[0].Body)
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

func TestCheckQuery(t *testing.T) {
	if err := checkQuery("  "); err == nil {
		t.Error("blank query must be rejected")
	}
	if err := checkQuery("tag:inbox"); err != nil {
		t.Errorf("valid query rejected: %v", err)
	}
}
