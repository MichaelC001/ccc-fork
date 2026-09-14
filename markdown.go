package main

import (
	"strings"
	"unicode/utf8"
)

// Telegram's Bot API does not accept the markdown-ish plain text a model
// writes: with parse_mode=HTML every raw "&", "<" or ">" is a parse error
// ("can't parse entities") and the whole message is rejected. Model output is
// full of them — placeholders such as <token>, generics, shell redirects — so
// passing it through untouched silently drops replies.
//
// renderTelegramHTML escapes the text first and only then re-introduces the
// small tag subset Telegram understands, which makes malformed output
// impossible: a tag is emitted only when its closing delimiter was found.

// telegramAllowedLinkSchemes are the URL schemes a rendered link may use; any
// other scheme is left as escaped literal text.
var telegramAllowedLinkSchemes = []string{"http://", "https://", "tg://", "mailto:"}

// renderTelegramHTML converts the markdown a model typically emits into the
// HTML subset Telegram accepts. Anything that is not recognised as markup ends
// up escaped, and tags are only ever emitted in balanced pairs.
func renderTelegramHTML(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		fence, info, ok := codeFence(lines[i])
		if !ok {
			out = append(out, renderTelegramBlockLine(lines[i]))
			continue
		}
		body, next := collectFencedBlock(lines, i+1, len(fence))
		out = append(out, renderCodeBlock(info, body))
		i = next
	}
	return strings.Join(out, "\n")
}

// collectFencedBlock returns the lines of a fenced code block that starts at
// start, plus the index of its closing fence (or of the last line when the
// block is never closed — the tag is still balanced either way).
func collectFencedBlock(lines []string, start, fenceLen int) (string, int) {
	for i := start; i < len(lines); i++ {
		fence, info, ok := codeFence(lines[i])
		if ok && info == "" && len(fence) >= fenceLen {
			return strings.Join(lines[start:i], "\n"), i
		}
	}
	return strings.Join(lines[start:], "\n"), len(lines) - 1
}

// codeFence reports whether a line is a ``` fence, returning the backtick run
// and the info string (the language, on an opening fence).
func codeFence(line string) (fence, info string, ok bool) {
	t := strings.TrimLeft(line, " \t")
	n := 0
	for n < len(t) && t[n] == '`' {
		n++
	}
	if n < 3 {
		return "", "", false
	}
	return t[:n], strings.TrimSpace(t[n:]), true
}

// renderCodeBlock wraps escaped code in the <pre> form Telegram documents,
// tagging the language when the info string is a plain identifier.
func renderCodeBlock(info, body string) string {
	if isLanguageTag(info) {
		return "<pre><code class=\"language-" + info + "\">" + htmlEscape(body) + "</code></pre>"
	}
	return "<pre>" + htmlEscape(body) + "</pre>"
}

