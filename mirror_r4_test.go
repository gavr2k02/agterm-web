package main

// Regression tests for the fourth Codex audit (the conversation mirror).

import (
	"context"
	"encoding/xml"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// validTelegramHTML: well-formed (balanced tags, valid entities, no stray
// "<"), only Telegram's tags, and within the message limit.
func validTelegramHTML(t *testing.T, s string) {
	t.Helper()
	// Telegram's <blockquote expandable> has no attribute value; XML wants one.
	x := strings.ReplaceAll(s, "<blockquote expandable>", `<blockquote expandable="">`)
	d := xml.NewDecoder(strings.NewReader("<root>" + x + "</root>"))
	for {
		_, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("invalid HTML (%v): %.300q", err, s)
		}
	}
	balanced(t, s)
	telegramNesting(t, s)
	if n := utf8.RuneCountInString(s); n > 4096 {
		t.Fatalf("%d characters", n)
	}
}

func TestMarkdownFuzzValid(t *testing.T) {
	alphabet := []string{"*", "**", "***", "_", "__", "~~", "`", "```", "[", "]", "(", ")", "https://x.io/", "<", ">", "&", "\"", "'",
		"\n", "\n\n", "# ", "- ", "1. ", "> ", "| a | b |\n", "word", "слово", " ", "\x01", "\x019\x01", "\x00", "é", "🙂"}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for n := rng.Intn(60); n > 0; n-- {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		for _, m := range mdToMessages(b.String(), "<b>s</b>\n") {
			validTelegramHTML(t, m)
		}
	}
	for _, md := range []string{"***important***", "**a *b** c*", "*a **b* c**", "_a __b_ c__", "***", "** **", strings.Repeat("*a", 2000)} {
		for _, m := range mdToMessages(md, "") {
			validTelegramHTML(t, m)
		}
	}
	if m := mdToMessages("***important***", ""); len(m) != 1 || m[0] != "<b><i>important</i></b>" {
		t.Errorf("triple emphasis: %q", m)
	}
	// Placeholder-looking input is text (no panic, nothing swapped in).
	if m := mdToMessages("x \x019999\x01 `c` [l](https://a.b)", ""); len(m) != 1 || !strings.Contains(m[0], "x 9999 <code>c</code>") {
		t.Errorf("control bytes: %q", m)
	}
}

func TestMirrorLongEscapedPrompt(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	f.add(fxPrompt(strings.Repeat("<&", 3000)))
	s := texts(f.tick(), "sendMessage")
	if len(s) != 1 || !strings.HasSuffix(s[0], "…") {
		t.Fatalf("got %d messages", len(s))
	}
	validTelegramHTML(t, s[0])
	f.add(fxUserRaw("<bash-input>"+strings.Repeat("<", 900)+"</bash-input>", nil), fxUserRaw("<bash-stdout>"+strings.Repeat("&\"", 4000)+"</bash-stdout>", nil))
	for _, m := range texts(f.tick(), "sendMessage") {
		validTelegramHTML(t, m)
	}
}

func TestSafelyRecovers(t *testing.T) {
	h := newBotHarness(t)
	h.bot.safely("test", func() { panic("boom") }) // must not propagate
	ctx, cancel := context.WithCancel(context.Background())
	runs := 0
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	h.bot.forever(ctx, "test", func(context.Context) {
		runs++
		if runs == 1 {
			panic("first run")
		}
		<-ctx.Done()
	})
	if runs < 1 {
		t.Fatal("loop never ran")
	}
}

func TestMirrorFallbackCwdCollision(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	a, b := filepath.Join(h.home, "a-b"), filepath.Join(h.home, "a_b")
	h.ag.cwd[sidA], h.ag.fg[sidA] = a, []string{"claude"}
	h.ag.setTree(t, fakeSession{sidA, "A", "active"})
	dir := projectDir(h.bot.projectsDir, a)
	if dir != projectDir(h.bot.projectsDir, b) {
		t.Fatal("no collision to test")
	}
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "other.jsonl")
	os.WriteFile(p, []byte(jline(map[string]any{"type": "user", "cwd": b, "turnOrigin": "human", "message": map[string]any{"content": "B"}})+"\n"), 0o600)
	h.bot.mirrorTick(context.Background())
	if h.bot.isMirrored(sidA) || len(h.bot.mirrors) != 0 {
		t.Fatal("another project's transcript taken")
	}
	// Two transcripts of the right cwd: ambiguous, nothing taken, shown in /status.
	for _, n := range []string{"x.jsonl", "y.jsonl"} {
		os.WriteFile(filepath.Join(dir, n), []byte(jline(map[string]any{"type": "user", "cwd": a})+"\n"), 0o600)
	}
	h.clock = h.clock.Add(mirrorMapEvery)
	h.bot.mirrorTick(context.Background())
	if h.bot.isMirrored(sidA) || len(h.bot.unmirrored) != 1 {
		t.Fatalf("ambiguous: %v %v", h.bot.mirrors, h.bot.unmirrored)
	}
}

