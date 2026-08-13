package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	ID           string       `json:"id"`
	Subject      string       `json:"subject,omitempty"`
	From         string       `json:"from,omitempty"`
	To           string       `json:"to,omitempty"`
	Cc           string       `json:"cc,omitempty"`
	Date         string       `json:"date,omitempty"`
	DateRelative string       `json:"date_relative,omitempty"`
	Tags         []string     `json:"tags,omitempty"`
	Attachments  []Attachment `json:"attachments,omitempty"`
	Body         string       `json:"body,omitempty"`
}

// Attachment describes a MIME part without carrying its bytes. Part is the
// number notmuch_extract needs to write the payload to disk.
type Attachment struct {
	Part        int    `json:"part"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Bytes       int64  `json:"bytes,omitempty"`
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

func collectParts(n any, includeHTML bool, body *strings.Builder, atts *[]Attachment) {
	switch v := n.(type) {
	case []any:
		for _, c := range v {
			collectParts(c, includeHTML, body, atts)
		}
	case map[string]any:
		// Any part carrying a filename is listed, whatever its MIME type —
		// notmuch_extract writes it to disk without inspecting the type.
		if fn := str(v["filename"]); fn != "" {
			num, _ := v["id"].(float64)
			size, _ := v["content-length"].(float64)
			*atts = append(*atts, Attachment{
				Part:        int(num),
				Filename:    fn,
				ContentType: str(v["content-type"]),
				Bytes:       int64(size),
			})
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

// attachmentDir is where extracted parts land. A stable directory (rather than
// a per-process temp dir) means paths handed to the client stay valid across
// server restarts.
func attachmentDir() string {
	if d := os.Getenv("NOTMUCH_MCP_ATTACHMENT_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "notmuch-mcp", "attachments")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "notmuch-mcp", "attachments")
	}
	return filepath.Join(os.TempDir(), "notmuch-mcp-attachments")
}

// maxAttachmentBytes caps a single extraction. The mail store already holds the
// message, so extracting copies bytes — a cap keeps a 2 GB "attachment" from
// filling the disk. Raise it with NOTMUCH_MCP_MAX_ATTACHMENT_BYTES.
const maxAttachmentBytes = 100 << 20

func maxBytes() int64 {
	if v := os.Getenv("NOTMUCH_MCP_MAX_ATTACHMENT_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return maxAttachmentBytes
}

// safeName reduces an attacker-controlled MIME filename to a single path
// element. Mail can name a part "../../.ssh/authorized_keys"; only the base
// name survives, and anything left that isn't a plain filename is replaced.
func safeName(name string, part int) string {
	// Take the last element by either separator. `\` is a legal filename byte on
	// unix, so filepath.Base leaves a Windows-style path whole — split on both.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, name))
	// A leading dot means "", ".", ".." or a dotfile — none of which this should
	// create. Keep the extension so the file type stays obvious to the client.
	if name == "" || strings.HasPrefix(name, ".") {
		ext := filepath.Ext(name)
		if ext == "." {
			ext = ""
		}
		return fmt.Sprintf("part-%d%s", part, ext)
	}
	return name
}

// extractPart writes one MIME part of one message to disk and returns the path.
// The payload is streamed, never buffered, and never returned to the model —
// the client reads the file itself. No MIME type is special-cased: whatever the
// part holds is what lands on disk.
func extractPart(ctx context.Context, messageID string, part int, name string) (string, int64, error) {
	if strings.TrimSpace(messageID) == "" {
		return "", 0, fmt.Errorf("message_id must not be empty")
	}
	if part < 1 {
		return "", 0, fmt.Errorf("part must be 1 or greater, got %d", part)
	}

	// One subdirectory per message, keyed by a hash so an exotic Message-ID
	// can't shape the path. Keeps same-named parts from colliding.
	// No name given: ask notmuch for the part's own filename. Cheap — this
	// returns just that part's metadata, not the message.
	if strings.TrimSpace(name) == "" {
		meta, err := run(ctx, "show", "--format=json", "--part="+strconv.Itoa(part), "--", "id:"+messageID)
		if err != nil {
			return "", 0, err
		}
		var p struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(meta, &p); err != nil {
			return "", 0, fmt.Errorf("parse part %d metadata: %w", part, err)
		}
		name = p.Filename
	}

	sum := sha256.Sum256([]byte(messageID))
	dir := filepath.Join(attachmentDir(), hex.EncodeToString(sum[:6]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, fmt.Errorf("create attachment dir: %w", err)
	}
	path := filepath.Join(dir, safeName(name, part))
	// Defence in depth: safeName already collapsed the name to one element.
	if filepath.Dir(path) != dir {
		return "", 0, fmt.Errorf("refusing to write outside %s", dir)
	}

	cmd := exec.CommandContext(ctx, bin(), "show", "--format=raw",
		"--part="+strconv.Itoa(part), "--", "id:"+messageID)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", 0, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", 0, err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", 0, fmt.Errorf("create %s: %w", path, err)
	}
	limit := maxBytes()
	written, copyErr := io.Copy(f, io.LimitReader(stdout, limit+1))
	closeErr := f.Close()
	// Drain whatever notmuch still wants to write, or Wait blocks on a full pipe.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	switch {
	case waitErr != nil:
		_ = os.Remove(path)
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return "", 0, fmt.Errorf("notmuch show --part=%d id:%s: %s", part, messageID, msg)
	case copyErr != nil:
		_ = os.Remove(path)
		return "", 0, copyErr
	case closeErr != nil:
		_ = os.Remove(path)
		return "", 0, closeErr
	case written > limit:
		_ = os.Remove(path)
		return "", 0, fmt.Errorf("part %d exceeds the %d byte limit; raise NOTMUCH_MCP_MAX_ATTACHMENT_BYTES to extract it", part, limit)
	case written == 0:
		_ = os.Remove(path)
		return "", 0, fmt.Errorf("part %d of id:%s is empty — check the part number from notmuch_show", part, messageID)
	}
	return path, written, nil
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
