package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* ---------- synthetic transcript entries (shapes as Claude Code writes them) ---------- */

func jline(v any) string { b, _ := json.Marshal(v); return string(b) }

func fxPrompt(text string) string {
	return jline(map[string]any{"type": "user", "isSidechain": false, "turnOrigin": "human", "origin": map[string]string{"kind": "human"}, "promptSource": "typed",
		"message": map[string]any{"role": "user", "content": text}})
}
func fxUserRaw(content any, extra map[string]any) string {
	m := map[string]any{"type": "user", "isSidechain": false, "message": map[string]any{"role": "user", "content": content}}
	for k, v := range extra {
		m[k] = v
	}
	return jline(m)
}
func fxAssistant(block map[string]any, extra map[string]any) string {
	m := map[string]any{"type": "assistant", "isSidechain": false, "message": map[string]any{"id": "msg_1", "role": "assistant", "content": []any{block}}}
	for k, v := range extra {
		m[k] = v
	}
	return jline(m)
}
func fxText(t string) string { return fxAssistant(map[string]any{"type": "text", "text": t}, nil) }
func fxTool(name string, input map[string]any) string {
	return fxAssistant(map[string]any{"type": "tool_use", "id": "toolu_1", "name": name, "input": input, "caller": map[string]any{}}, nil)
}
func fxTurnEnd() string {
	return jline(map[string]any{"type": "system", "subtype": "turn_duration", "durationMs": 1200, "messageCount": 4, "isMeta": false})
}

func TestParseEntry(t *testing.T) {
	type ev struct{ kind, text, err, tool string }
	cases := []struct {
		line string
		want *ev
	}{
		{fxPrompt("fix the bug"), &ev{kind: "prompt", text: "fix the bug"}},
		{fxUserRaw([]any{map[string]any{"type": "text", "text": "look"}, map[string]any{"type": "image", "source": map[string]any{}}}, map[string]any{"turnOrigin": "human"}), &ev{kind: "prompt", text: "look\n[картинка]"}},
		{fxUserRaw("<bash-input>go test ./...</bash-input>", map[string]any{"turnOrigin": "human"}), &ev{kind: "bash", text: "go test ./..."}},
		{fxUserRaw("<bash-stdout>ok</bash-stdout><bash-stderr>warn</bash-stderr>", nil), &ev{kind: "bashOut", text: "ok", err: "warn"}},
		{fxUserRaw("<command-message>compact</command-message>\n<command-name>/compact</command-name>\n<command-args>keep tests</command-args>", nil), &ev{kind: "prompt", text: "/compact keep tests"}},
		{fxText("**done**"), &ev{kind: "text", text: "**done**"}},
		{fxTool("Read", map[string]any{"file_path": "/x/auth.go"}), &ev{kind: "tool", tool: "Read auth.go"}},
		{fxTurnEnd(), &ev{kind: "turnEnd"}},
		// Left out:
		{fxUserRaw([]any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "secret file"}}, nil), nil},
		{fxUserRaw("<task-notification>agent done</task-notification>", map[string]any{"turnOrigin": "task_notification", "origin": map[string]string{"kind": "task-notification"}}), nil},
		{fxUserRaw("hi from a peer", map[string]any{"turnOrigin": "peer"}), nil},
		{fxUserRaw("meta", map[string]any{"isMeta": true}), nil},
		{fxUserRaw("summary", map[string]any{"isCompactSummary": true, "isVisibleInTranscriptOnly": true}), nil},
		{fxUserRaw("<system-reminder>x</system-reminder>", nil), nil},
		{fxUserRaw("<local-command-stdout>x</local-command-stdout>", nil), nil},
		{fxAssistant(map[string]any{"type": "thinking", "thinking": "hmm"}, nil), nil},
		{fxAssistant(map[string]any{"type": "text", "text": "sub"}, map[string]any{"isSidechain": true}), nil},
		{jline(map[string]any{"type": "system", "subtype": "stop_hook_summary"}), nil},
		{jline(map[string]any{"type": "system", "subtype": "compact_boundary"}), nil},
		{jline(map[string]any{"type": "last-prompt", "lastPrompt": "x"}), nil},
		{`{"type":"user","message":`, nil},
	}
	for _, c := range cases {
		got := parseEntry([]byte(c.line))
		if c.want == nil {
			if got != nil {
				t.Errorf("%.80s: got %+v, want nothing", c.line, got)
			}
			continue
		}
		if got == nil || got.kind != c.want.kind || got.text != c.want.text || got.err != c.want.err || got.tool != c.want.tool {
			t.Errorf("%.80s: got %+v, want %+v", c.line, got, c.want)
		}
	}
}

