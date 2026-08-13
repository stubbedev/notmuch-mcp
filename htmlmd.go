package main

// HTML → Markdown for mail, ported from stubbedev/html-to-md (Rust, an aerc
// text/html filter). Kept in-process rather than shelling out so a release
// binary needs nothing but notmuch.
//
// Generic HTML→Markdown converters faithfully preserve everything a marketing
// email contains — Outlook conditionals, display:none responsive duplicates,
// nested layout tables, decorative anchors — which is most of its bytes. On a
// sample of real inbox HTML this pipeline produced 15% of the raw size against
// a general-purpose converter's 27%.
//
// Pipeline:
//  1. Strip non-comment IE conditionals before parsing.
//  2. Parse.
//  3. DOM surgery: comments, namespaced/invisible/non-text elements, text
//     normalisation, link-text flattening, punctuation emphasis, stat headings,
//     flex rows, layout tables, empty anchors.
//  4. Walk the cleaned DOM emitting Markdown.
//  5. Collapse blank runs, hard-wrap.
//
// The port is deliberately literal — same passes, same order, same heuristics —
// so fixes can be carried across in either direction.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const defaultWrapWidth = 80

// htmlToMarkdown renders a mail's HTML part as Markdown.
func htmlToMarkdown(src string, width int) string {
	if width <= 0 {
		width = defaultWrapWidth
	}
	// Strip non-comment IE conditionals before parsing so Outlook bullet spans
	// (<![if !supportLists]><span>·</span><![endif]>) and other Outlook-only
	// blocks don't appear in the DOM as regular text nodes. The comment form
	// <!--[if mso]>…<![endif]--> is handled by the parser plus stripComments.
	doc, err := html.Parse(strings.NewReader(stripIEConditionals(src)))
	if err != nil {
		return ""
	}

	stripComments(doc)
	// Outlook/Word namespaced tags (o:p, w:WordDocument, v:shape, …) survive
	// HTML5 parsing as elements with a literal colon in their name.
	dropElements(doc, func(n *html.Node) bool { return strings.Contains(n.Data, ":") })
	// Responsive emails duplicate content: one version visible on desktop, one
	// on mobile, toggled via CSS. Since stylesheets are stripped, both would
	// render. Drop any element whose inline style hides it.
	dropElements(doc, func(n *html.Node) bool {
		s := strings.ToLower(attr(n, "style"))
		return strings.Contains(s, "display:none") || strings.Contains(s, "display: none") ||
			strings.Contains(s, "visibility:hidden") || strings.Contains(s, "visibility: hidden")
	})
	dropElements(doc, func(n *html.Node) bool {
		switch n.Data {
		case "head", "style", "script", "iframe", "img", "colgroup", "col",
			"figure", "picture", "source", "svg", "canvas", "video", "audio",
			"area", "map", "noscript":
			return true
		}
		return false
	})
	// Must run before dropEmptyAnchors so anchors padded with zero-width
	// characters become text-empty.
	normaliseTextNodes(doc)
	flattenLinkText(doc)
	unwrapPunctuationEmphasis(doc)
	demoteStatHeadings(doc)
	inlineFlexRowDivs(doc)
	flattenTables(doc)
	// Marketing emails wrap a brand logo in <a href="…"><img></a>; once the
	// <img> is gone the anchor has no visible text.
	dropEmptyAnchors(doc)

	return strings.TrimLeft(toMarkdown(doc, width), "\n")
}

// ─── DOM helpers ────────────────────────────────────────────────────────────

// walk visits n and all its descendants in document order. The tree must not be
// mutated during a walk — collect first, mutate after.
func walk(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

// collectAll returns n plus every descendant matching pred, in document order.
func collectAll(n *html.Node, pred func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	walk(n, func(c *html.Node) {
		if pred(c) {
			out = append(out, c)
		}
	})
	return out
}

// descendants excludes n itself, matching kuchikiki's descendants().
func descendants(n *html.Node, pred func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, func(d *html.Node) {
			if pred(d) {
				out = append(out, d)
			}
		})
	}
	return out
}

func isElement(n *html.Node, name string) bool {
	return n.Type == html.ElementNode && n.Data == name
}

func isElementOneOf(n *html.Node, names ...string) bool {
	if n.Type != html.ElementNode {
		return false
	}
	for _, name := range names {
		if n.Data == name {
			return true
		}
	}
	return false
}

func attr(n *html.Node, key string) string {
	if n.Type != html.ElementNode {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func detach(n *html.Node) {
	if n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
}

// insertBefore detaches node and inserts it ahead of ref.
func insertBefore(ref, node *html.Node) {
	if ref.Parent == nil {
		return
	}
	detach(node)
	ref.Parent.InsertBefore(node, ref)
}

func appendChild(parent, node *html.Node) {
	detach(node)
	parent.AppendChild(node)
}

func childNodes(n *html.Node) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, c)
	}
	return out
}

func newText(s string) *html.Node {
	return &html.Node{Type: html.TextNode, Data: s}
}

func newElement(name string) *html.Node {
	return &html.Node{Type: html.ElementNode, Data: name, DataAtom: atom.Lookup([]byte(name))}
}

func depth(n *html.Node) int {
	d := 0
	for p := n.Parent; p != nil; p = p.Parent {
		d++
	}
	return d
}

// subtreeText concatenates every text node in n's subtree, n included.
func subtreeText(n *html.Node) string {
	var b strings.Builder
	walk(n, func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	})
	return b.String()
}

// normaliseTextNodes rewrites text nodes in place so later passes (empty-anchor
// detection, cell blankness) see the cleaned text.
func normaliseTextNodes(root *html.Node) {
	for _, t := range collectAll(root, func(n *html.Node) bool { return n.Type == html.TextNode }) {
		t.Data = cleanInvisibles(t.Data)
	}
}

