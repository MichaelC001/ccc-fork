package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestRenderTelegramHTML(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{{
		name: "angle brackets in model output are escaped, not sent as tags",
		// The production failure: Telegram answered "can't parse entities" and
		// the chunk was dropped.
		in:   "Use header Authorization: Bearer <20>:<20> with <token_del_agente>",
		want: "Use header Authorization: Bearer &lt;20&gt;:&lt;20&gt; with &lt;token_del_agente&gt;",
	}, {
		name: "ampersands are escaped before anything else",
		in:   "a & b <c> &amp;",
		want: "a &amp; b &lt;c&gt; &amp;amp;",
	}, {
		name: "bold",
		in:   "this is **bold** text",
		want: "this is <b>bold</b> text",
	}, {
		name: "underscore bold",
		in:   "this is __bold__ text",
		want: "this is <b>bold</b> text",
	}, {
		name: "italic",
		in:   "this is *italic* text",
		want: "this is <i>italic</i> text",
	}, {
		name: "underscore italic at word boundaries",
		in:   "this is _italic_ text",
		want: "this is <i>italic</i> text",
	}, {
		name: "underscores inside identifiers are left alone",
		in:   "call update_instructions and send_to_bot now",
		want: "call update_instructions and send_to_bot now",
	}, {
		name: "strikethrough",
		in:   "~~gone~~ now",
		want: "<s>gone</s> now",
	}, {
		name: "inline code escapes its content",
		in:   "run `curl -H 'X: <tok>' host` please",
		want: "run <code>curl -H 'X: &lt;tok&gt;' host</code> please",
	}, {
		name: "nested code inside bold",
		in:   "**set `<token>` first**",
		want: "<b>set <code>&lt;token&gt;</code> first</b>",
	}, {
		name: "headings become bold",
		in:   "# Title\n## Sub <x>\n### Third",
		want: "<b>Title</b>\n<b>Sub &lt;x&gt;</b>\n<b>Third</b>",
	}, {
		name: "bullets become dots",
		in:   "- first\n* second\n  - nested <a>",
		want: "• first\n• second\n  • nested &lt;a&gt;",
	}, {
		name: "horizontal rule is not a bullet",
		in:   "---",
		want: "---",
	}, {
		name: "links",
		in:   "see [the docs](https://example.com/a?x=1&y=2) now",
		want: "see <a href=\"https://example.com/a?x=1&amp;y=2\">the docs</a> now",
	}, {
		name: "links with a rejected scheme stay literal",
		in:   "see [x](javascript:alert(1))",
		want: "see [x](javascript:alert(1))",
	}, {
		name: "fenced block keeps markdown literal",
		in:   "before\n```go\nif a < b && **x** {\n\treturn \"<y>\"\n}\n```\nafter",
		want: "before\n<pre><code class=\"language-go\">if a &lt; b &amp;&amp; **x** {\n\treturn \"&lt;y&gt;\"\n}</code></pre>\nafter",
	}, {
		name: "fenced block without a language",
		in:   "```\n<raw> & **stuff**\n```",
		want: "<pre>&lt;raw&gt; &amp; **stuff**</pre>",
	}, {
		name: "unterminated fence still closes its tag",
		in:   "```\n<oops>",
		want: "<pre>&lt;oops&gt;</pre>",
	}, {
		name: "emphasis around whitespace and dangling markers stay literal",
		in:   "2 * 3 * 4 and **dangling",
		want: "2 * 3 * 4 and **dangling",
	}, {
		name: "empty input",
		in:   "",
		want: "",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderTelegramHTML(c.in)
			if got != c.want {
				t.Fatalf("renderTelegramHTML(%q)\n got: %q\nwant: %q", c.in, got, c.want)
			}
			if err := checkTelegramHTML(got); err != nil {
				t.Fatalf("rendered output is not valid Telegram HTML: %v\noutput: %q", err, got)
			}
		})
	}
}

