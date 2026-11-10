package main

// Markdown (as Claude writes it) to Telegram HTML, and splitting long HTML into
// messages without breaking tags.
//
// Telegram's HTML subset: <b> <i> <s> <u> <code> <pre> <a href> <blockquote>.
// Everything else is text, so every piece of input is escaped first and only
// the converter's own tags are added. Headings become bold, lists stay text
// with a bullet, tables become <pre>, fenced code <pre><code class="language-x">.

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const tgMaxText = 4000 // characters per message, under Telegram's 4096

var (
	reFence    = regexp.MustCompile("^\\s*(```+|~~~+)\\s*([\\w+#.-]*)")
	reHeading  = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	reBullet   = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	reNumbered = regexp.MustCompile(`^(\s*)(\d{1,3})[.)]\s+(.*)$`)
	reQuote    = regexp.MustCompile(`^\s*>\s?(.*)$`)
	reRule     = regexp.MustCompile(`^\s*(?:-\s*){3,}$|^\s*(?:\*\s*){3,}$|^\s*(?:_\s*){3,}$`)
	reTableRow = regexp.MustCompile(`^\s*\|.*\|\s*$`)

	reLinkAt = regexp.MustCompile(`^\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
)

// mdInline converts one line of inline Markdown to Telegram HTML. It builds a
// list of pieces (escaped text, finished code spans and links, delimiters)
// and matches emphasis delimiters on a stack: a tag is written only when its
// closer is found, and openers left open inside it turn back into text, so the
// output is always properly nested. No placeholder strings are used, so no
// input can be mistaken for one.
func mdInline(s string) string { return mdInlineX(s, false) }

// File references in plain text ("internal/auth.go:42", "main.go") become <code>.
var reFileRef = regexp.MustCompile(`(?:[\w.-]+/)*[\w-][\w.-]*\.(?:go|py|js|mjs|ts|tsx|jsx|rs|java|kt|swift|rb|c|h|cc|cpp|hpp|cs|md|json|yaml|yml|toml|sh|sql|html|css|scss|vue|txt|mod|sum|lock|env|cfg|ini|xml|proto)(?::\d+(?::\d+)?)?`)

// fileRefSpans: where file references are in plain text; only whole words
// count (not a piece of a URL or of a longer name).
func fileRefSpans(t string) [][]int {
	var out [][]int
	for _, m := range reFileRef.FindAllStringIndex(t, -1) {
		before, after := rune(0), rune(0)
		if m[0] > 0 {
			before, _ = utf8.DecodeLastRuneInString(t[:m[0]])
		}
		if m[1] < len(t) {
			after, _ = utf8.DecodeRuneInString(t[m[1]:])
		}
		okBefore := before == 0 || unicode.IsSpace(before) || strings.ContainsRune("(\"'«[", before)
		okAfter := after == 0 || unicode.IsSpace(after) || strings.ContainsRune(".,;:)!?\"'»]", after)
		if okBefore && okAfter {
			out = append(out, m)
		}
	}
	return out
}

// mdInlineX: restricted is for text inside a blockquote or a link, where
// Telegram allows only bold, italic, underline and strikethrough: code spans
// become plain text and links "text (url)".
func mdInlineX(s string, restricted bool) string {
	type piece struct {
		html  string
		delim string // an unmatched delimiter (its literal text is html)
		plain string // for <code>: the escaped text it falls back to
	}
	code := func(raw string) piece {
		e := html.EscapeString(raw)
		return piece{html: "<code>" + e + "</code>", plain: e}
	}
	var out []piece
	type opener struct {
		kind string
		at   int
	}
	var stack []opener
	tags := map[string]string{"*": "i", "_": "i", "**": "b", "__": "b", "~~": "s"}
	r := []rune(s)
	isWord := func(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) }
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			t := text.String()
			text.Reset()
			if restricted {
				out = append(out, piece{html: html.EscapeString(t)})
				return
			}
			last := 0
			for _, m := range fileRefSpans(t) {
				if m[0] > last {
					out = append(out, piece{html: html.EscapeString(t[last:m[0]])})
				}
				out = append(out, code(t[m[0]:m[1]]))
				last = m[1]
			}
			if last < len(t) {
				out = append(out, piece{html: html.EscapeString(t[last:])})
			}
		}
	}
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case c == '`':
			j := i + 1
			for j < len(r) && r[j] != '`' && r[j] != '\n' {
				j++
			}
			if j < len(r) && r[j] == '`' && j > i+1 {
				flush()
				if restricted {
					out = append(out, piece{html: html.EscapeString(string(r[i+1 : j]))})
				} else {
					out = append(out, code(string(r[i+1:j])))
				}
				i = j + 1
				continue
			}
		case c == '[':
			if m := reLinkAt.FindStringSubmatch(string(r[i:])); m != nil {
				flush()
				if restricted {
					out = append(out, piece{html: mdInlineX(m[1], true) + " (" + html.EscapeString(m[2]) + ")"})
				} else {
					out = append(out, piece{html: `<a href="` + html.EscapeString(m[2]) + `">` + mdInlineX(m[1], true) + "</a>"})
				}
				i += utf8.RuneCountInString(m[0])
				continue
			}
		case c == '*' || c == '_' || c == '~':
			j := i
			for j < len(r) && r[j] == c {
				j++
			}
			n := j - i
			var prev, next rune
			if i > 0 {
				prev = r[i-1]
			}
			if j < len(r) {
				next = r[j]
			}
			canOpen := next != 0 && !unicode.IsSpace(next) && !isWord(prev)
			canClose := prev != 0 && !unicode.IsSpace(prev) && !isWord(next)
			var openOrder []string
			switch {
			case c == '~' && n == 2:
				openOrder = []string{"~~"}
			case c != '~' && n == 1:
				openOrder = []string{string(c)}
			case c != '~' && n == 2:
				openOrder = []string{string(c) + string(c)}
			case c != '~' && n == 3:
				openOrder = []string{string(c) + string(c), string(c)}
			}
			if openOrder == nil || (!canOpen && !canClose) {
				text.WriteString(string(r[i:j]))
				i = j
				continue
			}
			flush()
			left := openOrder
			if canClose {
				var rest []string
				for k := len(openOrder) - 1; k >= 0; k-- { // closers in reverse
					kind := openOrder[k]
					top := -1
					for x := len(stack) - 1; x >= 0; x-- {
						if stack[x].kind == kind {
							top = x
							break
						}
					}
					if top < 0 {
						rest = append([]string{kind}, rest...)
						continue
					}
					// Openers above it never closed: they stay text. Telegram
					// does not allow <code> inside bold or italic: plain text.
					for k := stack[top].at + 1; k < len(out); k++ {
						if out[k].plain != "" {
							out[k].html, out[k].plain = out[k].plain, ""
						}
					}
					out[stack[top].at].html = "<" + tags[kind] + ">"
					out[stack[top].at].delim = ""
					stack = stack[:top]
					out = append(out, piece{html: "</" + tags[kind] + ">"})
				}
				left = rest
			}
			for _, kind := range left {
				if canOpen {
					stack = append(stack, opener{kind, len(out)})
					out = append(out, piece{html: html.EscapeString(kind), delim: kind})
				} else {
					out = append(out, piece{html: html.EscapeString(kind)})
				}
			}
			i = j
			continue
		}
		text.WriteRune(c)
		i++
	}
	flush()
	var b strings.Builder
	for _, p := range out {
		b.WriteString(p.html)
	}
	return b.String()
}

