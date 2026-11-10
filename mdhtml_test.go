package main

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var reTag = regexp.MustCompile(`<(/?)([a-z]+)[^>]*>`)

// balanced: every tag is one of Telegram's and closed in order; no stray "<".
func balanced(t *testing.T, s string) {
	t.Helper()
	allowed := map[string]bool{"b": true, "i": true, "s": true, "u": true, "code": true, "pre": true, "a": true, "blockquote": true}
	var stack []string
	for _, m := range reTag.FindAllStringSubmatch(s, -1) {
		if !allowed[m[2]] {
			t.Fatalf("tag <%s> in %q", m[2], s)
		}
		if m[1] == "" {
			stack = append(stack, m[2])
			continue
		}
		if len(stack) == 0 || stack[len(stack)-1] != m[2] {
			t.Fatalf("unbalanced </%s> in %q", m[2], s)
		}
		stack = stack[:len(stack)-1]
	}
	if len(stack) != 0 {
		t.Fatalf("unclosed %v in %q", stack, s)
	}
	if strings.Count(reTag.ReplaceAllString(s, ""), "<") != 0 {
		t.Fatalf("raw < in %q", s)
	}
	if !utf8.ValidString(s) {
		t.Fatal("invalid UTF-8")
	}
}

func TestMarkdownInline(t *testing.T) {
	for in, want := range map[string]string{
		"**bold** and *it* and _it2_":        "<b>bold</b> and <i>it</i> and <i>it2</i>",
		"**bold *nested* bold**":             "<b>bold <i>nested</i> bold</b>",
		"use `a<b> && c` here":               "use <code>a&lt;b&gt; &amp;&amp; c</code> here",
		"`**not bold**`":                     "<code>**not bold**</code>",
		"[docs](https://x.io/a?b=1&c=\"2\")": `<a href="https://x.io/a?b=1&amp;c=&#34;2&#34;">docs</a>`,
		"[evil](javascript:alert(1))":        "[evil](javascript:alert(1))",
		"snake_case_name and 2*3*4":          "snake_case_name and 2*3*4",
		"<script>alert(1)</script>":          "&lt;script&gt;alert(1)&lt;/script&gt;",
		"~~gone~~ **unclosed":                "<s>gone</s> **unclosed",
		"a * b * c":                          "a * b * c",
	} {
		got := mdInline(in)
		if got != want {
			t.Errorf("mdInline(%q)\n got %q\nwant %q", in, got, want)
		}
		balanced(t, got)
	}
}

func TestMarkdownBlocks(t *testing.T) {
	md := "# Title\n\nSome **text**.\n\n- one\n- two `x`\n  - nested\n1. first\n\n> quoted\n\n```go\nfunc main() { if a < b {} }\n```\n\n| a | b |\n|---|---|\n| 1 | <2> |\n\n---\nend"
	msgs := mdToMessages(md, "")
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	got := msgs[0]
	for _, want := range []string{
		"<b>Title</b>",
		"Some <b>text</b>.",
		"• one\n• two <code>x</code>\n  ◦ nested\n1. first",
		"<blockquote>quoted</blockquote>",
		`<pre><code class="language-go">func main() { if a &lt; b {} }</code></pre>`,
		"<pre>a  b\n1  &lt;2&gt;</pre>",
		"———\n\nend",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q in\n%s", want, got)
		}
	}
	balanced(t, got)
	// An unclosed fence runs to the end.
	if m := mdToMessages("```\ncode <x>", ""); len(m) != 1 || m[0] != "<pre>code &lt;x&gt;</pre>" {
		t.Errorf("unclosed fence: %q", m)
	}
}

func TestMarkdownSplitting(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString("Paragraph with **bold** and `code` & <tags> " + strings.Repeat("слово ", 20) + "\n\n")
	}
	b.WriteString("```python\n" + strings.Repeat("print('<x>' & 1)\n", 400) + "```\n")
	b.WriteString(strings.Repeat("длинная строка без переносов ", 400)) // one block over the limit
	prefix := "<b>session</b>\n"
	msgs := mdToMessages(b.String(), prefix)
	if len(msgs) < 4 {
		t.Fatalf("only %d messages", len(msgs))
	}
	total := 0
	for _, m := range msgs {
		if n := utf8.RuneCountInString(m); n > tgMaxText {
			t.Errorf("message of %d chars", n)
		}
		if !strings.HasPrefix(m, prefix) {
			t.Error("prefix missing")
		}
		balanced(t, m)
		total += strings.Count(m, "print(")
	}
	if total != 400 {
		t.Errorf("code lines lost or duplicated: %d", total)
	}
	// A single enormous code line is cut, never inside an entity.
	for _, m := range mdToMessages("```\n"+strings.Repeat("&<>\"", 3000)+"\n```", "") {
		balanced(t, m)
		if utf8.RuneCountInString(m) > tgMaxText || regexp.MustCompile(`&[a-z#0-9]*$|^[a-z#0-9]*;`).MatchString(reTag.ReplaceAllString(m, "")) {
			t.Errorf("bad cut: %d chars", utf8.RuneCountInString(m))
		}
	}
}