// cleanInvisibles drops zero-width/format characters (emails use them as
// preview-text padding) and turns NBSP-class spaces into plain spaces so
// trimming and whitespace collapsing behave.
func cleanInvisibles(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, c := range s {
		switch {
		case c == '\u00AD', // soft hyphen
			c == '\u034F',                // combining grapheme joiner (Klaviyo et al.)
			c == '\u061C',                // arabic letter mark
			c == '\u115F', c == '\u1160', // hangul choseong/jungseong filler
			c == '\u17B4', c == '\u17B5', // khmer vowel inherent aq/aa
			c == '\u180E',                          // mongolian vowel separator
			c >= '\u200B' && c <= '\u200F',         // ZWSP, ZWNJ, ZWJ, LRM, RLM
			c >= '\u202A' && c <= '\u202E',         // bidi formatting
			c >= '\u2060' && c <= '\u2064',         // word joiner + invisible operators
			c >= '\u2066' && c <= '\u2069',         // bidi isolates
			c == '\u3164',                          // hangul filler
			c >= '\uFE00' && c <= '\uFE0F',         // variation selectors
			c == '\uFEFF',                          // BOM / zero-width nbsp
			c == '\uFFA0',                          // halfwidth hangul filler
			c >= '\U000E0020' && c <= '\U000E007F': // tag characters
			// Zero-width / format characters, used as preview-text padding. Drop.
		case c == '\u00A0',
			c >= '\u2000' && c <= '\u200A',
			c == '\u202F', c == '\u205F', c == '\u3000':
			// NBSP-class horizontal whitespace becomes a plain space, so runs
			// collapse and trimming works as expected.
			b.WriteByte(' ')
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

func stripComments(root *html.Node) {
	for _, c := range collectAll(root, func(n *html.Node) bool { return n.Type == html.CommentNode }) {
		detach(c)
	}
}

func dropElements(root *html.Node, pred func(*html.Node) bool) {
	victims := collectAll(root, func(n *html.Node) bool {
		return n.Type == html.ElementNode && pred(n)
	})
	for _, v := range victims {
		detach(v)
	}
}

func dropEmptyAnchors(root *html.Node) {
	for _, a := range collectAll(root, func(n *html.Node) bool { return isElement(n, "a") }) {
		trimmed := strings.TrimSpace(subtreeText(a))
		if trimmed == "" || isDecorativeGlyph(trimmed) {
			detach(a)
		}
	}
}

// isDecorativeGlyph reports whether s is a single non-alphanumeric character
// (›, », →, ▸ …) — an icon link, noise in a text reader.
func isDecorativeGlyph(s string) bool {
	rs := []rune(s)
	return len(rs) == 1 && !isAlphanumeric(rs[0])
}

func isAlphanumeric(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// unwrapPunctuationEmphasis unwraps emphasis tags whose content is purely
// punctuation (≤3 chars). Sentry tag rows wrap a literal `=` in <em>, which
// serialises as `*\=*` — visible noise.
func unwrapPunctuationEmphasis(root *html.Node) {
	candidates := collectAll(root, func(n *html.Node) bool {
		return isElementOneOf(n, "em", "i", "strong", "b", "u", "mark", "small")
	})
	for _, el := range candidates {
		text := subtreeText(el)
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			// Whitespace-only emphasis (<em> </em>) often glues two adjacent
			// inlines together; detaching would mash the neighbours. Merge the
			// space into a sibling first.
			if text != "" {
				mergeSeparatorSpace(el)
			}
			detach(el)
			continue
		}
		if len([]rune(trimmed)) <= 3 && allRunes(trimmed, func(r rune) bool {
			return !isAlphanumeric(r) && !unicode.IsSpace(r)
		}) {
			for _, k := range childNodes(el) {
				insertBefore(el, k)
			}
			detach(el)
		}
	}
}

func allRunes(s string, pred func(rune) bool) bool {
	for _, r := range s {
		if !pred(r) {
			return false
		}
	}
	return true
}

// mergeSeparatorSpace pushes a separator space onto a text sibling of el,
// preferring the previous one so a leading space can't start a "blank" line.
func mergeSeparatorSpace(el *html.Node) {
	if prev := el.PrevSibling; prev != nil && prev.Type == html.TextNode {
		if !strings.HasSuffix(prev.Data, " ") {
			prev.Data += " "
		}
		return
	}
	if next := el.NextSibling; next != nil && next.Type == html.TextNode {
		if !strings.HasPrefix(next.Data, " ") {
			next.Data = " " + next.Data
		}
		return
	}
	insertBefore(el, newText(" "))
}

// demoteStatHeadings rewrites headings whose whole content is a short number
// (Sentry's digest emits `<h1>471k</h1>` purely for visual scale) as a bold
// paragraph, so the document's heading hierarchy isn't skewed by a stat.
func demoteStatHeadings(root *html.Node) {
	candidates := collectAll(root, func(n *html.Node) bool {
		return isElementOneOf(n, "h1", "h2", "h3", "h4", "h5", "h6")
	})
	for _, h := range candidates {
		trimmed := strings.TrimSpace(subtreeText(h))
		if trimmed == "" || len([]rune(trimmed)) > 12 || !isStatText(trimmed) {
			continue
		}
		// <p><strong>…</strong></p> keeps the visible scale while staying a
		// block, so flattenTables won't glue it to a neighbouring inline link.
		para := newElement("p")
		strong := newElement("strong")
		para.AppendChild(strong)
		for _, k := range childNodes(h) {
			appendChild(strong, k)
		}
		insertBefore(h, para)
		detach(h)
	}
}

func isStatText(s string) bool {
	sawDigit := false
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			sawDigit = true
		case strings.ContainsRune("., kKMBmsµh", c):
		default:
			return false
		}
	}
	return sawDigit
}

// inlineFlexRowDivs collapses CSS flex rows. Sentry's "Issues with the most
// errors" rows are flex containers wrapping a count, a link block and a status
// pill; treating each <div> as a paragraph explodes one row into 4–5 blocks.
func inlineFlexRowDivs(root *html.Node) {
	// A row has a handful of direct children; a page-level flex wrapper has
	// either one or dozens. Use direct child count as the differentiator.
	const maxFlexDirectChildren = 8
	flexParents := collectAll(root, func(n *html.Node) bool {
		if !isFlexDiv(n) {
			return false
		}
		for p := n.Parent; p != nil; p = p.Parent {
			if isFlexDiv(p) {
				return false
			}
		}
		direct := 0
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				direct++
			}
		}
		return direct >= 2 && direct <= maxFlexDirectChildren
	})

	type target struct {
		depth int
		node  *html.Node
	}
	var targets []target
	for _, parent := range flexParents {
		for _, d := range descendants(parent, func(n *html.Node) bool { return isElement(n, "div") }) {
			targets = append(targets, target{depth(d), d})
		}
	}
	// Deepest first, so an inner div is unwrapped before its container.
	sortStable(targets, func(a, b target) bool { return a.depth > b.depth })

	for _, t := range targets {
		d := t.node
		if d.Parent == nil {
			continue
		}
		hasBlock := len(descendants(d, func(n *html.Node) bool {
			return isElementOneOf(n, "table", "ul", "ol", "li", "h1", "h2", "h3",
				"h4", "h5", "h6", "pre", "blockquote", "hr", "p", "div")
		})) > 0
		if hasBlock {
			continue
		}
		for _, c := range childNodes(d) {
			insertBefore(d, c)
		}
		insertBefore(d, newText(" "))
		detach(d)
	}
}