// mdBlocks converts Markdown to a list of HTML blocks; each block is
// self-contained (balanced tags), so messages can be cut between blocks.
func mdBlocks(md string) []string {
	lines := strings.Split(strings.ReplaceAll(stripControl(md), "\r\n", "\n"), "\n")
	var out, para []string
	flushPara := func() {
		if len(para) > 0 {
			out = append(out, strings.Join(para, "\n"))
			para = nil
		}
	}
	isList := func(l string) bool { return reBullet.MatchString(l) || reNumbered.MatchString(l) }
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if m := reFence.FindStringSubmatch(l); m != nil {
			flushPara()
			fence := m[1]
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fence[:3]); i++ {
				code = append(code, lines[i])
			}
			out = append(out, codeBlocks(m[2], code)...)
			continue
		}
		if reTableRow.MatchString(l) {
			flushPara()
			var rows []string
			for ; i < len(lines) && reTableRow.MatchString(lines[i]); i++ {
				rows = append(rows, strings.TrimSpace(lines[i]))
			}
			i--
			out = append(out, tableBlocks(rows)...)
			continue
		}
		if isList(l) && !reRule.MatchString(l) {
			flushPara()
			var items []string
			for ; i < len(lines) && isList(lines[i]) && !reRule.MatchString(lines[i]); i++ {
				items = append(items, lines[i])
			}
			i--
			out = append(out, listBlocks(items)...)
			continue
		}
		if reQuote.MatchString(l) {
			flushPara()
			var q []string
			for ; i < len(lines) && reQuote.MatchString(lines[i]); i++ {
				q = append(q, mdInlineX(reQuote.FindStringSubmatch(lines[i])[1], true))
			}
			i--
			out = append(out, "<blockquote>"+strings.Join(q, "\n")+"</blockquote>")
			continue
		}
		switch {
		case strings.TrimSpace(l) == "":
			flushPara()
		case reRule.MatchString(l):
			flushPara()
			out = append(out, "———")
		case reHeading.MatchString(l):
			flushPara()
			out = append(out, "<b>"+mdInlineX(reHeading.FindStringSubmatch(l)[1], true)+"</b>")
		default:
			para = append(para, mdInline(l))
		}
	}
	flushPara()
	return out
}