// TestRenderTelegramHTMLNeverLeaksRawAngleBrackets is the invariant that makes
// the "can't parse entities" rejection impossible: every "<" in the output
// opens a tag the renderer itself emitted.
func TestRenderTelegramHTMLNeverLeaksRawAngleBrackets(t *testing.T) {
	inputs := []string{
		"<b>not really bold</b>",
		"a<b>c</i>",
		"if x<y && y>z { }",
		"<<<>>>",
		"`<`",
		"**<**",
		"[<](<)",
		"5 < 6 & 7 > 3",
	}
	for _, in := range inputs {
		out := renderTelegramHTML(in)
		if err := checkTelegramHTML(out); err != nil {
			t.Errorf("renderTelegramHTML(%q) = %q: %v", in, out, err)
		}
	}
}

func TestSplitTelegramHTMLShortMessageIsUntouched(t *testing.T) {
	in := renderTelegramHTML("hello <world>")
	got := splitTelegramHTML(in, telegramChunkLimit)
	if len(got) != 1 || got[0] != in {
		t.Fatalf("splitTelegramHTML(%q) = %q, want a single untouched chunk", in, got)
	}
}

// TestSplitTelegramHTMLLongReply is the regression for the 4787-char reply
// that Telegram rejected in production: after rendering it must split into
// chunks that each fit and are each independently valid.
func TestSplitTelegramHTMLLongReply(t *testing.T) {
	sample := longReplySample()
	if len(sample) != 4787 {
		t.Fatalf("sample is %d chars, want 4787", len(sample))
	}

	chunks := splitTelegramHTML(renderTelegramHTML(sample), telegramChunkLimit)
	if len(chunks) < 2 {
		t.Fatalf("a %d-char reply should split into several chunks, got %d", len(sample), len(chunks))
	}
	for i, c := range chunks {
		if len(c) > telegramTextLimit {
			t.Errorf("chunk %d is %d bytes, over Telegram's %d cap", i+1, len(c), telegramTextLimit)
		}
		if err := checkTelegramHTML(c); err != nil {
			t.Errorf("chunk %d is not valid Telegram HTML: %v", i+1, err)
		}
	}

	// Nothing may be lost in the split: the visible text of the chunks, taken
	// together, is the visible text of the whole rendered reply.
	var joined strings.Builder
	for _, c := range chunks {
		joined.WriteString(plainTextFromHTML(c))
	}
	want := squeeze(plainTextFromHTML(renderTelegramHTML(sample)))
	if got := squeeze(joined.String()); got != want {
		t.Errorf("text was lost or duplicated in the split:\n got %d chars\nwant %d chars", len(got), len(want))
	}
}

func TestSplitTelegramHTMLReopensTagsAcrossACut(t *testing.T) {
	body := strings.TrimSpace(strings.Repeat("word ", 40))
	in := renderTelegramHTML("**" + body + "**")
	chunks := splitTelegramHTML(in, 120)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 120 {
			t.Errorf("chunk %d is %d bytes, over the 120 budget", i+1, len(c))
		}
		if err := checkTelegramHTML(c); err != nil {
			t.Errorf("chunk %d: %v (%q)", i+1, err, c)
		}
		if !strings.HasPrefix(c, "<b>") || !strings.HasSuffix(c, "</b>") {
			t.Errorf("chunk %d does not carry the bold across the cut: %q", i+1, c)
		}
	}
}

func TestSplitTelegramHTMLSplitsInsideACodeBlock(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("```go\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "fmt.Println(%d < %d)\n", i, i+1)
	}
	sb.WriteString("```")

	chunks := splitTelegramHTML(renderTelegramHTML(sb.String()), 400)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 400 {
			t.Errorf("chunk %d is %d bytes, over the 400 budget", i+1, len(c))
		}
		if err := checkTelegramHTML(c); err != nil {
			t.Errorf("chunk %d: %v (%q)", i+1, err, c)
		}
		if !strings.HasPrefix(c, "<pre><code class=\"language-go\">") {
			t.Errorf("chunk %d does not reopen the code block: %q", i+1, c[:min(40, len(c))])
		}
	}
}