func isFlexDiv(n *html.Node) bool {
	if !isElement(n, "div") {
		return false
	}
	s := attr(n, "style")
	return strings.Contains(s, "display: flex") || strings.Contains(s, "display:flex")
}

// flattenLinkText squashes newlines and tabs inside <a> text. Markdown link text
// split across physical lines breaks rendering and confuses the wrap pass, which
// treats each line in isolation.
func flattenLinkText(root *html.Node) {
	for _, a := range collectAll(root, func(n *html.Node) bool { return isElement(n, "a") }) {
		for _, t := range collectAll(a, func(n *html.Node) bool { return n.Type == html.TextNode }) {
			if strings.ContainsAny(t.Data, "\n\t") {
				t.Data = strings.NewReplacer("\n", " ", "\t", " ").Replace(t.Data)
			}
		}
	}
}

// sortStable is a tiny insertion sort; the slices here are short and it keeps
// ties in document order, matching the Rust sort_by_key.
func sortStable[T any](s []T, less func(a, b T) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ─── Table flattening ───────────────────────────────────────────────────────

func flattenTables(root *html.Node) {
	type entry struct {
		depth int
		node  *html.Node
	}
	var tables []entry
	for _, n := range collectAll(root, func(n *html.Node) bool { return isElement(n, "table") }) {
		tables = append(tables, entry{depth(n), n})
	}
	// Deepest first, so an outer table's cells already contain the paragraph
	// rewrites of any inner table before it is examined.
	sortStable(tables, func(a, b entry) bool { return a.depth > b.depth })

	for _, e := range tables {
		table := e.node
		if table.Parent == nil {
			continue // already swallowed by an outer rewrite
		}
		if strings.TrimSpace(subtreeText(table)) == "" {
			detach(table)
			continue
		}
		if isDataTable(table) {
			continue
		}
		flattenOneTable(table)
	}
}

// isDataTable defaults to "layout" — most marketing HTML uses <table> purely
// for columns — and only returns true on positive evidence: <thead>/<caption>,
// or uniform ≥2-cell rows with a real border. role=presentation/none always
// wins as layout, and a nested table strongly implies layout.
func isDataTable(t *html.Node) bool {
	if role := strings.ToLower(strings.TrimSpace(attr(t, "role"))); role == "presentation" || role == "none" {
		return false
	}
	// Bare <th> is not evidence: Steam, Mailchimp et al. use <th class="column-…">
	// for layout, as siblings of <td> in the same row.
	if hasOwnDescendant(t, "thead") || hasOwnDescendant(t, "caption") {
		return true
	}
	if len(descendants(t, func(n *html.Node) bool { return isElement(n, "table") })) > 0 {
		return false
	}

	rows := collectRows(t)
	if len(rows) < 2 {
		return false
	}
	maxC, minC := 0, 1<<31-1
	for _, tr := range rows {
		c := countCells(tr)
		if c > maxC {
			maxC = c
		}
		if c < minC {
			minC = c
		}
	}
	if maxC < 2 {
		return false
	}

	border := attr(t, "border")
	hasBorder := border != ""
	if n, err := strconv.Atoi(border); err == nil {
		hasBorder = n > 0
	}
	return minC == maxC && hasBorder
}

// hasOwnDescendant matches descendants whose nearest <table> ancestor is root.
// Without this, a layout wrapper sees <thead>/<caption> from a nested data table
// and stays unflattened, emitting all its content as one unstructured blob.
func hasOwnDescendant(root *html.Node, tag string) bool {
	for _, n := range descendants(root, func(n *html.Node) bool { return isElement(n, tag) }) {
		if nearestTableAncestor(n) == root {
			return true
		}
	}
	return false
}

// collectRows returns only the <tr>s whose nearest <table> ancestor is t, so an
// outer layout table can't sweep in and destroy a nested data table's rows.
func collectRows(t *html.Node) []*html.Node {
	var out []*html.Node
	for _, tr := range descendants(t, func(n *html.Node) bool { return isElement(n, "tr") }) {
		if nearestTableAncestor(tr) == t {
			out = append(out, tr)
		}
	}
	return out
}

func nearestTableAncestor(n *html.Node) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if isElement(p, "table") {
			return p
		}
	}
	return nil
}

func countCells(tr *html.Node) int {
	n := 0
	for c := tr.FirstChild; c != nil; c = c.NextSibling {
		if isElement(c, "td") || isElement(c, "th") {
			n++
		}
	}
	return n
}

type cellMode int

const (
	cellInline cellMode = iota
	cellBlocks
	// cellParagraph: the cell's inline content spans multiple <br> lines; emit
	// it as one standalone multi-line paragraph rather than merging it.
	cellParagraph
)