func TestMirrorOwnershipChanges(t *testing.T) {
	// Moved to another session: what A buffered goes to A.
	f := mirrorSetup(t, true, "startup")
	f.tick()
	f.add(fxText("A reply"))
	f.tick()
	os.Remove(filepath.Join(f.h.bot.claudeDir, sidA+".left.json"))
	os.WriteFile(filepath.Join(f.h.bot.claudeDir, sidB+".left.json"), []byte(jline(map[string]any{"transcript_path": f.path, "source": "resume"})), 0o600)
	sent := only(f.wait(6*time.Second), "sendMessage")
	if len(sent) != 1 || str(sent[0].Params["text"]) != "🤖 A reply" || num(sent[0].Params["message_thread_id"]) != f.h.bot.st.Topics[sidA].Thread {
		t.Fatalf("buffered reply went to %+v", sent)
	}
	f.add(fxPrompt("now B"))
	sent = only(f.tick(), "sendMessage")
	if len(sent) != 1 || num(sent[0].Params["message_thread_id"]) != f.h.bot.st.Topics[sidB].Thread {
		t.Fatalf("new owner: %+v", sent)
	}

	// /clear: the old conversation's buffered reply is still delivered.
	g := mirrorSetup(t, true, "startup")
	g.tick()
	g.add(fxText("last words"))
	g.tick()
	next := filepath.Join(filepath.Dir(g.path), "next.jsonl")
	os.WriteFile(next, nil, 0o600)
	os.WriteFile(filepath.Join(g.h.bot.claudeDir, sidA+".left.json"), []byte(jline(map[string]any{"transcript_path": next, "source": "clear"})), 0o600)
	if s := texts(g.wait(6*time.Second), "sendMessage"); len(s) != 1 || s[0] != "🤖 last words" {
		t.Fatalf("/clear lost %q", s)
	}

	// One transcript claimed by two sessions: neither, shown in /status.
	c := mirrorSetup(t, true, "startup")
	os.WriteFile(filepath.Join(c.h.bot.claudeDir, sidB+".left.json"), []byte(jline(map[string]any{"transcript_path": c.path, "source": "resume"})), 0o600)
	c.tick()
	if len(c.h.bot.mirrors) != 0 || len(c.h.bot.unmirrored) != 1 || !strings.Contains(c.h.bot.statusText(context.Background()), "один разговор") {
		t.Errorf("conflict: %v %v", c.h.bot.mirrors, c.h.bot.unmirrored)
	}
}

func restartBot(t *testing.T, f *mirrorFix) {
	t.Helper()
	old := f.h.bot
	b, err := newTGBot(old.api, old.srv, filepath.Join(f.h.dir, "telegram-token"))
	if err != nil {
		t.Fatal(err)
	}
	b.now, b.after, b.out = old.now, old.after, old.out
	b.home, b.claudeDir, b.projectsDir, b.settingsPath = old.home, old.claudeDir, old.projectsDir, old.settingsPath
	f.h.bot = b
}

func TestMirrorPendingSurvivesRestart(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	f.tick()
	f.add(fxText("buffered reply"))
	f.tick()
	restartBot(t, f)
	f.add(fxTurnEndIn(3_000)) // the held final text survived the restart
	if s := texts(f.tick(), "sendMessage"); len(s) != 1 || s[0] != "🏁 Полностью готово · 3 с\nbuffered reply" {
		t.Fatalf("held text lost: %q", s)
	}
	// Queued during an outage, then restart: delivered once connectivity returns.
	f.h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "sendMessage" {
			return 503, `{"ok":false,"error_code":503}`, true
		}
		return 0, "", false
	}
	f.add(fxPrompt("during outage"))
	f.tick()
	restartBot(t, f)
	f.h.tg.hook = nil
	if s := texts(f.tick(), "sendMessage"); len(s) != 1 || s[0] != "👤 during outage" {
		t.Fatalf("queue lost: %q", s)
	}
	// The tool line in progress continues as the same message after a restart.
	f.add(fxTool("Read", map[string]any{"file_path": "/a"}))
	f.tick()
	restartBot(t, f)
	f.add(fxTool("Read", map[string]any{"file_path": "/b"}), fxTurnEnd())
	if e := texts(f.tick(), "editMessageText"); len(e) != 1 || e[0] != "🔧 2 действия: Read a · Read b" {
		t.Fatalf("tool line after restart: %q", e)
	}
	if len(f.h.bot.mirrorIDs) != 0 {
		t.Errorf("finished tool line id kept: %v", f.h.bot.mirrorIDs)
	}
}