func isLanguageTag(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '+' || c == '-' || c == '#' || c == '.' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// renderTelegramBlockLine applies the line-level markdown (headings, bullets)
// and then the inline markup.
func renderTelegramBlockLine(line string) string {
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	rest := line[len(indent):]

	if level := headingLevel(rest); level > 0 {
		title := strings.TrimSpace(rest[level:])
		if title != "" {
			return indent + "<b>" + renderInlineTelegramHTML(title) + "</b>"
		}
	}
	if len(rest) > 1 && (rest[0] == '-' || rest[0] == '*' || rest[0] == '+') &&
		(rest[1] == ' ' || rest[1] == '\t') {
		return indent + "• " + renderInlineTelegramHTML(strings.TrimLeft(rest[1:], " \t"))
	}
	return indent + renderInlineTelegramHTML(rest)
}

// headingLevel returns the number of leading '#' when the line is an ATX
// heading ("# ", "## ", …), and 0 otherwise.
func headingLevel(s string) int {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(s) || (s[n] != ' ' && s[n] != '\t') {
		return 0
	}
	return n
}

// renderInlineTelegramHTML escapes a single line and converts the inline
// markdown inside it. Every byte that is not consumed by a recognised
// construct is escaped, so the output can never contain a stray "<".
func renderInlineTelegramHTML(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	for i := 0; i < len(s); {
		var (
			next int
			out  string
			ok   bool
		)
		switch s[i] {
		case '`':
			next, out, ok = inlineCode(s, i)
		case '[':
			next, out, ok = inlineLink(s, i)
		case '*', '_', '~':
			next, out, ok = inlineEmphasis(s, i)
		}
		if ok {
			b.WriteString(out)
			i = next
			continue
		}
		b.WriteString(escapeByteForHTML(s[i]))
		i++
	}
	return b.String()
}

func escapeByteForHTML(c byte) string {
	switch c {
	case '&':
		return "&amp;"
	case '<':
		return "&lt;"
	case '>':
		return "&gt;"
	}
	return string(c)
}

// inlineCode renders a `code` span. The content is escaped and never gets any
// further formatting, which is what makes `<token>` safe to show verbatim.
func inlineCode(s string, i int) (int, string, bool) {
	n := backtickRun(s, i)
	j := i + n
	for j < len(s) {
		if s[j] != '`' {
			j++
			continue
		}
		m := backtickRun(s, j)
		if m == n {
			content := s[i+n : j]
			if strings.TrimSpace(content) == "" {
				return 0, "", false
			}
			return j + m, "<code>" + htmlEscape(content) + "</code>", true
		}
		j += m
	}
	return 0, "", false
}

func backtickRun(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}

// inlineLink renders [text](url) when the URL uses a scheme Telegram will
// accept; anything else falls through and is shown as escaped literal text.
func inlineLink(s string, i int) (int, string, bool) {
	label := strings.IndexByte(s[i:], ']')
	if label < 0 {
		return 0, "", false
	}
	label += i
	if label+1 >= len(s) || s[label+1] != '(' {
		return 0, "", false
	}
	end := strings.IndexByte(s[label+2:], ')')
	if end < 0 {
		return 0, "", false
	}
	end += label + 2
	url := strings.TrimSpace(s[label+2 : end])
	if url == "" || strings.ContainsAny(url, " \t\"'<>") || !hasAllowedScheme(url) {
		return 0, "", false
	}
	text := s[i+1 : label]
	rendered := renderInlineTelegramHTML(text)
	if strings.TrimSpace(text) == "" {
		rendered = htmlEscape(url)
	}
	return end + 1, "<a href=\"" + htmlEscape(url) + "\">" + rendered + "</a>", true
}

func hasAllowedScheme(url string) bool {
	lower := strings.ToLower(url)
	for _, s := range telegramAllowedLinkSchemes {
		if strings.HasPrefix(lower, s) {
			return true
		}
	}
	return false
}

// inlineEmphasis renders **bold**, __bold__, *italic*, _italic_ and ~~strike~~.
// Underscores are only treated as emphasis at word boundaries so that
// snake_case_identifiers survive untouched.
func inlineEmphasis(s string, i int) (int, string, bool) {
	delim, tag := emphasisDelimiter(s, i)
	if delim == "" {
		return 0, "", false
	}
	if delim == "_" && i > 0 && isWordByte(s[i-1]) {
		return 0, "", false
	}
	for j := i + len(delim); j < len(s); {
		if s[j] == '`' {
			if next, _, ok := inlineCode(s, j); ok {
				j = next
				continue
			}
		}
		if !strings.HasPrefix(s[j:], delim) {
			j++
			continue
		}
		content := s[i+len(delim) : j]
		end := j + len(delim)
		if delim == "_" && end < len(s) && isWordByte(s[end]) {
			j = end
			continue
		}
		if content == "" || isSpaceByte(content[0]) || isSpaceByte(content[len(content)-1]) {
			j = end
			continue
		}
		return end, "<" + tag + ">" + renderInlineTelegramHTML(content) + "</" + tag + ">", true
	}
	return 0, "", false
}

func emphasisDelimiter(s string, i int) (delim, tag string) {
	switch {
	case strings.HasPrefix(s[i:], "***"):
		return "", "" // ambiguous; leave it escaped rather than guess
	case strings.HasPrefix(s[i:], "**"):
		return "**", "b"
	case strings.HasPrefix(s[i:], "__"):
		return "__", "b"
	case strings.HasPrefix(s[i:], "~~"):
		return "~~", "s"
	case s[i] == '*':
		return "*", "i"
	case s[i] == '_':
		return "_", "i"
	}
	return "", ""
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 0x80 || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n'
}

// ---------------------------------------------------------------------------
// Splitting rendered HTML
// ---------------------------------------------------------------------------

// splitTelegramHTML splits rendered Telegram HTML into chunks of at most
// maxLen bytes. It never cuts a tag, an entity or a multi-byte rune, and it
// never leaves a chunk unbalanced: whatever is still open at the cut is closed
// at the end of the chunk and reopened at the start of the next one, so every
// chunk is independently valid HTML for Telegram.
func splitTelegramHTML(s string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = telegramChunkLimit
	}
	if len(s) <= maxLen {
		return []string{s}
	}
	var out []string
	var open []string // full opening tags, outermost first
	for pos := 0; pos < len(s); {
		prefix := strings.Join(open, "")
		budget := maxLen - len(prefix)
		if budget < 1 {
			budget = 1
		}
		cut, cutOpen := htmlCut(s, pos, open, budget)
		out = append(out, prefix+s[pos:cut]+closeTags(cutOpen))
		open = cutOpen
		pos = cut
		if len(open) == 0 {
			for pos < len(s) && (s[pos] == '\n' || s[pos] == ' ') {
				pos++
			}
		}
	}
	return out
}