func flattenOneTable(table *html.Node) {
	var emitted []*html.Node

	for _, tr := range collectRows(table) {
		var cells []*html.Node
		for c := tr.FirstChild; c != nil; c = c.NextSibling {
			if isElement(c, "td") || isElement(c, "th") {
				cells = append(cells, c)
			}
		}
		if len(cells) == 0 {
			continue
		}

		// Inline runs accumulate into one paragraph spanning the row, joined by
		// single spaces. Block kids emit as standalone siblings so their
		// structure survives — a Bitbucket PR row of three <p>s stays three
		// paragraphs while [feature][→][develop] lozenges collapse to one line.
		var rowP *html.Node
		flushRowP := func() {
			if rowP != nil && strings.TrimSpace(subtreeText(rowP)) != "" {
				emitted = append(emitted, rowP)
			}
			rowP = nil
		}
		for _, cell := range cells {
			kids := childNodes(cell)
			var nonBlank []*html.Node
			for _, k := range kids {
				if !isBlank(k) {
					nonBlank = append(nonBlank, k)
				}
			}
			if len(nonBlank) == 0 {
				continue
			}

			mode, items := classifyCell(nonBlank)
			switch mode {
			case cellInline:
				if rowP == nil {
					rowP = newElement("p")
				}
				if !endsWithWhitespace(rowP) && rowP.FirstChild != nil {
					rowP.AppendChild(newText(" "))
				}
				// Preserve inter-element whitespace: marketing legends emit
				// `<span></span>X<span> (n)</span>\n<span></span>Y` where the
				// whitespace text is all that keeps `X (n)` off `Y`.
				for _, n := range includeInlineWhitespace(kids, items) {
					appendChild(rowP, n)
				}
			case cellBlocks:
				flushRowP()
				for _, b := range items {
					detach(b)
					emitted = append(emitted, b)
				}
			case cellParagraph:
				flushRowP()
				p := newElement("p")
				for _, n := range items {
					appendChild(p, n)
				}
				emitted = append(emitted, p)
			}
		}
		flushRowP()
	}

	// If every flattened row is a short single-line paragraph this is a
	// key-value / price-summary table (`Subtotal | 1.780,00 kr`). Join the rows
	// with <br> so they render compactly instead of as blank-line-separated
	// one-liners. Any long or block-bearing row leaves the table untouched, so
	// marketing layout tables are never crammed.
	if len(emitted) >= 2 && allNodes(emitted, isShortInlineParagraph) {
		group := newElement("p")
		for i, n := range emitted {
			if i > 0 {
				group.AppendChild(newElement("br"))
			}
			for _, child := range childNodes(n) {
				appendChild(group, child)
			}
		}
		emitted = []*html.Node{group}
	}

	for _, n := range emitted {
		insertBefore(table, n)
	}
	detach(table)
}

func allNodes(ns []*html.Node, pred func(*html.Node) bool) bool {
	for _, n := range ns {
		if !pred(n) {
			return false
		}
	}
	return true
}

// isShortInlineParagraph reports a <p> holding a short single line of inline
// content: one row of a key-value / spec table.
func isShortInlineParagraph(n *html.Node) bool {
	if !isElement(n, "p") || subtreeHasBlock(n) || subtreeHasBr(n) {
		return false
	}
	l := len([]rune(strings.TrimSpace(subtreeText(n))))
	return l > 0 && l <= 60
}

// includeInlineWhitespace re-threads whitespace-only text nodes from kids into
// items whenever they sit between two retained nodes. When items came from a
// wrapper's grandchildren it is not a subsequence of kids; then items is used
// untouched.
func includeInlineWhitespace(kids, items []*html.Node) []*html.Node {
	if len(items) == 0 {
		return nil
	}
	// Subsequence check.
	next := 0
	for _, k := range kids {
		if next < len(items) && items[next] == k {
			next++
		}
	}
	if next != len(items) {
		return items
	}

	out := make([]*html.Node, 0, len(items))
	idx := 0
	started, lastWasItem := false, false
	for _, k := range kids {
		switch {
		case idx < len(items) && items[idx] == k:
			out = append(out, k)
			idx++
			started, lastWasItem = true, true
		case started && lastWasItem && isBlank(k):
			if idx < len(items) {
				out = append(out, k)
			}
			lastWasItem = false
		}
	}
	return out
}

func classifyCell(nonBlank []*html.Node) (cellMode, []*html.Node) {
	// A cell holding a <br> is multi-line (a benefit card, an address block, a
	// title+subtitle). Merging it inline with the next cell would splice that
	// cell onto this one's tail line; one block per child would scatter the
	// lines with blank gaps. Keep it as one tight multi-line paragraph.
	for _, n := range nonBlank {
		if subtreeHasBr(n) {
			return cellParagraph, nonBlank
		}
	}
	if allNodes(nonBlank, func(n *html.Node) bool { return !isBlockKid(n) }) {
		return cellInline, nonBlank
	}
	if len(nonBlank) == 1 {
		only := nonBlank[0]
		if isElement(only, "p") || isElement(only, "div") {
			grandkids := childNodes(only)
			var gkNonBlank []*html.Node
			for _, n := range grandkids {
				if !isBlank(n) {
					gkNonBlank = append(gkNonBlank, n)
				}
			}
			if allNodes(gkNonBlank, func(n *html.Node) bool { return !isBlockKid(n) }) {
				return cellInline, grandkids
			}
			return cellBlocks, gkNonBlank
		}
	}
	return cellBlocks, nonBlank
}

func subtreeHasBr(n *html.Node) bool {
	return len(collectAll(n, func(d *html.Node) bool { return isElement(d, "br") })) > 0
}

func isBlockKid(n *html.Node) bool {
	return isElementOneOf(n, "p", "div", "table", "ul", "ol", "li", "h1", "h2",
		"h3", "h4", "h5", "h6", "pre", "blockquote", "hr")
}

func endsWithWhitespace(n *html.Node) bool {
	last := n.LastChild
	if last == nil {
		return true // empty parent — no separator needed
	}
	if last.Type != html.TextNode {
		return false
	}
	rs := []rune(last.Data)
	if len(rs) == 0 {
		return true
	}
	return unicode.IsSpace(rs[len(rs)-1])
}

func isBlank(n *html.Node) bool {
	if n.Type == html.TextNode {
		return strings.TrimSpace(n.Data) == ""
	}
	return n.Type == html.CommentNode
}

// ─── Markdown output helpers ────────────────────────────────────────────────

// uniSpace matches what Rust's regex crate means by `\s`: the Unicode
// White_Space property. Go's RE2 `\s` is ASCII-only, so a U+2028 between two
// words would stay glued inside a wrap token here while splitting upstream.
const uniSpace = `[\s\x{0085}\p{Z}]`

var (
	structuralRe = regexp.MustCompile(`^(#{1,6}` + uniSpace + `|[-*+]` + uniSpace + `|\d+\.` + uniSpace + `|>` + uniSpace + `|[-*_]{3,}` + uniSpace + `*$)`)
	refLinkRe    = regexp.MustCompile(`^\[[^\]]+\]:` + uniSpace)
	blankRunsRe  = regexp.MustCompile(`\n{3,}`)
	listMarkerRe = regexp.MustCompile(`^(?:[-*+]` + uniSpace + `+|\d+\.` + uniSpace + `+)`)
	// A wrap token is any whitespace-separated chunk, except that markdown
	// links and inline code embed spaces that must not split it. `\\.` consumes
	// escaped chars so JSON-shaped payloads with \[ and \] don't close early.
	tokenRe       = regexp.MustCompile("(?:!?\\[(?:\\\\.|[^\\]])*\\]\\((?:\\\\.|[^)])*\\)|`[^`]*`|[^" + `\s\x{0085}\p{Z}` + "])+")
	linkStripRe   = regexp.MustCompile(`\[([^\]\n]+)\]\([^)\n]*\)`)
	linkOnlyRe    = regexp.MustCompile(`^\[[^\]\n]+\]\([^)\n]*\)$`)
	ieConditional = regexp.MustCompile(`(?si)<!\[if[^\]]*\]>.*?<!\[endif\]>`)
)