func TestToolItem(t *testing.T) {
	raw := func(m map[string]any) json.RawMessage { b, _ := json.Marshal(m); return b }
	for _, c := range []struct {
		name string
		in   map[string]any
		want string
	}{
		{"Edit", map[string]any{"file_path": "/a/b/auth.go"}, "Edit auth.go"},
		{"Bash", map[string]any{"command": "go test ./... -run TestVeryLongNameThatGoesOnAndOn\nsecond line"}, "Bash: go test ./... -run TestVeryLongNameThat…"},
		{"Grep", map[string]any{"pattern": "func main"}, "Grep func main"},
		{"WebFetch", map[string]any{"url": "https://docs.example.com/x?y"}, "WebFetch docs.example.com"},
		{"Agent", map[string]any{"description": "review the diff", "prompt": "long"}, "🤖 агент: review the diff"},
		{"Task", map[string]any{"description": "explore"}, "🤖 агент: explore"},
		{"mcp__claude-in-chrome__navigate", nil, "navigate"},
		{"ToolSearch", nil, "ToolSearch"},
	} {
		if got := toolItem(c.name, raw(c.in)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

/* ---------- mirror harness ---------- */

type mirrorFix struct {
	h    *botHarness
	path string // transcript
}

// mirrorSetup pairs the bot and maps sidA's left pane to a transcript.
func mirrorSetup(t *testing.T, forum bool, source string, initial ...string) *mirrorFix {
	h := newBotHarness(t)
	if forum {
		h.pair(forumChat, true)
	} else {
		h.pair(privChat, false)
	}
	h.flush()
	h.tg.take()
	tr := filepath.Join(h.home, ".claude", "projects", "-p", "11111111-2222-3333-4444-555555555555.jsonl")
	os.MkdirAll(filepath.Dir(tr), 0o755)
	os.WriteFile(tr, []byte(strings.Join(initial, "\n")+strings.Repeat("\n", min(1, len(initial)))), 0o600)
	os.MkdirAll(h.bot.claudeDir, 0o700)
	os.WriteFile(filepath.Join(h.bot.claudeDir, sidA+".left.json"), []byte(jline(map[string]any{
		"session_id": "11111111-2222-3333-4444-555555555555", "transcript_path": tr, "cwd": "/p", "source": source, "hook_event_name": "SessionStart",
	})), 0o600)
	return &mirrorFix{h, tr}
}

func (f *mirrorFix) add(lines ...string) {
	fh, _ := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0o600)
	fh.WriteString(strings.Join(lines, "\n") + "\n")
	fh.Close()
}

// tick runs one mirror pass and returns the Telegram calls it made.
func (f *mirrorFix) tick() []tgCall {
	f.h.bot.mirrorTick(context.Background())
	return f.h.tg.take()
}

func (f *mirrorFix) wait(d time.Duration) []tgCall {
	f.h.clock = f.h.clock.Add(d)
	return f.tick()
}

func texts(calls []tgCall, method string) []string {
	var out []string
	for _, c := range only(calls, method) {
		out = append(out, str(c.Params["text"]))
	}
	return out
}

func fxTurnEndIn(ms int64) string {
	return jline(map[string]any{"type": "system", "subtype": "turn_duration", "durationMs": ms, "messageCount": 4, "isMeta": false})
}

func TestMirrorForumTurn(t *testing.T) {
	f := mirrorSetup(t, true, "resume", fxPrompt("old history that must not be resent"))
	thread := f.h.bot.st.Topics[sidA].Thread
	if c := f.tick(); len(c) != 0 || !f.h.bot.isMirrored(sidA) {
		t.Fatalf("history dumped or not mirrored: %+v", c)
	}

	// Text followed by a tool is intermediate: "🤖", silent; then "⏳ Работает".
	f.add(fxPrompt("add <tests> & docs"),
		fxText("I'll **check** it."),
		fxTool("Read", map[string]any{"file_path": "/p/auth.go"}),
		fxUserRaw([]any{map[string]any{"type": "tool_result", "tool_use_id": "t", "content": "body"}}, nil),
		fxAssistant(map[string]any{"type": "thinking", "thinking": "secret"}, nil),
		fxTool("Bash", map[string]any{"command": "go test ./..."}))
	calls := f.tick()
	sent := only(calls, "sendMessage")
	got := texts(calls, "sendMessage")
	if len(sent) != 3 || got[0] != "👤 add &lt;tests&gt; &amp; docs" || got[1] != "🤖 I&#39;ll <b>check</b> it." || got[2] != "⏳ Работает · Read auth.go" {
		t.Fatalf("messages %q", got)
	}
	for _, c := range sent {
		if num(c.Params["message_thread_id"]) != thread || c.Params["disable_notification"] != true {
			t.Errorf("not silent in the session's topic: %+v", c.Params)
		}
	}
	// The second tool edits the same line, not sooner than 3 s after the last edit.
	if e := texts(f.wait(time.Second), "editMessageText"); len(e) != 0 {
		t.Fatalf("edited too soon: %q", e)
	}
	if e := texts(f.wait(3*time.Second), "editMessageText"); len(e) != 1 || e[0] != "⏳ Работает · Read auth.go · Bash: go test ./..." {
		t.Fatalf("edit %q", e)
	}
	// Text before the turn end is final: held until the end, however long.
	f.add(fxTool("Edit", map[string]any{"file_path": "/p/auth.go"}), fxText("First part."), fxText("**Done**."))
	if s := texts(f.wait(time.Minute), "sendMessage"); len(s) != 0 {
		t.Fatalf("final text sent before the turn end: %q", s)
	}
	// Turn end: the tool line's total first, then ✅ as the last message.
	f.add(fxTurnEndIn(134_400))
	calls = f.tick()
	var order []string
	for _, c := range calls {
		if c.Method == "sendMessage" || c.Method == "editMessageText" {
			order = append(order, c.Method+":"+str(c.Params["text"]))
		}
	}
	want := []string{
		"editMessageText:🔧 3 действия: Read auth.go · Bash: go test ./... · Edit auth.go",
		"sendMessage:🏁 Полностью готово · 2 мин 14 с · 3 действия\nFirst part.\n\n<b>Done</b>.",
	}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("turn end\n got %q\nwant %q", order, want)
	}
	// A new turn starts a new tool line.
	f.add(fxPrompt("next"), fxTool("Grep", map[string]any{"pattern": "TODO"}))
	if s := texts(f.tick(), "sendMessage"); len(s) != 2 || s[1] != "⏳ Работает · Grep TODO" {
		t.Fatalf("new turn %q", s)
	}
}

func TestMirrorTurnEndings(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	f.tick()
	f.h.bot.lastSrc[sidA] = "tg" // loud allowed for the ✅
	// A turn without any text: a short ✅.
	f.add(fxPrompt("p"), fxTool("Read", map[string]any{"file_path": "/a"}), fxTurnEndIn(5_000))
	sent := only(f.tick(), "sendMessage")
	if len(sent) != 3 || str(sent[2].Params["text"]) != "🏁 Полностью готово · 5 с · 1 действие" || sent[2].Params["disable_notification"] != false {
		t.Fatalf("no-text turn: %+v", sent)
	}
	// Interrupted (a new prompt, no turn end): held text goes out as 🤖, no ✅.
	f.add(fxPrompt("q"), fxText("half an answer"), fxPrompt("stop, do this instead"))
	s := texts(f.tick(), "sendMessage")
	if len(s) != 3 || s[1] != "🤖 half an answer" || strings.Contains(strings.Join(s, ""), "✅") {
		t.Fatalf("interrupt: %q", s)
	}
	// A turn end that never comes: after 10 minutes the text goes out as 🤖.
	f.add(fxText("waiting forever"))
	f.tick()
	if s := texts(f.wait(9*time.Minute), "sendMessage"); len(s) != 0 {
		t.Fatalf("too early: %q", s)
	}
	if s := texts(f.wait(2*time.Minute), "sendMessage"); len(s) != 1 || s[0] != "🤖 waiting forever" {
		t.Fatalf("hold timeout: %q", s)
	}
	// A long final text: earlier parts silent, ✅ on the last (and loud).
	f.add(fxPrompt("long"), fxText(strings.Repeat("слово ", 1200)), fxTurnEndIn(3_700_000))
	sent = only(f.tick(), "sendMessage")
	last := sent[len(sent)-1]
	if len(sent) < 3 || !strings.HasPrefix(str(last.Params["text"]), "🏁 Полностью готово · 1 ч 1 мин\n") || last.Params["disable_notification"] != false || sent[1].Params["disable_notification"] != true {
		t.Fatalf("long final: %d messages, last %.60q", len(sent), str(last.Params["text"]))
	}
	for _, m := range sent {
		validTelegramHTML(t, str(m.Params["text"]))
	}
}

func TestFmtDur(t *testing.T) {
	for ms, want := range map[int64]string{0: "0 с", 1_400: "1 с", 59_400: "59 с", 60_000: "1 мин", 134_400: "2 мин 14 с", 3_600_000: "1 ч 0 мин", 3_725_000: "1 ч 2 мин"} {
		if got := fmtDur(ms); got != want {
			t.Errorf("fmtDur(%d) = %q, want %q", ms, got, want)
		}
	}
}

func TestMirrorBashAndSlash(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	f.add(fxUserRaw("<bash-input>ls <dir></bash-input>", map[string]any{"turnOrigin": "human"}),
		fxUserRaw("<bash-stdout>"+strings.Repeat("x", 2000)+"</bash-stdout><bash-stderr>oops</bash-stderr>", nil),
		fxUserRaw("<command-message>compact</command-message>\n<command-name>/compact</command-name>\n<command-args></command-args>", nil),
		fxUserRaw("<local-command-stdout>Compacted</local-command-stdout>", nil))
	s := texts(f.tick(), "sendMessage")
	if len(s) != 2 || !strings.HasPrefix(s[0], "💻 <code>ls &lt;dir&gt;</code>\n<pre>…xxx") || !strings.HasSuffix(s[0], "\noops</pre>") || len([]rune(s[0])) > 1600 || s[1] != "👤 /compact" {
		t.Fatalf("got %q", s)
	}
}

func TestMirrorEchoAndSound(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	idle := int64(time.Minute / time.Millisecond)
	f.h.ag.idle = &idle
	f.h.ag.setTree(t, fakeSession{sidA, "◐ Fix bug", "blocked"}, fakeSession{sidB, "shell", "idle"})
	f.h.bot.srv.ctl.Tree(context.Background())
	thread := f.h.bot.st.Topics[sidA].Thread

	// Text sent from Telegram is typed into the session and not echoed back.
	m := f.h.msg(forumChat, userID, "run   the\ntests")
	m.IsTopicMessage, m.MessageThreadID = true, thread
	f.h.send(m)
	f.h.ag.typed()
	f.add(fxPrompt("run the tests"), fxText("On it."), fxTurnEndIn(2_000))
	calls := f.tick()
	sent := only(calls, "sendMessage")
	if len(sent) != 1 || str(sent[0].Params["text"]) != "🏁 Полностью готово · 2 с\nOn it." {
		t.Fatalf("echoed or lost: %q", texts(calls, "sendMessage"))
	}
	// Last prompt came from Telegram: the ✅ rings even though the user is at the Mac.
	if sent[0].Params["disable_notification"] != false {
		t.Error("reply to a Telegram prompt was silent")
	}
	// A prompt typed at the Mac, user active: everything quiet, ✅ included.
	f.add(fxPrompt("typed at the mac"), fxText("Sure."), fxTurnEndIn(1_000))
	sent = only(f.tick(), "sendMessage")
	if len(sent) != 2 || sent[0].Params["disable_notification"] != true || sent[1].Params["disable_notification"] != true {
		t.Fatalf("at the Mac should be quiet: %+v", sent)
	}
	// The same words typed at the Mac later are not taken for an echo.
	f.add(fxPrompt("run the tests"))
	if s := texts(f.tick(), "sendMessage"); len(s) != 1 || s[0] != "👤 run the tests" {
		t.Fatalf("second identical prompt: %q", s)
	}
}

func TestSoundPolicyMatrix(t *testing.T) {
	h := newBotHarness(t)
	h.pair(privChat, false)
	os.WriteFile(h.bot.settingsPath, []byte(`{"loudAfterIdleMinutes": 5}`), 0o600)
	for _, c := range []struct {
		idle   time.Duration
		src    string
		muted  bool
		silent bool
	}{
		{time.Minute, "mac", false, true},
		{time.Minute, "", false, true},
		{time.Minute, "tg", false, false},
		{10 * time.Minute, "mac", false, false},
		{10 * time.Minute, "tg", false, false},
		{10 * time.Minute, "tg", true, true},
		{time.Minute, "tg", true, true},
	} {
		ms := int64(c.idle / time.Millisecond)
		h.ag.idle = &ms
		h.ag.setTree(t, fakeSession{sidA, "a", "idle"})
		h.bot.srv.ctl.Tree(context.Background())
		h.bot.lastSrc[sidA] = c.src
		h.bot.st.MutedUntil = time.Time{}
		if c.muted {
			h.bot.st.MutedUntil = h.clock.Add(time.Hour)
		}
		if got := h.bot.silentFor(context.Background(), sidA); got != c.silent {
			t.Errorf("%+v: silent=%v", c, got)
		}
	}
	if d := h.bot.loudAfter(); d != 5*time.Minute {
		t.Errorf("setting: %v", d)
	}
	os.WriteFile(h.bot.settingsPath, []byte(`nonsense`), 0o600)
	if d := h.bot.loudAfter(); d != defaultLoudAfter {
		t.Errorf("default: %v", d)
	}
}

func TestMirrorOffsetsSurviveRestart(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	f.add(fxPrompt("one"))
	if s := texts(f.tick(), "sendMessage"); len(s) != 1 {
		t.Fatal(s)
	}
	// A partial last line waits until it is complete.
	fh, _ := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0o600)
	line := fxPrompt("two")
	fh.WriteString(line[:20])
	fh.Close()
	if s := texts(f.tick(), "sendMessage"); len(s) != 0 {
		t.Fatalf("partial line sent: %q", s)
	}
	saved, _ := loadTGState(f.h.bot.statePath)
	if saved.Follow[f.path] == nil || saved.Follow[f.path].Off == 0 {
		t.Fatal("offset not saved")
	}
	// Restart: a new bot from the same state continues where it was.
	b2, err := newTGBot(f.h.bot.api, f.h.bot.srv, filepath.Join(f.h.dir, "telegram-token"))
	if err != nil {
		t.Fatal(err)
	}
	b2.now, b2.after, b2.out = f.h.bot.now, f.h.bot.after, f.h.bot.out
	b2.claudeDir, b2.projectsDir, b2.settingsPath, b2.home = f.h.bot.claudeDir, f.h.bot.projectsDir, f.h.bot.settingsPath, f.h.bot.home
	f.h.bot = b2
	fh, _ = os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0o600)
	fh.WriteString(line[20:] + "\n")
	fh.Close()
	if s := texts(f.tick(), "sendMessage"); len(s) != 1 || s[0] != "👤 two" {
		t.Fatalf("after restart: %q", s)
	}
}

