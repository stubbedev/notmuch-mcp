package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// bin is the notmuch executable. Overridable so a user with a wrapper script
// (or a nix store path) doesn't need it first on PATH.
func bin() string {
	if b := os.Getenv("NOTMUCH_BIN"); b != "" {
		return b
	}
	return "notmuch"
}

// run executes notmuch with the given arguments. Arguments are passed as an
// argv slice — never through a shell — so query strings can't inject commands.
func run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("notmuch %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

// checkQuery rejects an empty query. notmuch treats a bare query as an error
// for search, but for `tag` an accidental empty/whitespace argument list is a
// footgun worth refusing explicitly.
func checkQuery(q string) error {
	if strings.TrimSpace(q) == "" {
		return fmt.Errorf("query must not be empty (use \"*\" to match every message)")
	}
	return nil
}

// checkTag rejects tag names notmuch would misread as flags or separators.
func checkTag(t string) error {
	switch {
	case t == "":
		return fmt.Errorf("tag must not be empty")
	case strings.ContainsAny(t, " \t\n"):
		return fmt.Errorf("tag %q must not contain whitespace", t)
	case strings.HasPrefix(t, "-"), strings.HasPrefix(t, "+"):
		return fmt.Errorf("tag %q must not start with + or -; use the add/remove arguments", t)
	}
	return nil
}

// Message is a flattened notmuch message. `notmuch show --format=json` returns
// deeply nested thread/MIME trees with base64 attachment payloads; feeding that
// to a model wastes a lot of context, so we keep the headers, the text body and
// the attachment names.
type Message struct {
	ID           string   `json:"id"`
	Subject      string   `json:"subject,omitempty"`
	From         string   `json:"from,omitempty"`
	To           string   `json:"to,omitempty"`
	Cc           string   `json:"cc,omitempty"`
	Date         string   `json:"date,omitempty"`
	DateRelative string   `json:"date_relative,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Attachments  []string `json:"attachments,omitempty"`
	Body         string   `json:"body,omitempty"`
}

// flatten walks the show output. The tree alternates arrays (threads, reply
// lists, [message, replies] pairs) and objects (messages), so a generic walk
// that emits every object it meets at message level is enough.
func flatten(raw []byte, includeHTML bool, maxBody int) ([]Message, error) {
	var forest any
	if err := json.Unmarshal(raw, &forest); err != nil {
		return nil, fmt.Errorf("parse notmuch show output: %w", err)
	}
	var out []Message
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case []any:
			for _, c := range v {
				walk(c)
			}
		case map[string]any:
			out = append(out, toMessage(v, includeHTML, maxBody))
		}
	}
	walk(forest)
	return out, nil
}

func toMessage(m map[string]any, includeHTML bool, maxBody int) Message {
	msg := Message{ID: str(m["id"]), DateRelative: str(m["date_relative"])}
	if h, ok := m["headers"].(map[string]any); ok {
		msg.Subject, msg.From = str(h["Subject"]), str(h["From"])
		msg.To, msg.Cc, msg.Date = str(h["To"]), str(h["Cc"]), str(h["Date"])
	}
	if tags, ok := m["tags"].([]any); ok {
		for _, t := range tags {
			msg.Tags = append(msg.Tags, str(t))
		}
	}
	var body strings.Builder
	collectParts(m["body"], includeHTML, &body, &msg.Attachments)
	msg.Body = truncate(strings.TrimSpace(body.String()), maxBody)
	return msg
}

func collectParts(n any, includeHTML bool, body *strings.Builder, atts *[]string) {
	switch v := n.(type) {
	case []any:
		for _, c := range v {
			collectParts(c, includeHTML, body, atts)
		}
	case map[string]any:
		if fn := str(v["filename"]); fn != "" {
			*atts = append(*atts, fn)
		}
		ct := strings.ToLower(str(v["content-type"]))
		s, isText := v["content"].(string)
		if !isText {
			collectParts(v["content"], includeHTML, body, atts)
			return
		}
		if strings.HasPrefix(ct, "text/plain") || (includeHTML && strings.HasPrefix(ct, "text/")) {
			body.WriteString(s)
			body.WriteString("\n")
		}
	}
}

func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf("\n…[truncated, %d bytes total]", len(s))
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