func collapseBlankRuns(md string) string { return blankRunsRe.ReplaceAllString(md, "\n\n") }

// visibleWidth is the number of characters the reader sees once `[text](url)`
// renders as bare `text`.
func visibleWidth(s string) int {
	return len([]rune(linkStripRe.ReplaceAllString(strings.TrimSpace(s), "$1")))
}

func stripIEConditionals(h string) string { return ieConditional.ReplaceAllString(h, "") }

// lines splits like Rust's str::lines: on \n, dropping a trailing \r.
func lines(s string) []string {
	out := strings.Split(s, "\n")
	for i, l := range out {
		out[i] = strings.TrimSuffix(l, "\r")
	}
	if n := len(out); n > 0 && out[n-1] == "" {
		out = out[:n-1]
	}
	return out
}

// wrapLines hard-wraps paragraphs at width columns on word boundaries. Links and
// inline code stay intact even when a single token exceeds the limit. Fenced and
// indented code, headings, tables and reference definitions are left alone;
// blockquote and list continuation lines keep their prefix.
func wrapLines(md string, width int) string {
	var out strings.Builder
	out.Grow(len(md))
	inFence := false
	for i, line := range strings.Split(md, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			out.WriteString(line)
			continue
		}
		if inFence || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") ||
			strings.TrimSpace(line) == "" {
			out.WriteString(line)
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		skip := strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "|") ||
			refLinkRe.MatchString(trimmed)
		if !skip {
			if m := structuralRe.FindString(trimmed); m != "" {
				skip = strings.ContainsAny(m, "_*-") &&
					allRunes(trimmed, func(r rune) bool { return r == '-' || r == '*' || r == '_' || r == ' ' })
			}
		}
		if skip {
			out.WriteString(line)
			continue
		}

		leading := line[:len(line)-len(strings.TrimLeft(line, " "))]
		afterIndent := line[len(leading):]

		quoteEnd := 0
		for idx, ch := range afterIndent {
			if ch == '>' || ch == ' ' {
				quoteEnd = idx + len(string(ch))
			} else {
				break
			}
		}
		quotePrefix := ""
		if strings.Contains(afterIndent[:quoteEnd], ">") {
			quotePrefix = afterIndent[:quoteEnd]
		}
		body := afterIndent[len(quotePrefix):]

		listMarker, content := "", body
		if m := listMarkerRe.FindString(body); m != "" {
			listMarker, content = m, body[len(m):]
		}
		firstPrefix := leading + quotePrefix + listMarker
		contPrefix := leading + quotePrefix + strings.Repeat(" ", len([]rune(listMarker)))

		tokens := tokenRe.FindAllString(content, -1)
		if len(tokens) == 0 {
			out.WriteString(line)
			continue
		}

		out.WriteString(firstPrefix)
		col := len([]rune(firstPrefix))
		atLineStart := true
		for _, tok := range tokens {
			// Wrap on visible width: a pager renders `[text](url)` as `text`,
			// so a tracking URL's raw length is not what the reader pays.
			tlen := visibleWidth(tok)
			wouldOverflow := col+1+tlen > width
			// Suppress the wrap only when it cannot help — the token is itself
			// wider than the budget, so a fresh line gains nothing. A token that
			// does fit always gets to wrap, even when the current line is
			// already over budget, so following words flow normally instead of
			// orphaning one per line.
			uselessWrap := tlen > width
			if !atLineStart && wouldOverflow && !uselessWrap {
				out.WriteByte('\n')
				out.WriteString(contPrefix)
				col = len([]rune(contPrefix))
				atLineStart = true
			}
			if !atLineStart {
				out.WriteByte(' ')
				col++
			}
			out.WriteString(tok)
			col += tlen
			atLineStart = false
		}
	}
	return out.String()
}

// ─── Markdown serialiser ────────────────────────────────────────────────────

// toMarkdown walks the cleaned DOM and emits Markdown. Heading levels are
// normalised so the shallowest heading becomes `#`.
func toMarkdown(root *html.Node, width int) string {
	shift := minHeadingLevel(root) - 1
	if shift < 0 {
		shift = 0
	}
	blocks := joinLinkRows(compressHeadingLevels(dropEmptySections(docBlocks(root, shift))), width)
	return wrapLines(collapseBlankRuns(strings.Join(blocks, "\n\n")), width)
}

// joinLinkRows joins a run of consecutive single-short-link blocks into one
// ` · `-separated line. Vendor emails stack nav bars and footers vertically, one
// block per link, rendering as a tall column. The short-text cap keeps
// article-title link lists on their own lines, and a run is only merged when the
// joined line fits, so it never wraps and dangles a ` ·`.
func joinLinkRows(blocks []string, width int) []string {
	isShortLink := func(b string) bool {
		t := strings.TrimSpace(b)
		return linkOnlyRe.MatchString(t) && visibleWidth(t) <= 30
	}
	var out, run []string
	flush := func() {
		joined := strings.Join(run, " · ")
		if len(run) >= 2 && visibleWidth(joined) <= width {
			out = append(out, joined)
		} else {
			out = append(out, run...)
		}
		run = nil
	}
	for _, b := range blocks {
		if isShortLink(b) {
			run = append(run, b)
		} else {
			flush()
			out = append(out, b)
		}
	}
	flush()
	return out
}

// compressHeadingLevels remaps the distinct levels present to a contiguous 1..n
// range. Emails routinely jump levels — a promo marked <h1> with the real
// sections as <h4> — which a uniform shift preserves as `#` → `####`.
func compressHeadingLevels(blocks []string) []string {
	var used []int
	seen := map[int]bool{}
	for _, b := range blocks {
		if lvl, ok := blockHeadingLevel(b); ok && !seen[lvl] {
			seen[lvl] = true
			used = append(used, lvl)
		}
	}
	if len(used) < 2 {
		return blocks
	}
	sortStable(used, func(a, b int) bool { return a < b })
	rank := map[int]int{}
	for i, l := range used {
		rank[l] = i + 1
	}
	out := make([]string, len(blocks))
	for i, b := range blocks {
		if lvl, ok := blockHeadingLevel(b); ok {
			hashes := len(b) - len(strings.TrimLeft(b, "#"))
			out[i] = strings.Repeat("#", rank[lvl]) + b[hashes:]
		} else {
			out[i] = b
		}
	}
	return out
}