func TestSplitTelegramHTMLNeverCutsAnEntity(t *testing.T) {
	in := renderTelegramHTML(strings.Repeat("a<b>&c ", 200))
	for _, budget := range []int{40, 41, 42, 43, 57, 64, 100, 313} {
		for i, c := range splitTelegramHTML(in, budget) {
			if err := checkTelegramHTML(c); err != nil {
				t.Errorf("budget %d chunk %d: %v (%q)", budget, i+1, err, c)
			}
		}
	}
}

func TestPlainTextFromHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<b>bold</b> and <i>italic</i>", "bold and italic"},
		{"&lt;token&gt; &amp; more", "<token> & more"},
		{"<pre><code class=\"language-go\">a &lt; b</code></pre>", "a < b"},
		{"<a href=\"https://x/\">link</a>", "link"},
		{"2 &lt; 3", "2 < 3"},
		{"&amp;lt; stays escaped once", "&lt; stays escaped once"},
	}
	for _, c := range cases {
		if got := plainTextFromHTML(c.in); got != c.want {
			t.Errorf("plainTextFromHTML(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// checkTelegramHTML reports whether a string is something Telegram's HTML
// parser would accept: balanced known tags and no bare "<" or "&".
func checkTelegramHTML(s string) error {
	known := map[string]bool{"b": true, "i": true, "s": true, "u": true, "a": true, "code": true, "pre": true}
	var stack []string
	for i := 0; i < len(s); {
		switch s[i] {
		case '<':
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				return fmt.Errorf("bare %q at %d", '<', i)
			}
			inner := s[i+1 : i+end]
			name := strings.TrimPrefix(inner, "/")
			if sp := strings.IndexByte(name, ' '); sp > 0 {
				name = name[:sp]
			}
			if !known[name] {
				return fmt.Errorf("unsupported or bare tag %q at %d", inner, i)
			}
			if strings.HasPrefix(inner, "/") {
				if len(stack) == 0 || stack[len(stack)-1] != name {
					return fmt.Errorf("unbalanced </%s> at %d", name, i)
				}
				stack = stack[:len(stack)-1]
			} else {
				stack = append(stack, name)
			}
			i += end + 1
		case '&':
			end := strings.IndexByte(s[i:], ';')
			if end < 2 || end > 8 {
				return fmt.Errorf("bare %q at %d", '&', i)
			}
			i += end + 1
		default:
			i++
		}
	}
	if len(stack) > 0 {
		return fmt.Errorf("tags left open: %v", stack)
	}
	return nil
}

// squeeze collapses whitespace so text comparisons survive the newline a split
// point may swallow at a chunk boundary.
func squeeze(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// longReplySample reproduces the shape of the 4787-char reply that Telegram
// rejected: prose, angle-bracket placeholders, bullets and a code block.
func longReplySample() string {
	var sb strings.Builder
	sb.WriteString("# Deploy report\n\n")
	sb.WriteString("The agent finished. Set `Authorization: Bearer <20>:<20>` and pass ")
	sb.WriteString("<token_del_agente> in every request; see [the docs](https://example.com/docs).\n\n")
	for i := 0; sb.Len() < 3400; i++ {
		fmt.Fprintf(&sb, "- step %d: **checked** `unit-%d.service` and compared a < b && c > d in update_instructions\n", i, i)
	}
	sb.WriteString("\n```bash\n")
	for i := 0; sb.Len() < 4600; i++ {
		fmt.Fprintf(&sb, "curl -sS -H 'X-Token: <tok%d>' https://host/%d | jq '.items[] | select(.n > %d)'\n", i, i, i)
	}
	sb.WriteString("```\n\nDone — ~~no~~ **all** checks passed.")

	out := sb.String()
	const want = 4787
	if len(out) < want {
		return out + strings.Repeat(" .", (want-len(out))/2)
	}
	return out[:want]
}