const (
	listVisible   = 5  // items shown of a long list
	listCollapse  = 15 // longer lists fold the rest
	codeCollapse  = 12 // longer code goes into an expandable quote
	tableMaxCols  = 34 // display columns a <pre> grid may take
	tableMaxCards = 20
	statusMaxLen  = 12
)

// listBlocks renders list lines: "•" at the top level, "◦" below, two
// spaces per level, numbers kept. A long list shows its first items and
// folds the rest into an expandable quote.
func listBlocks(items []string) []string {
	render := func(l string, restricted bool) string {
		indent, mark, body := "", "", ""
		if m := reBullet.FindStringSubmatch(l); m != nil {
			indent, body = m[1], m[2]
		} else if m := reNumbered.FindStringSubmatch(l); m != nil {
			indent, mark, body = m[1], m[2]+".", m[3]
		}
		level := min(len(strings.ReplaceAll(indent, "\t", "  "))/2, 4)
		if mark == "" {
			mark = "•"
			if level > 0 {
				mark = "◦"
			}
		}
		return strings.Repeat("  ", level) + mark + " " + mdInlineX(body, restricted)
	}
	if len(items) <= listCollapse {
		var ls []string
		for _, it := range items {
			ls = append(ls, render(it, false))
		}
		return []string{strings.Join(ls, "\n")}
	}
	var head, rest []string
	for i, it := range items {
		if i < listVisible {
			head = append(head, render(it, false))
		} else {
			rest = append(rest, render(it, true))
		}
	}
	return []string{strings.Join(head, "\n"), "<blockquote expandable>" + strings.Join(rest, "\n") + "</blockquote>"}
}

/* ---------- tables ---------- */