// blockHeadingLevel returns the leading-# level of an ATX heading block.
func blockHeadingLevel(block string) (int, bool) {
	h := strings.TrimLeft(block, " \t")
	hashes := len(h) - len(strings.TrimLeft(h, "#"))
	if hashes >= 1 && hashes <= 6 && strings.HasPrefix(h[hashes:], " ") {
		return hashes, true
	}
	return 0, false
}

// dropEmptySections drops headings that introduce nothing: a heading whose next
// block is another heading at the same or shallower level.
func dropEmptySections(blocks []string) []string {
	levels := make([][2]int, len(blocks)) // {level, isHeading}
	for i, b := range blocks {
		if lvl, ok := blockHeadingLevel(b); ok {
			levels[i] = [2]int{lvl, 1}
		}
	}
	var out []string
	for i, b := range blocks {
		if levels[i][1] == 1 && i+1 < len(blocks) && levels[i+1][1] == 1 && levels[i+1][0] <= levels[i][0] {
			continue
		}
		out = append(out, b)
	}
	return out
}

// minHeadingLevel finds the shallowest heading level with non-empty text.
func minHeadingLevel(root *html.Node) int {
	shallowest := 7
	walk(root, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		lvl := headingLevelOf(n.Data)
		if lvl == 0 || strings.TrimSpace(subtreeText(n)) == "" {
			return
		}
		if lvl < shallowest {
			shallowest = lvl
		}
	})
	return shallowest
}

// headingLevelOf returns 1..6, or 0 when the tag is not a heading.
func headingLevelOf(tag string) int {
	switch tag {
	case "h1":
		return 1
	case "h2":
		return 2
	case "h3":
		return 3
	case "h4":
		return 4
	case "h5":
		return 5
	case "h6":
		return 6
	}
	return 0
}

func docBlocks(root *html.Node, shift int) []string {
	start := root
	for _, n := range collectAll(root, func(n *html.Node) bool { return isElement(n, "body") }) {
		start = n
		break
	}
	return nodeBlocks(start, shift)
}

// nodeBlocks groups consecutive inline children into one paragraph and emits
// block children on their own. Without the grouping, a container mixing inline
// content with a block child scatters each text run and <strong> into separate
// blocks instead of keeping the sentence together.
func nodeBlocks(node *html.Node, shift int) []string {
	var out []string
	var inlineRun []*html.Node
	flush := func() {
		if len(inlineRun) == 0 {
			return
		}
		var b strings.Builder
		for _, n := range inlineRun {
			b.WriteString(nodeInline(n))
		}
		if s := tidyInlineBlock(b.String()); s != "" {
			out = append(out, s)
		}
		inlineRun = nil
	}
	for _, c := range childNodes(node) {
		isBlock := (c.Type == html.ElementNode && isBlockName(c.Data)) || subtreeHasBlock(c)
		if isBlock {
			flush()
			out = append(out, childToBlocks(c, shift)...)
		} else {
			inlineRun = append(inlineRun, c)
		}
	}
	flush()
	return out
}

func hasBlockChild(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && isBlockName(c.Data) {
			return true
		}
	}
	return false
}

func isBlockName(name string) bool {
	switch name {
	case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "hr", "pre",
		"blockquote", "table", "header", "footer", "section", "article", "main",
		"aside", "nav", "center":
		return true
	}
	return false
}

func subtreeHasBlock(n *html.Node) bool {
	return len(descendants(n, func(c *html.Node) bool {
		return c.Type == html.ElementNode && isBlockName(c.Data)
	})) > 0
}

