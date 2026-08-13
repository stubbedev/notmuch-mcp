// Command notmuch-mcp is a Model Context Protocol server over the notmuch mail
// indexer. It reads and tags mail. It cannot send, delete or move anything: the
// only notmuch subcommands it ever invokes are search, show, count and tag.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Set via -ldflags at release time.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

const defaultLimit = 20

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("notmuch-mcp %s (%s) built %s\n", Version, Commit, BuildDate)
		return
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "notmuch-mcp", Version: Version}, nil)
	addTools(server)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Printf("server failed: %v", err)
		os.Exit(1)
	}
}

type searchArgs struct {
	Query  string `json:"query" jsonschema:"notmuch query, e.g. 'tag:inbox and from:alice' or 'subject:invoice and date:last_week..'"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max threads to return (default 20)"`
	Offset int    `json:"offset,omitempty" jsonschema:"skip this many threads, for paging"`
	Sort   string `json:"sort,omitempty" jsonschema:"newest-first (default) or oldest-first"`
}

type showArgs struct {
	Query        string `json:"query" jsonschema:"notmuch query selecting the messages to read; id:<message-id> reads one message"`
	Limit        int    `json:"limit,omitempty" jsonschema:"max messages to return (default 20)"`
	EntireThread bool   `json:"entire_thread,omitempty" jsonschema:"include every message in the matching threads, not just the matches"`
	IncludeHTML  bool   `json:"include_html,omitempty" jsonschema:"include HTML parts when a message has no plain-text alternative (verbose)"`
	MaxBodyChars int    `json:"max_body_chars,omitempty" jsonschema:"truncate each body to this many bytes (default 4000, 0 for unlimited)"`
}

type extractArgs struct {
	MessageID string `json:"message_id" jsonschema:"the message's Message-ID, as returned in notmuch_show's id field (without the id: prefix)"`
	Part      int    `json:"part" jsonschema:"the part number from notmuch_show's attachments list"`
	Filename  string `json:"filename,omitempty" jsonschema:"name to save as; defaults to the attachment's own filename"`
}

type countArgs struct {
	Query  string `json:"query" jsonschema:"notmuch query to count"`
	Output string `json:"output,omitempty" jsonschema:"messages (default), threads or files"`
}

type tagsArgs struct {
	Query string `json:"query,omitempty" jsonschema:"restrict to tags used by messages matching this query (default '*', all tags)"`
}

type tagArgs struct {
	Query  string   `json:"query" jsonschema:"notmuch query selecting the messages to retag; every match is modified"`
	Add    []string `json:"add,omitempty" jsonschema:"tags to add"`
	Remove []string `json:"remove,omitempty" jsonschema:"tags to remove"`
}

func addTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "notmuch_search",
		Description: "Search mail. Returns one JSON summary per matching thread (subject, authors, tags, date, thread id) — not message bodies. Use notmuch_show to read them.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a searchArgs) (*mcp.CallToolResult, any, error) {
		if err := checkQuery(a.Query); err != nil {
			return nil, nil, err
		}
		sort := a.Sort
		if sort == "" {
			sort = "newest-first"
		}
		if sort != "newest-first" && sort != "oldest-first" {
			return nil, nil, fmt.Errorf("sort must be newest-first or oldest-first, got %q", sort)
		}
		out, err := run(ctx, "search", "--format=json", "--sort="+sort,
			"--limit="+strconv.Itoa(limitOr(a.Limit)), "--offset="+strconv.Itoa(max(a.Offset, 0)),
			"--", a.Query)
		if err != nil {
			return nil, nil, err
		}
		return text(string(out)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "notmuch_show",
		Description: "Read messages matching a query. Returns flattened messages: headers, tags, plain-text body, and an attachments list (part number, filename, type, size). " +
			"Attachment bytes are never returned inline — call notmuch_extract with the part number to write one to disk.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a showArgs) (*mcp.CallToolResult, any, error) {
		if err := checkQuery(a.Query); err != nil {
			return nil, nil, err
		}
		raw, err := run(ctx, "show", "--format=json",
			"--entire-thread="+strconv.FormatBool(a.EntireThread),
			"--include-html="+strconv.FormatBool(a.IncludeHTML),
			"--body=true", "--limit="+strconv.Itoa(limitOr(a.Limit)),
			"--", a.Query)
		if err != nil {
			return nil, nil, err
		}
		maxBody := 4000
		if a.MaxBodyChars != 0 {
			maxBody = a.MaxBodyChars
		}
		msgs, err := flatten(raw, a.IncludeHTML, maxBody)
		if err != nil {
			return nil, nil, err
		}
		buf, err := json.Marshal(msgs)
		if err != nil {
			return nil, nil, err
		}
		return text(string(buf)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "notmuch_extract",
		Description: "Write one attachment (any file type — PDF, image, zip, spreadsheet, anything) to a file on disk and return its path. " +
			"The bytes go to disk, not through this conversation, so read or process the returned path with your own file tools. " +
			"Get message_id and part from notmuch_show's attachments list.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a extractArgs) (*mcp.CallToolResult, any, error) {
		path, n, err := extractPart(ctx, a.MessageID, a.Part, a.Filename)
		if err != nil {
			return nil, nil, err
		}
		return text(fmt.Sprintf("wrote %d bytes to %s", n, path)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "notmuch_count",
		Description: "Count messages, threads or files matching a query. Cheap way to size a query before showing it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a countArgs) (*mcp.CallToolResult, any, error) {
		if err := checkQuery(a.Query); err != nil {
			return nil, nil, err
		}
		output := a.Output
		if output == "" {
			output = "messages"
		}
		switch output {
		case "messages", "threads", "files":
		default:
			return nil, nil, fmt.Errorf("output must be messages, threads or files, got %q", output)
		}
		out, err := run(ctx, "count", "--output="+output, "--", a.Query)
		if err != nil {
			return nil, nil, err
		}
		return text(string(out)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "notmuch_tags",
		Description: "List tags in use. Call this before tagging so you reuse existing tag names instead of inventing new ones.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a tagsArgs) (*mcp.CallToolResult, any, error) {
		query := a.Query
		if query == "" {
			query = "*"
		}
		out, err := run(ctx, "search", "--format=json", "--output=tags", "--", query)
		if err != nil {
			return nil, nil, err
		}
		return text(string(out)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "notmuch_tag",
		Description: "Add and/or remove tags on every message matching the query. This writes to the mail store. Check notmuch_count first — a broad query retags a lot of mail, and there is no undo beyond running the inverse tag call.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a tagArgs) (*mcp.CallToolResult, any, error) {
		if err := checkQuery(a.Query); err != nil {
			return nil, nil, err
		}
		if len(a.Add) == 0 && len(a.Remove) == 0 {
			return nil, nil, fmt.Errorf("provide at least one tag in add or remove")
		}
		args := []string{"tag"}
		for _, t := range a.Add {
			if err := checkTag(t); err != nil {
				return nil, nil, err
			}
			args = append(args, "+"+t)
		}
		for _, t := range a.Remove {
			if err := checkTag(t); err != nil {
				return nil, nil, err
			}
			args = append(args, "-"+t)
		}
		args = append(args, "--", a.Query)

		n, err := run(ctx, "count", "--output=messages", "--", a.Query)
		if err != nil {
			return nil, nil, err
		}
		if _, err := run(ctx, args...); err != nil {
			return nil, nil, err
		}
		return text(fmt.Sprintf("tagged %s messages matching %q (add=%v remove=%v)",
			strings.TrimSpace(string(n)), a.Query, a.Add, a.Remove)), nil, nil
	})
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func limitOr(n int) int {
	if n <= 0 {
		return defaultLimit
	}
	return n
}
