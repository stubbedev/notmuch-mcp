package main

// HTML → Markdown for mail, via stubbedev/html-to-md's pipeline (the aerc
// text/html filter) linked in-process, so a release binary needs nothing but
// notmuch. Generic converters faithfully keep everything a marketing email
// contains — Outlook conditionals, display:none duplicates, nested layout
// tables, decorative anchors — which is most of its bytes.

import "github.com/stubbedev/html-to-md/go/htmltomd"

// htmlToMarkdown renders a mail's HTML part as Markdown.
func htmlToMarkdown(src string) string { return htmltomd.Convert(src) }