func TestMirrorDeletedTopicRecreated(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	f.tick()
	old := f.h.bot.st.Topics[sidA].Thread
	f.h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "sendMessage" && num(p["message_thread_id"]) == old {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: message thread not found"}`, true
		}
		return 0, "", false
	}
	f.add(fxPrompt("hello"))
	f.tick()
	calls := f.tick()
	sent := only(calls, "sendMessage")
	if len(only(calls, "createForumTopic")) != 1 || len(sent) != 1 || num(sent[0].Params["message_thread_id"]) == old || str(sent[0].Params["text"]) != "👤 hello" {
		t.Fatalf("not recreated: %+v", calls)
	}
}

func TestMirrorEchoPerPane(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	f.tick()
	f.h.bot.lastPane[sidA] = "right" // Telegram text goes to the right pane
	m := f.h.msg(forumChat, userID, "yes")
	m.IsTopicMessage, m.MessageThreadID = true, f.h.bot.st.Topics[sidA].Thread
	f.h.send(m)
	f.h.ag.typed()
	f.add(fxPrompt("yes")) // the left pane's transcript: typed at the Mac
	if s := texts(f.tick(), "sendMessage"); len(s) != 1 || s[0] != "👤 yes" {
		t.Fatalf("left-pane prompt swallowed: %q", s)
	}
}

func TestMirrorMuteAtSendTime(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	f.tick()
	f.h.bot.lastSrc[sidA] = "tg"
	fail := true
	f.h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "sendMessage" && fail {
			return 502, `{"ok":false,"error_code":502}`, true
		}
		return 0, "", false
	}
	f.add(fxText("reply"), fxTurnEnd())
	f.tick()
	f.h.bot.st.MutedUntil = f.h.clock.Add(time.Hour)
	fail = false
	if s := only(f.tick(), "sendMessage"); len(s) != 1 || s[0].Params["disable_notification"] != true {
		t.Fatalf("mute ignored: %+v", s)
	}
}

func TestMirrorBacklogBounded(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	f.tick()
	f.h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "sendMessage" {
			return 503, `{"ok":false,"error_code":503}`, true
		}
		return 0, "", false
	}
	for i := 0; i < 10; i++ {
		var lines []string
		for j := 0; j < 50; j++ {
			lines = append(lines, fxPrompt(strings.Repeat("x", 500)))
		}
		f.add(lines...)
		f.tick()
	}
	if n := len(f.h.bot.mirrorQ); n > mirrorQMax {
		t.Fatalf("queue grew to %d", n)
	}
	// Nothing is lost: once Telegram is back, reading resumes where it paused.
	f.h.tg.hook = nil
	total := 0
	for i := 0; i < 20; i++ {
		total += len(texts(f.tick(), "sendMessage"))
	}
	if total != 500 {
		t.Errorf("delivered %d of 500", total)
	}
}

func TestMirrorTranscriptReplaced(t *testing.T) {
	f := mirrorSetup(t, true, "startup", fxPrompt(strings.Repeat("old", 100)))
	f.tick()
	f.h.tg.take()
	// Replaced by a new, shorter file (another inode) with a complete entry.
	tmp := f.path + ".new"
	os.WriteFile(tmp, []byte(fxPrompt("after replace")+"\n"), 0o600)
	os.Rename(tmp, f.path)
	if s := texts(f.tick(), "sendMessage"); len(s) != 1 || s[0] != "👤 after replace" {
		t.Fatalf("replacement: %q", s)
	}
	// Truncated in place: also read from the start.
	os.WriteFile(f.path, []byte(fxPrompt("after truncate")+"\n"), 0o600)
	if s := texts(f.tick(), "sendMessage"); len(s) != 1 || s[0] != "👤 after truncate" {
		t.Fatalf("truncation: %q", s)
	}
}

// telegramNesting checks the Bot API's nesting rules: blockquotes are not
// nested and hold only b/i/s/u; <pre> holds only <code>; <code> and <a> hold
// no other entity (except b/i/s/u inside <a>).
func telegramNesting(t *testing.T, s string) {
	t.Helper()
	var stack []string
	for _, m := range reTag.FindAllStringSubmatch(s, -1) {
		if m[1] == "/" {
			stack = stack[:len(stack)-1]
			continue
		}
		tag := m[2]
		for _, outer := range stack {
			ok := true
			switch outer {
			case "blockquote":
				ok = tag == "b" || tag == "i" || tag == "s" || tag == "u"
			case "pre":
				ok = tag == "code"
			case "code":
				ok = false
			case "a":
				ok = tag == "b" || tag == "i" || tag == "s" || tag == "u"
			case "b", "i", "s", "u":
				ok = tag != "code" && tag != "pre"
			}
			if !ok {
				t.Fatalf("<%s> inside <%s>: %.300q", tag, outer, s)
			}
		}
		stack = append(stack, tag)
	}
}