// runeWidth: display columns of a character in a monospace font: 0 for
// combining marks, joiners and variation selectors, 2 for East Asian wide
// characters and emoji.
func runeWidth(r rune) int {
	switch {
	case r == 0x200D || (r >= 0xFE00 && r <= 0xFE0F) || (r >= 0x1F3FB && r <= 0x1F3FF) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
		return 0
	case r >= 0x1100 && r <= 0x115F, r >= 0x2E80 && r <= 0xA4CF, r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F, r >= 0xFF00 && r <= 0xFF60, r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F000 && r <= 0x1FAFF, r >= 0x2600 && r <= 0x27BF, r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

func strWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

var reTagAny = regexp.MustCompile(`<[^>]+>`)

// plainInline: a cell without its Markdown marks (for widths and the grid).
func plainInline(s string) string {
	return strings.TrimSpace(html.UnescapeString(reTagAny.ReplaceAllString(mdInlineX(s, true), "")))
}

var (
	reTableSep = regexp.MustCompile(`^\s*:?-+:?\s*$`)
	reNumeric  = regexp.MustCompile(`^[-+−]?\d[\d\s.,]*%?$`)
)

// splitRow splits "| a | b |" into cells ("\|" is a literal bar).
func splitRow(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimSuffix(strings.TrimPrefix(row, "|"), "|")
	var cells []string
	var cur strings.Builder
	esc := false
	for _, r := range row {
		switch {
		case esc:
			if r != '|' {
				cur.WriteRune('\\')
			}
			cur.WriteRune(r)
			esc = false
		case r == '\\':
			esc = true
		case r == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

// tableBlocks renders a Markdown table: an aligned <pre> grid when it fits a
// phone (≤ tableMaxCols columns), otherwise a card per row — the first
// column in bold with a short status-like column after it, then
// "Header: value" lines.
func tableBlocks(rows []string) []string {
	var grid [][]string
	for i, r := range rows {
		cells := splitRow(r)
		if i == 1 && func() bool {
			for _, c := range cells {
				if !reTableSep.MatchString(c) {
					return false
				}
			}
			return true
		}() {
			continue // the |---|---| line
		}
		grid = append(grid, cells)
	}
	if len(grid) == 0 {
		return nil
	}
	cols := 0
	for _, r := range grid {
		cols = max(cols, len(r))
	}
	for i := range grid { // ragged rows
		for len(grid[i]) < cols {
			grid[i] = append(grid[i], "")
		}
	}
	plain := make([][]string, len(grid))
	widths := make([]int, cols)
	for i, r := range grid {
		plain[i] = make([]string, cols)
		for j, c := range r {
			plain[i][j] = plainInline(c)
			widths[j] = max(widths[j], strWidth(plain[i][j]))
		}
	}
	total := 2 * (cols - 1)
	for _, w := range widths {
		total += w
	}
	if total <= tableMaxCols && len(grid) <= tableMaxCards+1 {
		var lines []string
		for _, r := range plain {
			var b strings.Builder
			for j, c := range r {
				pad := strings.Repeat(" ", widths[j]-strWidth(c))
				if j > 0 {
					b.WriteString("  ")
				}
				if reNumeric.MatchString(c) {
					b.WriteString(pad + c)
				} else {
					b.WriteString(c + pad)
				}
			}
			lines = append(lines, strings.TrimRight(b.String(), " "))
		}
		return codeBlocks("", lines)
	}
	header, body := plain[0], grid[1:]
	var out []string
	for i, r := range body {
		if i == tableMaxCards {
			n := len(body) - tableMaxCards
			out = append(out, "… ещё "+strconv.Itoa(n)+" "+plural(n, "строка", "строки", "строк"))
			break
		}
		first := "<b>" + mdInlineX(r[0], true) + "</b>"
		status := -1
		for j := 1; j < cols; j++ {
			if p := plainInline(r[j]); p != "" && utf8.RuneCountInString(p) <= statusMaxLen && !reNumeric.MatchString(p) {
				status = j
				break
			}
		}
		if status > 0 {
			first += " · " + mdInline(r[status])
		}
		lines := []string{first}
		for j := 1; j < cols; j++ {
			if j == status || strings.TrimSpace(r[j]) == "" {
				continue
			}
			label := header[j]
			if label == "" {
				label = "—"
			}
			lines = append(lines, html.EscapeString(label)+": "+mdInline(r[j]))
		}
		out = append(out, strings.Join(lines, "\n"))
	}
	return out
}

// codeBlocks renders code lines as <pre> blocks, each under tgMaxText.
func codeBlocks(lang string, lines []string) []string {
	if len(lines) > codeCollapse {
		head := "📄 код"
		if lang != "" {
			head += " (" + html.EscapeString(lang) + ")"
		}
		head += ", " + strconv.Itoa(len(lines)) + " " + plural(len(lines), "строка", "строки", "строк")
		// Telegram does not allow <pre> inside a blockquote: plain lines.
		return append([]string{head}, quoteBlocks(lines, true)...)
	}
	open, close := "<pre>", "</pre>"
	if lang != "" {
		open, close = `<pre><code class="language-`+html.EscapeString(lang)+`">`, "</code></pre>"
	}
	var out []string
	var cur []string
	size := 0
	budget := tgMaxText - utf8.RuneCountInString(open+close) - 10
	flush := func() {
		if len(cur) > 0 {
			out = append(out, open+strings.Join(cur, "\n")+close)
			cur, size = nil, 0
		}
	}
	for _, l := range lines {
		for _, e := range escapedChunks(l, budget) {
			n := utf8.RuneCountInString(e) + 1
			if size+n > budget {
				flush()
			}
			cur = append(cur, e)
			size += n
		}
	}
	flush()
	return out
}

// quoteBlocks puts plain lines (escaped here) into blockquotes of at most
// tgMaxText characters each.
func quoteBlocks(lines []string, expandable bool) []string {
	open, close := "<blockquote>", "</blockquote>"
	if expandable {
		open = "<blockquote expandable>"
	}
	budget := tgMaxText - utf8.RuneCountInString(open+close) - 10
	var out, cur []string
	size := 0
	for _, l := range lines {
		for _, e := range escapedChunks(l, budget) {
			n := utf8.RuneCountInString(e) + 1
			if size+n > budget && len(cur) > 0 {
				out = append(out, open+strings.Join(cur, "\n")+close)
				cur, size = nil, 0
			}
			cur = append(cur, e)
			size += n
		}
	}
	if len(cur) > 0 {
		out = append(out, open+strings.Join(cur, "\n")+close)
	}
	return out
}

// escapedChunks escapes a line, cut into pieces of at most budget characters
// each, never inside an entity.
func escapedChunks(line string, budget int) []string {
	line = stripControl(line)
	var out []string
	var b strings.Builder
	n := 0
	for _, r := range line {
		e := html.EscapeString(string(r))
		if n+utf8.RuneCountInString(e) > budget {
			out = append(out, b.String())
			b.Reset()
			n = 0
		}
		b.WriteString(e)
		n += utf8.RuneCountInString(e)
	}
	return append(out, b.String())
}

// stripControl drops control characters other than newline and tab: they are
// not valid in Telegram's HTML.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// mdToMessages converts Markdown to Telegram HTML messages of at most
// tgMaxText characters each (minus room for a prefix), cut between blocks; a
// text block that alone is too long is cut at line or word boundaries, as
// plain escaped text (its inline formatting is the price of the cut).
func mdToMessages(md string, prefix string) []string {
	budget := tgMaxText - utf8.RuneCountInString(prefix)
	var msgs []string
	cur := ""
	push := func() {
		if cur != "" {
			msgs = append(msgs, prefix+cur)
			cur = ""
		}
	}
	for _, blk := range mdBlocks(md) {
		for _, piece := range fitBlock(blk, budget) {
			if cur != "" && utf8.RuneCountInString(cur)+2+utf8.RuneCountInString(piece) > budget {
				push()
			}
			if cur != "" {
				cur += "\n\n"
			}
			cur += piece
		}
	}
	push()
	return msgs
}

func fitBlock(blk string, budget int) []string {
	if utf8.RuneCountInString(blk) <= budget {
		return []string{blk}
	}
	if strings.HasPrefix(blk, "<pre>") { // codeBlocks already keeps under tgMaxText
		return []string{blk}
	}
	plain := html.UnescapeString(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(blk, ""))
	if strings.HasPrefix(blk, "<blockquote") { // stays a quote, cut between lines
		return quoteBlocks(strings.Split(plain, "\n"), strings.HasPrefix(blk, "<blockquote expandable>"))
	}
	var out []string
	r := []rune(plain)
	for len(r) > 0 {
		n := min(len(r), budget-50)
		if n < len(r) {
			cut := n
			for cut > n/2 && r[cut] != '\n' && r[cut] != ' ' {
				cut--
			}
			if cut > n/2 {
				n = cut
			}
		}
		out = append(out, html.EscapeString(string(r[:n])))
		r = []rune(strings.TrimLeft(string(r[n:]), " \n"))
	}
	return out
}