// htmlCut finds where to cut s[pos:] so the chunk fits in budget bytes, and
// reports which tags are open there. It prefers a newline, then a space, and
// only falls back to a hard cut at a token boundary.
func htmlCut(s string, pos int, open []string, budget int) (int, []string) {
	stack := open
	used := 0
	bestNL, bestSpace := -1, -1
	var bestNLStack, bestSpaceStack []string

	i := pos
	for i < len(s) {
		n, kind, name := nextHTMLToken(s, i)
		after := stack
		switch kind {
		case htmlTokenOpen:
			after = append(append([]string(nil), stack...), s[i:i+n])
		case htmlTokenClose:
			if len(stack) > 0 && tagName(stack[len(stack)-1]) == name {
				after = stack[:len(stack)-1]
			}
		default: // exhaustive-ok: text tokens leave the open-tag stack alone
		}
		if i > pos && used+n+closeTagsLen(after) > budget {
			break
		}
		used += n
		stack = after
		i += n
		if kind != htmlTokenText {
			continue
		}
		switch s[i-1] {
		case '\n':
			bestNL, bestNLStack = i, stack
		case ' ':
			bestSpace, bestSpaceStack = i, stack
		}
	}
	if i >= len(s) {
		return len(s), stack
	}
	half := pos + budget/2
	if bestNL > half {
		return bestNL, bestNLStack
	}
	if bestSpace > half {
		return bestSpace, bestSpaceStack
	}
	return i, stack
}

type htmlTokenKind int

const (
	htmlTokenText htmlTokenKind = iota
	htmlTokenOpen
	htmlTokenClose
)

// nextHTMLToken measures the token starting at i: a tag, an entity, or a
// single rune. Anything that only looks like a tag is treated as text.
func nextHTMLToken(s string, i int) (n int, kind htmlTokenKind, name string) {
	if s[i] == '<' {
		end := strings.IndexByte(s[i:], '>')
		if end > 0 {
			inner := s[i+1 : i+end]
			if strings.HasSuffix(inner, "/") {
				return end + 1, htmlTokenText, ""
			}
			if strings.HasPrefix(inner, "/") {
				if isTagName(inner[1:]) {
					return end + 1, htmlTokenClose, inner[1:]
				}
			} else if nm := tagNameOf(inner); nm != "" {
				return end + 1, htmlTokenOpen, nm
			}
		}
	}
	if s[i] == '&' {
		if end := strings.IndexByte(s[i:], ';'); end > 1 && end <= 12 {
			return end + 1, htmlTokenText, ""
		}
	}
	_, size := utf8.DecodeRuneInString(s[i:])
	if size < 1 {
		size = 1
	}
	return size, htmlTokenText, ""
}

// tagName extracts the element name from a full opening tag such as
// `<code class="language-go">`.
func tagName(openTag string) string {
	return tagNameOf(strings.TrimSuffix(strings.TrimPrefix(openTag, "<"), ">"))
}

func tagNameOf(inner string) string {
	if end := strings.IndexAny(inner, " \t"); end > 0 {
		inner = inner[:end]
	}
	if !isTagName(inner) {
		return ""
	}
	return inner
}

func isTagName(s string) bool {
	if s == "" || len(s) > 10 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

func closeTags(stack []string) string {
	var b strings.Builder
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteString("</" + tagName(stack[i]) + ">")
	}
	return b.String()
}

func closeTagsLen(stack []string) int {
	n := 0
	for _, t := range stack {
		n += len(tagName(t)) + 3
	}
	return n
}

// plainTextFromHTML undoes renderTelegramHTML well enough to retry a rejected
// message without parse_mode: tags go away, entities come back as themselves.
func plainTextFromHTML(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		n, kind, _ := nextHTMLToken(s, i)
		if kind != htmlTokenText {
			i += n
			continue
		}
		b.WriteString(s[i : i+n])
		i += n
	}
	out := b.String()
	out = strings.ReplaceAll(out, "&lt;", "<")
	out = strings.ReplaceAll(out, "&gt;", ">")
	out = strings.ReplaceAll(out, "&quot;", "\"")
	out = strings.ReplaceAll(out, "&#39;", "'")
	return strings.ReplaceAll(out, "&amp;", "&") // last, so &amp;lt; survives as &lt;
}