func childToBlocks(node *html.Node, shift int) []string {
	if node.Type == html.TextNode {
		if trimmed := strings.TrimSpace(node.Data); trimmed != "" {
			return []string{escapeText(trimmed)}
		}
		return nil
	}
	if node.Type != html.ElementNode {
		return nil
	}

	switch node.Data {
	case "html", "body":
		return nodeBlocks(node, shift)

	// Structural containers (and <p>): recurse when block children are present,
	// otherwise emit one paragraph.
	case "p", "div", "center", "header", "footer", "section", "article", "main",
		"aside", "nav", "form", "fieldset":
		if hasBlockChild(node) {
			return nodeBlocks(node, shift)
		}
		if s := tidyInlineBlock(childrenInline(node)); s != "" {
			return []string{s}
		}
		return nil

	case "h1", "h2", "h3", "h4", "h5", "h6":
		lvl := headingLevelOf(node.Data) - shift
		lvl = clamp(lvl, 1, 6)
		// A heading is a single line; fold any <br>-newline to a space.
		s := strings.TrimSpace(strings.ReplaceAll(childrenInline(node), "\n", " "))
		if s == "" {
			return nil
		}
		return []string{strings.Repeat("#", lvl) + " " + s}

	case "hr":
		return []string{"---"}

	case "pre":
		// Strip per-line trailing whitespace: source <pre> often pads lines to a
		// fixed column, which renders as ragged trailing space. Indentation stays.
		var ls []string
		for _, l := range lines(subtreeText(node)) {
			ls = append(ls, strings.TrimRight(l, " \t\v\f\r"))
		}
		body := strings.Trim(strings.Join(ls, "\n"), "\n")
		if body == "" {
			return nil
		}
		return []string{"```\n" + body + "\n```"}

	case "blockquote":
		inner := nodeBlocks(node, shift)
		if len(inner) == 0 {
			return nil
		}
		var quoted []string
		for _, l := range strings.Split(strings.Join(inner, "\n\n"), "\n") {
			if l == "" {
				quoted = append(quoted, ">")
			} else {
				quoted = append(quoted, "> "+l)
			}
		}
		return []string{strings.Join(quoted, "\n")}

	case "ul":
		if s := serializeList(node, false, 0); s != "" {
			return []string{s}
		}
		return nil
	case "ol":
		if s := serializeList(node, true, 0); s != "" {
			return []string{s}
		}
		return nil

	case "table":
		if s := serializeTable(node); s != "" {
			return []string{s}
		}
		return nil

	default:
		// Unknown/inline element at block level: recurse when a descendant is a
		// block (handles microdata spans wrapping a whole email layout),
		// otherwise treat it as an inline paragraph.
		if subtreeHasBlock(node) {
			return nodeBlocks(node, shift)
		}
		if s := tidyInlineBlock(nodeInline(node)); s != "" {
			return []string{s}
		}
		return nil
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ─── Inline serialisation ───────────────────────────────────────────────────

func nodeInline(node *html.Node) string {
	if node.Type == html.TextNode {
		return escapeText(node.Data)
	}
	if node.Type != html.ElementNode {
		return ""
	}
	switch node.Data {
	case "a":
		// A link is a single line; fold any <br>-newline in the text to a space
		// so `[multi\nline](url)` doesn't break the link syntax.
		inner := strings.Join(strings.Fields(strings.ReplaceAll(childrenInline(node), "\n", " ")), " ")
		if inner == "" || isDecorativeGlyph(inner) {
			return ""
		}
		href := attr(node, "href")
		if href == "" {
			return inner
		}
		// Garbage href: broken templates stuff text or markup into the
		// attribute. A real URL has no whitespace or angle brackets — keep the
		// visible text rather than emit a broken link.
		if strings.ContainsAny(href, " \t\n\r<>") || strings.IndexFunc(href, unicode.IsSpace) >= 0 {
			return inner
		}
		// [url](url) → bare url
		if strings.TrimRight(inner, "/") == strings.TrimRight(href, "/") {
			return inner
		}
		// If the text already contains link syntax, fall back to plain text to
		// avoid nested [[…](url)](url), which breaks parsers.
		display := inner
		if strings.Contains(inner, "](") {
			display = strings.Join(strings.Fields(subtreeText(node)), " ")
		}
		if display == "" {
			return ""
		}
		return fmt.Sprintf("[%s](%s)", display, href)

	case "strong", "b":
		return emphasis(node, "**")
	case "em", "i":
		return emphasis(node, "*")

	case "code":
		s := strings.TrimSpace(subtreeText(node))
		if s == "" {
			return ""
		}
		return "`" + s + "`"

	// <br> is an intentional line break — emit a real newline so signatures,
	// address blocks and log dumps stay tight instead of reflowing onto one
	// line or exploding into blank-line-separated paragraphs. Contexts where a
	// raw newline is harmful sanitise it: emphasis and links split or fold it,
	// table cells, headings and list items flatten it.
	case "br":
		return "\n"
	}
	return childrenInline(node)
}

// tidyInlineBlock trims every line of a multi-line inline block so leading
// spaces from source text nodes don't survive as ragged indentation, while
// keeping blank lines (from <br><br>). A block that reduces to a single ASCII
// punctuation char is dropped: separator residue left between removed <img>s.
func tidyInlineBlock(s string) string {
	var out string
	if !strings.Contains(s, "\n") {
		out = strings.TrimSpace(s)
	} else {
		ls := strings.Split(s, "\n")
		for i, l := range ls {
			ls[i] = strings.TrimSpace(l)
		}
		out = strings.TrimSpace(strings.Join(ls, "\n"))
	}
	if len(out) == 1 && isASCIIPunct(out[0]) {
		return ""
	}
	return out
}

func isASCIIPunct(b byte) bool {
	return (b >= '!' && b <= '/') || (b >= ':' && b <= '@') ||
		(b >= '[' && b <= '`') || (b >= '{' && b <= '~')
}

func childrenInline(node *html.Node) string {
	var b strings.Builder
	for c := node.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(nodeInline(c))
	}
	s := b.String()
	// Adjacent same-marker emphasis with no separating whitespace — e.g.
	// `<b>So, w</b><b>atch this</b>` splitting a word — concatenates to
	// `**So, w****atch this**`. Escaped literal asterisks are `\*`, so a bare
	// `****` only ever comes from marker adjacency; dropping it merges the runs.
	if strings.Contains(s, "****") {
		return strings.ReplaceAll(s, "****", "")
	}
	return s
}

// emphasis wraps inline content in a marker, keeping leading/trailing
// whitespace outside it. HTML often splits a phrase across adjacent <b> runs
// where the only separating space sits at a marker boundary; trimming it would
// fuse the words.
func emphasis(node *html.Node, marker string) string {
	inner := childrenInline(node)
	// A <br> inside the emphasis leaves a newline in inner; wrap each line
	// separately so the markers never span a line break, which would render the
	// literal `**`.
	if strings.Contains(inner, "\n") {
		parts := strings.Split(inner, "\n")
		for i, line := range parts {
			s := strings.TrimSpace(line)
			if s == "" {
				parts[i] = ""
			} else {
				parts[i] = marker + s + marker
			}
		}
		return strings.Join(parts, "\n")
	}
	s := strings.TrimSpace(inner)
	if s == "" {
		return ""
	}
	lead, trail := "", ""
	if rs := []rune(inner); len(rs) > 0 {
		if unicode.IsSpace(rs[0]) {
			lead = " "
		}
		if unicode.IsSpace(rs[len(rs)-1]) {
			trail = " "
		}
	}
	return lead + marker + s + marker + trail
}

// decodeUnicodeEscapes turns literal escape sequences that broken sender
// templates emit as visible text back into characters: \uXXXX (with UTF-16
// surrogate pairing), \u{XXXX} and \UXXXXXXXX. No human types these into body
// copy, so a stray `–` is always a templating bug. Anything that isn't a
// well-formed escape is left verbatim.
func decodeUnicodeEscapes(s string) string {
	if !strings.Contains(s, `\u`) && !strings.Contains(s, `\U`) {
		return s
	}
	c := []rune(s)
	var out strings.Builder
	out.Grow(len(s))
	hex := func(sl []rune) (uint32, bool) {
		for _, h := range sl {
			if !isHexDigit(h) {
				return 0, false
			}
		}
		v, err := strconv.ParseUint(string(sl), 16, 32)
		if err != nil {
			return 0, false
		}
		return uint32(v), true
	}
	for i := 0; i < len(c); {
		if c[i] == '\\' && i+1 < len(c) && (c[i+1] == 'u' || c[i+1] == 'U') {
			// \u{...} brace form.
			if c[i+1] == 'u' && i+2 < len(c) && c[i+2] == '{' {
				if end := indexRune(c[i+3:], '}'); end >= 0 {
					if cp, ok := hex(c[i+3 : i+3+end]); ok && validRune(cp) {
						out.WriteRune(rune(cp))
						i += 3 + end + 1
						continue
					}
				}
			}
			width := 4
			if c[i+1] == 'U' {
				width = 8
			}
			if i+2+width <= len(c) {
				if cp, ok := hex(c[i+2 : i+2+width]); ok {
					// A \uXXXX high surrogate followed by a low surrogate pairs up.
					if width == 4 && cp >= 0xD800 && cp <= 0xDBFF {
						j := i + 6
						if j+6 <= len(c) && c[j] == '\\' && c[j+1] == 'u' {
							if lo, ok := hex(c[j+2 : j+6]); ok && lo >= 0xDC00 && lo <= 0xDFFF {
								full := 0x10000 + ((cp - 0xD800) << 10) + (lo - 0xDC00)
								if validRune(full) {
									out.WriteRune(rune(full))
									i = j + 6
									continue
								}
							}
						}
					} else if validRune(cp) {
						out.WriteRune(rune(cp))
						i += 2 + width
						continue
					}
				}
			}
		}
		out.WriteRune(c[i])
		i++
	}
	return out.String()
}

// validRune mirrors Rust's char::from_u32: surrogates and out-of-range fail.
func validRune(cp uint32) bool {
	return cp <= 0x10FFFF && (cp < 0xD800 || cp > 0xDFFF)
}

func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func indexRune(s []rune, target rune) int {
	for i, r := range s {
		if r == target {
			return i
		}
	}
	return -1
}

// escapeText escapes characters that would create unintended Markdown syntax,
// and collapses whitespace runs to a single space, matching how a browser
// collapses whitespace.
func escapeText(s string) string {
	s = decodeUnicodeEscapes(s)
	var out strings.Builder
	out.Grow(len(s) + 4)
	prevSpace := false
	for _, c := range s {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\u00A0' {
			if !prevSpace {
				out.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		switch c {
		case '\\', '`', '*', '_', '[', ']':
			out.WriteByte('\\')
			out.WriteRune(c)
		default:
			out.WriteRune(c)
		}
	}
	return out.String()
}

// ─── List serialisation ─────────────────────────────────────────────────────

func serializeList(list *html.Node, ordered bool, depth int) string {
	indent := strings.Repeat("  ", depth)
	var items []string
	n := 1

	for _, child := range childNodes(list) {
		if !isElement(child, "li") {
			continue
		}
		marker := "- "
		if ordered {
			marker = strconv.Itoa(n) + ". "
		}
		n++

		var inlineParts, subLists []string
		for _, kid := range childNodes(child) {
			switch {
			case isElement(kid, "ul"):
				subLists = append(subLists, serializeList(kid, false, depth+1))
			case isElement(kid, "ol"):
				subLists = append(subLists, serializeList(kid, true, depth+1))
			case isBlockKid(kid):
				// <p>/<div> inside li: gather as inline text.
				if s := strings.TrimSpace(childrenInline(kid)); s != "" {
					inlineParts = append(inlineParts, s)
				}
			default:
				inlineParts = append(inlineParts, nodeInline(kid))
			}
		}

		// A list item is one logical line; fold any <br>-newline to a space so a
		// continuation can't escape the bullet's indentation.
		itemText := strings.Join(strings.Fields(strings.Join(inlineParts, "")), " ")
		if itemText == "" && len(subLists) == 0 {
			continue
		}
		itemLines := []string{indent + marker + itemText}
		for _, sub := range subLists {
			// The sub-list already carries its own depth-based indent.
			itemLines = append(itemLines, strings.Split(sub, "\n")...)
		}
		items = append(items, strings.Join(itemLines, "\n"))
	}
	return strings.Join(items, "\n")
}

// ─── Table serialisation ────────────────────────────────────────────────────

func serializeTable(table *html.Node) string {
	rows := collectRows(table)
	if len(rows) < 2 {
		return ""
	}

	parsed := make([][]string, 0, len(rows))
	for _, tr := range rows {
		var row []string
		for c := tr.FirstChild; c != nil; c = c.NextSibling {
			if !isElement(c, "td") && !isElement(c, "th") {
				continue
			}
			// A <br>-newline inside a cell would break the row; collapse every
			// whitespace run. Links never contain spaces, so this can't split one.
			row = append(row, strings.Join(strings.Fields(childrenInline(c)), " "))
		}
		parsed = append(parsed, row)
	}

	ncols := 0
	for _, r := range parsed {
		if len(r) > ncols {
			ncols = len(r)
		}
	}
	if ncols == 0 {
		return ""
	}

	// Drop columns where every cell is empty.
	var keep []int
	for c := 0; c < ncols; c++ {
		for _, row := range parsed {
			if c < len(row) && strings.TrimSpace(row[c]) != "" {
				keep = append(keep, c)
				break
			}
		}
	}
	if len(keep) < ncols && len(keep) > 0 {
		for i, row := range parsed {
			nr := make([]string, 0, len(keep))
			for _, c := range keep {
				if c < len(row) {
					nr = append(nr, row[c])
				} else {
					nr = append(nr, "")
				}
			}
			parsed[i] = nr
		}
	}
	ncols = 0
	if len(parsed) > 0 {
		ncols = len(parsed[0])
	}
	if ncols == 0 {
		return ""
	}

	widths := make([]int, ncols)
	for i := range widths {
		widths[i] = 3
	}
	for _, row := range parsed {
		for c, cell := range row {
			if c < ncols {
				if w := visibleWidth(cell); w > widths[c] {
					widths[c] = w
				}
			}
		}
	}

	// Row 0 is always the header; the separator follows.
	var out []string
	for i, row := range parsed {
		var line strings.Builder
		line.WriteByte('|')
		for c := 0; c < ncols; c++ {
			cell := ""
			if c < len(row) {
				cell = row[c]
			}
			pad := widths[c] - visibleWidth(cell)
			line.WriteByte(' ')
			line.WriteString(cell)
			if pad > 0 {
				line.WriteString(strings.Repeat(" ", pad))
			}
			line.WriteString(" |")
		}
		out = append(out, line.String())
		if i == 0 {
			var sep strings.Builder
			sep.WriteByte('|')
			for _, w := range widths {
				sep.WriteByte(' ')
				sep.WriteString(strings.Repeat("-", w))
				sep.WriteString(" |")
			}
			out = append(out, sep.String())
		}
	}
	return strings.Join(out, "\n")
}