func TestMirrorFallbackAndAmbiguity(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	cwd := filepath.Join(h.home, "proj")
	h.ag.cwd[sidA], h.ag.fg[sidA] = cwd, []string{"claude"}
	h.ag.cwd[sidB], h.ag.fg[sidB] = filepath.Join(h.home, "other"), []string{"claude", "--resume"}
	h.ag.cwd[sidC], h.ag.fg[sidC] = filepath.Join(h.home, "other"), []string{"claude"}
	h.ag.setTree(t, fakeSession{sidA, "A", "active"}, fakeSession{sidB, "B", "active"}, fakeSession{sidC, "C", "active"})
	dir := projectDir(h.bot.projectsDir, cwd)
	os.MkdirAll(dir, 0o755)
	old := filepath.Join(dir, "old.jsonl")
	os.WriteFile(old, []byte(fxPrompt("x")+"\n"), 0o600)
	os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	if want := "-" + strings.TrimPrefix(regexpNotAlnum(cwd), "-"); filepath.Base(dir) != want {
		t.Fatalf("project dir %s, want %s", filepath.Base(dir), want)
	}
	h.bot.mirrorTick(context.Background())
	if h.bot.isMirrored(sidA) {
		t.Fatal("a transcript idle for an hour was taken")
	}
	cur := filepath.Join(dir, "cur.jsonl")
	withCwd := func(c string) string {
		return jline(map[string]any{"type": "user", "cwd": c, "turnOrigin": "human", "message": map[string]any{"content": "history"}})
	}
	os.WriteFile(cur, []byte(withCwd(cwd)+"\n"), 0o600)
	// Same project folder, another cwd (~/proj vs ~/proj_ would collide too): ignored.
	os.WriteFile(filepath.Join(dir, "decoy.jsonl"), []byte(withCwd(cwd+"/sub")+"\n"), 0o600)
	h.clock = h.clock.Add(mirrorMapEvery)
	h.bot.mirrorTick(context.Background())
	if !h.bot.isMirrored(sidA) || h.bot.isMirrored(sidB) || h.bot.isMirrored(sidC) {
		t.Fatalf("mirrored: %v", h.bot.mirrored)
	}
	if un := h.bot.unmirrored; len(un) != 2 || un[0] != "B" || un[1] != "C" {
		t.Errorf("unmirrored %v", un)
	}
	if s := h.bot.statusText(context.Background()); !strings.Contains(s, "Зеркало: 1 сессия; не зеркалится: B, C (перезапусти сессию)") {
		t.Errorf("status: %s", s)
	}
	h.tg.take()
	f := &mirrorFix{h, cur}
	if s := texts(f.tick(), "sendMessage"); len(s) != 0 {
		t.Fatalf("fallback dumped history: %q", s)
	}
	f.add(fxPrompt("new"))
	if s := texts(f.tick(), "sendMessage"); len(s) != 1 || s[0] != "👤 new" {
		t.Fatalf("fallback: %q", s)
	}
	// Once the hook has written a mapping, it wins over the fallback.
	other := filepath.Join(dir, "hooked.jsonl")
	os.WriteFile(other, nil, 0o600)
	os.MkdirAll(h.bot.claudeDir, 0o700)
	os.WriteFile(filepath.Join(h.bot.claudeDir, strings.ToLower(sidA)+".left.json"), []byte(jline(map[string]any{"transcript_path": other, "source": "clear"})), 0o600)
	h.clock = h.clock.Add(mirrorMapEvery)
	h.bot.mirrorTick(context.Background())
	if _, ok := h.bot.mirrors[other]; !ok || len(h.bot.mirrors) != 1 {
		t.Errorf("mapping not preferred: %v", h.bot.mirrors)
	}
}

