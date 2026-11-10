package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func one(t *testing.T, md string) string {
	t.Helper()
	m := mdToMessages(md, "")
	if len(m) != 1 {
		t.Fatalf("%d messages for %q", len(m), md)
	}
	validTelegramHTML(t, m[0])
	return m[0]
}

func TestWidths(t *testing.T) {
	for s, w := range map[string]int{"abc": 3, "привет": 6, "✅ ok": 5, "日本": 4, "é": 1, "👍🏽": 2, "❤️": 2} {
		if got := strWidth(s); got != w {
			t.Errorf("strWidth(%q) = %d, want %d", s, got, w)
		}
	}
}

func TestTableGrid(t *testing.T) {
	got := one(t, "| Имя | Кол-во | Ок |\n|---|---:|---|\n| **a** | 1 | ✅ |\n| бб | 22 | `x` |\n| c |")
	want := "<pre>Имя  Кол-во  Ок\na         1  ✅\nбб       22  x\nc</pre>"
	if got != want {
		t.Errorf("grid\n got %q\nwant %q", got, want)
	}
}

func TestTableCards(t *testing.T) {
	md := "| Задача | Статус | Описание | Срок |\n|---|---|---|---|\n" +
		"| Логин через **Google** | готово | Кнопка и `callback` на сервере, тесты на всё | 12.11 |\n" +
		"| Экспорт | в работе | | |\n" +
		"| Отчёты | | Длинное описание без статуса, которое точно не влезет в узкую сетку | 20.11 |"
	m := mdToMessages(md, "")
	if len(m) != 1 {
		t.Fatalf("%d messages", len(m))
	}
	validTelegramHTML(t, m[0])
	want := "<b>Логин через <b>Google</b></b> · готово\nОписание: Кнопка и <code>callback</code> на сервере, тесты на всё\nСрок: 12.11\n\n" +
		"<b>Экспорт</b> · в работе\n\n" +
		"<b>Отчёты</b>\nОписание: Длинное описание без статуса, которое точно не влезет в узкую сетку\nСрок: 20.11"
	if m[0] != want {
		t.Errorf("cards\n got %q\nwant %q", m[0], want)
	}
	// More than 20 rows: the first 20 and a count.
	var b strings.Builder
	b.WriteString("| Name | Long description column |\n|---|---|\n")
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&b, "| row %d | %s |\n", i, strings.Repeat("words ", 8))
	}
	got := strings.Join(mdToMessages(b.String(), ""), "\n\n")
	if strings.Count(got, "<b>row ") != 20 || !strings.HasSuffix(got, "… ещё 5 строк") {
		t.Errorf("long table: %d cards, tail %q", strings.Count(got, "<b>row "), got[len(got)-30:])
	}
}

func TestExpandables(t *testing.T) {
	code := func(n int) string {
		var ls []string
		for i := 0; i < n; i++ {
			ls = append(ls, fmt.Sprintf("line %d <x>", i))
		}
		return "```go\n" + strings.Join(ls, "\n") + "\n```"
	}
	if got := one(t, code(12)); !strings.HasPrefix(got, `<pre><code class="language-go">`) {
		t.Errorf("12 lines folded: %.60q", got)
	}
	got := one(t, code(13))
	if !strings.HasPrefix(got, "📄 код (go), 13 строк\n\n<blockquote expandable>line 0 &lt;x&gt;\n") || strings.Contains(got, "<pre>") {
		t.Errorf("13 lines: %.120q", got)
	}
	list := func(n int) string {
		var ls []string
		for i := 1; i <= n; i++ {
			ls = append(ls, fmt.Sprintf("- item %d `c`", i))
		}
		return strings.Join(ls, "\n")
	}
	if got := one(t, list(15)); strings.Contains(got, "blockquote") {
		t.Errorf("15 items folded")
	}
	got = one(t, list(16))
	if !strings.HasPrefix(got, "• item 1 <code>c</code>\n") || !strings.Contains(got, "• item 5 <code>c</code>\n\n<blockquote expandable>• item 6 c\n") || strings.Count(got, "•") != 16 {
		t.Errorf("16 items: %q", got)
	}
	// Huge code splits into several messages, each a whole quote.
	msgs := mdToMessages(code(2000), "<b>s</b>\n")
	if len(msgs) < 3 {
		t.Fatalf("%d messages", len(msgs))
	}
	for _, m := range msgs {
		validTelegramHTML(t, m)
	}
	// A long quote is split as quotes too.
	var q strings.Builder
	for i := 0; i < 400; i++ {
		q.WriteString("> строка цитаты с текстом " + fmt.Sprint(i) + "\n")
	}
	for _, m := range mdToMessages(q.String(), "") {
		validTelegramHTML(t, m)
		if !strings.HasPrefix(m, "<blockquote>") {
			t.Errorf("quote piece %.40q", m)
		}
	}
}

func TestListsQuotesHeadingsFiles(t *testing.T) {
	got := one(t, "## План `x`\n\n1. первый\n   - вложенный\n     - глубже\n2. второй\n\n> цитата с `кодом` и [ссылкой](https://a.b)\n\nсм. internal/auth.go:42, main.go и https://x.io/a.js")
	for _, want := range []string{
		"<b>План x</b>\n\n1. первый\n  ◦ вложенный\n    ◦ глубже\n2. второй",
		"<blockquote>цитата с кодом и ссылкой (https://a.b)</blockquote>",
		"см. <code>internal/auth.go:42</code>, <code>main.go</code> и https://x.io/a.js",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q in\n%s", want, got)
		}
	}
	// Code inside emphasis is not allowed by Telegram: it becomes text.
	if got := one(t, "**run `go test` in main.go**"); got != "<b>run go test in main.go</b>" {
		t.Errorf("emphasis with code: %q", got)
	}
}

func TestMirrorBashOutputFolds(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	out := func(n int) string {
		var ls []string
		for i := 0; i < n; i++ {
			ls = append(ls, fmt.Sprintf("out %d", i))
		}
		return strings.Join(ls, "\n")
	}
	f.add(fxUserRaw("<bash-input>ls</bash-input>", nil), fxUserRaw("<bash-stdout>"+out(8)+"</bash-stdout>", nil),
		fxUserRaw("<bash-input>ls -la</bash-input>", nil), fxUserRaw("<bash-stdout>"+out(9)+"</bash-stdout>", nil))
	s := texts(f.tick(), "sendMessage")
	if len(s) != 2 || !strings.Contains(s[0], "<pre>out 0") || !strings.Contains(s[1], "<blockquote expandable>out 0") {
		t.Fatalf("got %q", s)
	}
	for _, m := range s {
		validTelegramHTML(t, m)
	}
	_ = context.Background
}
