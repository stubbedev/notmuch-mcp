# notmuch-mcp

An MCP server over [notmuch](https://notmuchmail.org/). It lets an agent search,
read and tag your mail.

It cannot send, delete, move or modify mail files. The only notmuch subcommands
it ever runs are `search`, `show`, `count` and `tag`, and they are invoked as an
argv slice — never through a shell.

## Install

```sh
go install github.com/stubbedev/notmuch-mcp@latest
```

Or grab a binary from [releases](https://github.com/stubbedev/notmuch-mcp/releases).

Requires `notmuch` on `PATH` with an indexed database (`notmuch new`). Set
`NOTMUCH_BIN` to point at a specific binary, and `NOTMUCH_CONFIG` to use a
database other than the default.

## Use

The server speaks MCP over stdio. For Claude Code:

```sh
claude mcp add notmuch -- notmuch-mcp
```

Or in an `mcpServers` config block:

```json
{
  "mcpServers": {
    "notmuch": {
      "command": "notmuch-mcp"
    }
  }
}
```

## Tools

| Tool             | What it does                                                                                     |
| ---------------- | ------------------------------------------------------------------------------------------------ |
| `notmuch_search` | Thread summaries for a query — subject, authors, tags, date. No bodies. `limit` defaults to 20.  |
| `notmuch_show`   | Reads messages: headers, tags, plain-text body, attachment *names*. Bodies truncate at 4000 bytes. |
| `notmuch_count`  | Counts messages, threads or files matching a query.                                               |
| `notmuch_tags`   | Lists tags in use, so the agent reuses names instead of inventing them.                           |
| `notmuch_tag`    | Adds and/or removes tags on every message matching a query. The only write.                       |

`notmuch_show` flattens notmuch's nested thread/MIME JSON down to the headers,
the text body and the attachment filenames. Attachment payloads are never
returned, and HTML parts are skipped unless you pass `include_html` — otherwise
a single marketing mail would eat the context window.

`notmuch_tag` writes to the mail store and has no undo beyond the inverse call.
It refuses an empty query, and refuses tag names starting with `+`/`-` or
containing whitespace. A broad query still retags a lot of mail — the tool
description tells the model to `notmuch_count` first.

## Development

```sh
just          # list recipes
just check    # lint + test
just smoke    # drive the binary over stdio against a throwaway notmuch db
just test
just release-patch
```