func TestMirrorDoneNotificationSkipped(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	f.tick()
	f.h.bot.onStatus(winA, sidA, "completed", "")
	f.h.bot.onStatus(winA, sidB, "completed", "")
	f.h.bot.onStatus(winA, sidA, "blocked", "")
	f.h.flush()
	var got []string
	for _, c := range only(f.h.tg.take(), "sendMessage") {
		got = append(got, strings.SplitN(str(c.Params["text"]), "\n", 2)[0]+"@"+fmtInt(num(c.Params["message_thread_id"])))
	}
	want := []string{"✅ Готово@" + fmtInt(f.h.bot.st.Topics[sidB].Thread), "🟠 Ждёт тебя@" + fmtInt(f.h.bot.st.Topics[sidA].Thread)}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMirrorPrivateMode(t *testing.T) {
	f := mirrorSetup(t, false, "startup")
	f.add(fxPrompt("hello"))
	sent := only(f.tick(), "sendMessage")
	if len(sent) != 1 || str(sent[0].Params["text"]) != "<b>Fix bug</b>\n👤 hello" || sent[0].Params["message_thread_id"] != nil {
		t.Fatalf("private: %+v", sent)
	}
	// A reply to a mirrored message goes into its session.
	ref := f.h.bot.st.MsgRefs[len(f.h.bot.st.MsgRefs)-1]
	m := f.h.msg(privChat, userID, "and more")
	m.ReplyTo = &tgMessage{MessageID: ref.ID}
	f.h.send(m)
	if typed := f.h.ag.typed(); len(typed) != 2 || typed[0] != sidA+"=and more" {
		t.Errorf("reply routing: %q", typed)
	}
}

func TestMirrorTransientRetryAndEditMerge(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	f.tick()
	fail := true
	f.h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "sendMessage" && fail {
			return 502, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`, true
		}
		return 0, "", false
	}
	f.add(fxPrompt("p1"), fxPrompt("p2"))
	f.tick()
	if len(f.h.bot.mirrorQ) != 2 {
		t.Fatalf("queue after a 502: %d", len(f.h.bot.mirrorQ))
	}
	fail = false
	if s := texts(f.tick(), "sendMessage"); len(s) != 2 || s[0] != "👤 p1" || s[1] != "👤 p2" {
		t.Fatalf("order after retry: %q", s)
	}
	// Pending edits of the same message merge into the newest.
	f.add(fxTool("Read", map[string]any{"file_path": "/a"}))
	f.tick()
	tm := f.h.bot.mirrors[f.path]
	tm.Tools = append(tm.Tools, "X")
	tm.ToolN++
	f.h.bot.queueToolEdit(tm, false)
	tm.Tools = append(tm.Tools, "Y")
	tm.ToolN++
	f.h.bot.queueToolEdit(tm, false)
	if e := texts(f.tick(), "editMessageText"); len(e) != 1 || e[0] != "⏳ Работает · Read a · X · Y" {
		t.Fatalf("merged edit %q", e)
	}
}

func TestOutboxBulkYieldsToPriority(t *testing.T) {
	f := newFakeTG(t)
	o := newTGOutbox(newTGClient(f.srv.URL, testTGToken))
	o.perChat = 0
	release := make(chan struct{})
	entered := make(chan struct{})
	var order []string
	f.hook = func(m string, p map[string]any) (int, string, bool) {
		order = append(order, str(p["text"]))
		if str(p["text"]) == "prio" {
			close(entered)
			<-release
		}
		return 0, "", false
	}
	done := make(chan struct{})
	go func() {
		o.call(context.Background(), privChat, "sendMessage", map[string]any{"text": "prio"}, nil)
		close(done)
	}()
	<-entered
	bulk := make(chan struct{})
	go func() {
		o.callBulk(context.Background(), privChat, "sendMessage", map[string]any{"text": "bulk"}, nil)
		close(bulk)
	}()
	select {
	case <-bulk:
		t.Fatal("bulk went out while a priority call was in flight")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	<-done
	<-bulk
	if strings.Join(order, ",") != "prio,bulk" {
		t.Errorf("order %v", order)
	}
}

func regexpNotAlnum(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func TestMirrorBackgroundTasks(t *testing.T) {
	f := mirrorSetup(t, true, "startup")
	f.tick()
	f.h.bot.lastSrc[sidA] = "tg"
	launchBash := fxUserRaw([]any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_bg1", "content": "Command running in background with ID: bx1"}},
		map[string]any{"toolUseResult": map[string]any{"stdout": "", "backgroundTaskId": "bx1"}})
	launchAgent := fxUserRaw([]any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_ag1", "content": "Async agent launched"}},
		map[string]any{"toolUseResult": map[string]any{"isAsync": true, "status": "async_launched", "agentId": "a77"}})
	// A turn that leaves two tasks running: silent "✅ Ответ готов · в фоне ещё 2".
	f.add(fxPrompt("audit it"), fxTool("Bash", map[string]any{"command": "codex"}), launchBash,
		fxTool("Agent", map[string]any{"description": "review"}), launchAgent, fxText("Started."), fxTurnEndIn(4_000))
	sent := only(f.tick(), "sendMessage")
	last := sent[len(sent)-1]
	if str(last.Params["text"]) != "✅ Ответ готов · в фоне ещё 2 · 4 с · 2 действия\nStarted." || last.Params["disable_notification"] != true {
		t.Fatalf("bg turn: %q %v", str(last.Params["text"]), last.Params["disable_notification"])
	}
	// The bash task ends while Claude is busy elsewhere (queued attachment), the
	// notice turn finishes with the agent still running: still silent.
	f.add(jline(map[string]any{"type": "attachment", "attachment": map[string]any{"type": "queued_command", "commandMode": "task-notification",
		"prompt": "<task-notification>\n<task-id>bx1</task-id>\n<tool-use-id>toolu_bg1</tool-use-id>\n<status>failed</status>\n</task-notification>"}}),
		fxText("Codex failed, retrying later."), fxTurnEndIn(1_000))
	sent = only(f.tick(), "sendMessage")
	if s := str(sent[len(sent)-1].Params["text"]); !strings.HasPrefix(s, "✅ Ответ готов · в фоне ещё 1 · 1 с\n") {
		t.Fatalf("one left: %q", s)
	}
	// The agent hands back: its notice turn is the real end — 🏁 and loud.
	f.add(fxUserRaw("<agent-message from=\"a77\" kind=\"handback\">\n[Subagent hand-back] all good</agent-message>", map[string]any{"turnOrigin": "peer", "origin": map[string]string{"kind": "peer"}}),
		fxText("All done."), fxTurnEndIn(2_000))
	sent = only(f.tick(), "sendMessage")
	last = sent[len(sent)-1]
	if str(last.Params["text"]) != "🏁 Полностью готово · 2 с\nAll done." || last.Params["disable_notification"] != false {
		t.Fatalf("fully done: %q %v", str(last.Params["text"]), last.Params["disable_notification"])
	}
}

func TestParseBackgroundNotices(t *testing.T) {
	for _, c := range []struct {
		line string
		kind string
		id   string
	}{
		{jline(map[string]any{"type": "queue-operation", "operation": "enqueue", "content": "<task-notification>\n<task-id>b1</task-id>\n<tool-use-id>toolu_x</tool-use-id>\n<status>completed</status>\n</task-notification>"}), "bgEnd", "toolu_x"},
		{jline(map[string]any{"type": "queue-operation", "operation": "remove", "content": "<task-notification><task-id>b1</task-id><status>completed</status></task-notification>"}), "", ""},
		{jline(map[string]any{"type": "attachment", "attachment": map[string]any{"type": "task_status", "taskId": "b2", "status": "killed"}}), "bgEnd", "b2"},
		{jline(map[string]any{"type": "attachment", "attachment": map[string]any{"type": "task_status", "taskId": "b2", "status": "running"}}), "", ""},
		{fxUserRaw("<task-notification><task-id>b3</task-id><status>completed</status></task-notification>", map[string]any{"turnOrigin": "task_notification"}), "bgEnd", "b3"},
		{fxTool("TaskStop", map[string]any{"task_id": "b4"}), "tool", "b4"},
		{fxUserRaw("<task-notification>\n<task-id>bm1</task-id>\n<summary>Monitor event: \"deploy\"</summary>\n<event>server: f855</event>\n</task-notification>", map[string]any{"turnOrigin": "task_notification"}), "", ""},
		{fxUserRaw("<task-notification>\n<task-id>bm1</task-id>\n<tool-use-id>toolu_m</tool-use-id>\n<status>completed</status>\n<summary>Monitor \"deploy\" stream ended</summary>\n</task-notification>", map[string]any{"turnOrigin": "task_notification"}), "bgEnd", "toolu_m"},
		{fxUserRaw("<task-notification>\n<task-id>bm1</task-id>\n<summary>Monitor event: \"deploy\"</summary>\n<event>[Monitor expired after 30m with 2 events delivered.]</event>\n</task-notification>", map[string]any{"turnOrigin": "task_notification"}), "bgEnd", "bm1"},
		{fxUserRaw([]any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_m", "content": "Monitor started"}}, map[string]any{"toolUseResult": map[string]any{"taskId": "bm1", "timeoutMs": 1200000, "persistent": false}}), "bgStart", "toolu_m"},
		{fxUserRaw([]any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_p", "content": "Monitor started"}}, map[string]any{"toolUseResult": map[string]any{"taskId": "bm2", "timeoutMs": 0, "persistent": true}}), "", ""},
	} {
		ev := parseEntry([]byte(c.line))
		if c.kind == "" {
			if ev != nil {
				t.Errorf("%.80s: got %+v", c.line, ev)
			}
			continue
		}
		if ev == nil || ev.kind != c.kind || ev.id != c.id {
			t.Errorf("%.80s: got %+v, want %s %s", c.line, ev, c.kind, c.id)
		}
	}
}
