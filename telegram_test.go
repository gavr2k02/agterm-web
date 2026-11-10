package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testTGToken = "123456:SECRET-token_value-abcdefghijklmnop"
	sidA        = "DDDDDDDD-4444-4444-4444-444444444444"
	sidB        = "EEEEEEEE-5555-5555-5555-555555555555"
	sidC        = "FFFFFFFF-6666-6666-6666-666666666666"
	winA        = "AAAAAAAA-1111-1111-1111-111111111111"
	userID      = 4242
	privChat    = 4242
	forumChat   = -1001234567890
)

/* ---------- fake Telegram ---------- */

type tgCall struct {
	Method string
	Params map[string]any
}

type fakeTG struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []tgCall
	nextID int64
	// hook may answer a call itself (status code, body); ok=false falls through.
	hook func(method string, p map[string]any) (code int, body string, ok bool)
	// files served at /file/bot<token>/<path>; getFile knows "docs/<file_id>".
	files map[string][]byte
}

func newFakeTG(t *testing.T) *fakeTG {
	f := &fakeTG{nextID: 100, files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := strings.CutPrefix(r.URL.Path, "/file/bot"+testTGToken+"/"); ok {
			f.mu.Lock()
			body, found := f.files[p]
			f.mu.Unlock()
			if !found {
				w.WriteHeader(404)
				return
			}
			w.Write(body)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/bot"+testTGToken+"/") {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
			return
		}
		method := path.Base(r.URL.Path)
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		f.mu.Lock()
		f.calls = append(f.calls, tgCall{method, p})
		hook := f.hook
		f.nextID++
		id := f.nextID
		f.mu.Unlock()
		if hook != nil {
			if code, body, ok := hook(method, p); ok {
				w.WriteHeader(code)
				fmt.Fprint(w, body)
				return
			}
		}
		switch method {
		case "sendMessage":
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"chat":{"id":%d}}}`, id, num(p["chat_id"]))
		case "createForumTopic":
			fmt.Fprintf(w, `{"ok":true,"result":{"message_thread_id":%d,"name":%q}}`, id, p["name"])
		case "getUpdates":
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		case "getFile":
			f.mu.Lock()
			body, found := f.files["docs/"+str(p["file_id"])]
			f.mu.Unlock()
			if !found {
				fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: invalid file_id"}`)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"result":{"file_id":%q,"file_path":"docs/%s","file_size":%d}}`, str(p["file_id"]), str(p["file_id"]), len(body))
		default:
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTG) take() []tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

func only(calls []tgCall, method string) []tgCall {
	var out []tgCall
	for _, c := range calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func str(v any) string { s, _ := v.(string); return s }

func markup(c tgCall) string {
	b, _ := json.Marshal(c.Params["reply_markup"])
	return string(b)
}

/* ---------- fake agterm ---------- */

type fakeAgterm struct {
	dir  string
	path string
	cwd  map[string]string   // session id -> cwd reported in the tree
	fg   map[string][]string // session id -> foreground argv; none = a shell at its prompt
	idle *int64              // tree idleMs, when set
}

func newFakeAgterm(t *testing.T) *fakeAgterm {
	dir := t.TempDir()
	script := `#!/bin/sh
d="` + dir + `"
printf '%s\n' "$*" >> "$d/args"
case "$1 $2" in
  "window list") echo '{"ok":true,"result":{"windows":[{"id":"` + winA + `","name":"w","open":true}]}}' ;;
  "tree --window") printf '{"ok":true,"result":{"tree":'; cat "$d/tree.json"; echo '}}' ;;
  "session text") printf '{"ok":true,"result":{"text":'; cat "$d/text.json"; echo '}}' ;;
  "session new")
    printf '%s\n' "$@" > "$d/new.args"
    [ -f "$d/new-fails" ] && { echo '{"ok":false,"error":"no such workspace"}'; exit 1; }
    cp "$d/tree-after-new.json" "$d/tree.json"
    echo '{"ok":true,"result":{"id":"'"$(cat "$d/newid")"'"}}' ;;
  "session type")
    t=""; prev=""
    for a in "$@"; do [ "$prev" = "--target" ] && t="$a"; prev="$a"; done
    [ -f "$d/type-fails" ] && { echo '{"ok":false,"error":"no such session"}'; exit 1; }
    printf '%s=' "$t" >> "$d/typed"; cat >> "$d/typed"; printf '\n' >> "$d/typed"
    echo '{"ok":true}' ;;
  *) echo '{"ok":false,"error":"unexpected"}'; exit 1 ;;
esac
`
	p := filepath.Join(dir, "agtermctl")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &fakeAgterm{dir: dir, path: p, cwd: map[string]string{}, fg: map[string][]string{}}
	a.setText("")
	return a
}

type fakeSession struct{ id, name, status string }

func (a *fakeAgterm) setTree(t *testing.T, ss ...fakeSession) { a.setTreeWS(t, "EM", ss...) }

func (a *fakeAgterm) setTreeWS(t *testing.T, ws string, ss ...fakeSession) {
	var sessions []map[string]any
	for _, s := range ss {
		row := map[string]any{"id": s.id, "name": s.name, "status": s.status, "realized": true, "cwd": a.cwd[s.id]}
		if fg, ok := a.fg[s.id]; ok {
			row["foreground"] = fg
		} else {
			row["foregroundShell"] = "zsh"
		}
		sessions = append(sessions, row)
	}
	tree := map[string]any{"workspaces": []any{map[string]any{"id": "W1", "name": ws, "sessions": sessions}}}
	if a.idle != nil {
		tree["idleMs"] = *a.idle
	}
	b, _ := json.Marshal(tree)
	if err := os.WriteFile(filepath.Join(a.dir, "tree.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// afterNew sets the id `session new` returns and the tree it leaves behind.
func (a *fakeAgterm) afterNew(t *testing.T, id string, ss ...fakeSession) {
	old, _ := os.ReadFile(filepath.Join(a.dir, "tree.json"))
	a.setTree(t, ss...)
	os.Rename(filepath.Join(a.dir, "tree.json"), filepath.Join(a.dir, "tree-after-new.json"))
	os.WriteFile(filepath.Join(a.dir, "tree.json"), old, 0o644)
	os.WriteFile(filepath.Join(a.dir, "newid"), []byte(id), 0o644)
}

// args returns the logged agtermctl command lines and clears them.
func (a *fakeAgterm) args() string {
	p := filepath.Join(a.dir, "args")
	b, _ := os.ReadFile(p)
	os.Remove(p)
	return string(b)
}

func (a *fakeAgterm) setText(s string) {
	b, _ := json.Marshal(s)
	os.WriteFile(filepath.Join(a.dir, "text.json"), b, 0o644)
}

// typed returns "SESSION=bytes" records and clears them.
func (a *fakeAgterm) typed() []string {
	p := filepath.Join(a.dir, "typed")
	b, _ := os.ReadFile(p)
	os.Remove(p)
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

/* ---------- harness ---------- */

type botHarness struct {
	t        *testing.T
	bot      *tgBot
	tg       *fakeTG
	ag       *fakeAgterm
	clock    time.Time
	pending  []func()
	slept    []time.Duration
	dir      string
	home     string
	cmu      sync.Mutex
	toolsDir string
}

func newBotHarness(t *testing.T) *botHarness {
	h := &botHarness{t: t, tg: newFakeTG(t), ag: newFakeAgterm(t), clock: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), dir: t.TempDir(), home: realTempDir(t)}
	h.ag.setTree(t, fakeSession{sidA, "◐ Fix bug", "blocked"}, fakeSession{sidB, "shell", "idle"})
	srv := &server{ctl: &Ctl{Path: h.ag.path, Timeout: 5 * time.Second}, token: testToken, hub: newHub(h.ag.path), sleep: func(time.Duration) {}}
	api := newTGClient(h.tg.srv.URL, testTGToken)
	api.sleep = func(ctx context.Context, d time.Duration) error { h.slept = append(h.slept, d); return ctx.Err() }
	b, err := newTGBot(api, srv, filepath.Join(h.dir, "telegram-token"))
	if err != nil {
		t.Fatal(err)
	}
	// The bot reads and moves the fake clock under cmu, since some tests run
	// it from other goroutines; tests set h.clock directly only while nothing
	// else runs.
	now := func() time.Time { h.cmu.Lock(); defer h.cmu.Unlock(); return h.clock }
	advance := func(d time.Duration) { h.cmu.Lock(); h.clock = h.clock.Add(d); h.cmu.Unlock() }
	b.now = now
	b.after = func(d time.Duration, f func()) { h.cmu.Lock(); h.pending = append(h.pending, f); h.cmu.Unlock() }
	b.out.perChat, b.out.groupMax = 0, 0 // the limiter has its own test
	b.out.now = now
	b.out.sleep = func(ctx context.Context, d time.Duration) error { // waiting moves the fake clock
		h.cmu.Lock()
		h.slept = append(h.slept, d)
		h.cmu.Unlock()
		advance(d)
		return ctx.Err()
	}
	b.pollSleep = func(ctx context.Context, d time.Duration) error { advance(d); return ctx.Err() }
	b.home = h.home
	b.launcherFile = filepath.Join(h.dir, "launcher.json")
	b.claudeDir = filepath.Join(h.dir, "claude-sessions")
	b.projectsDir = filepath.Join(h.home, ".claude", "projects")
	b.settingsPath = filepath.Join(h.dir, "telegram-settings.json")
	h.bot = b
	return h
}

// flush runs the queued timers (and the ones they queue) once.
func (h *botHarness) flush() {
	h.cmu.Lock()
	p := h.pending
	h.pending = nil
	h.cmu.Unlock()
	for _, f := range p {
		f()
	}
}

func (h *botHarness) msg(chat, user int64, text string) *tgMessage {
	return &tgMessage{MessageID: 1, From: &tgUser{ID: user}, Chat: tgChat{ID: chat, Type: "private"}, Text: text}
}

func (h *botHarness) send(m *tgMessage) []tgCall {
	h.bot.handleUpdate(context.Background(), tgUpdate{Message: m})
	return h.tg.take()
}

func (h *botHarness) pair(chat int64, forum bool) {
	h.t.Helper()
	code, err := newPairCode(h.bot.pairPath, h.clock)
	if err != nil {
		h.t.Fatal(err)
	}
	m := h.msg(chat, userID, "/pair "+code)
	m.Chat.IsForum = forum
	if forum {
		m.Chat.Type = "supergroup"
	}
	calls := h.send(m)
	if !h.bot.st.paired() || len(only(calls, "sendMessage")) != 1 {
		h.t.Fatalf("pairing failed: %+v", calls)
	}
}

/* ---------- tests ---------- */

func TestTGPairing(t *testing.T) {
	h := newBotHarness(t)
	// Nothing pending: silence.
	if calls := h.send(h.msg(privChat, userID, "/pair 123456")); len(calls) != 0 {
		t.Fatalf("answered without a pending code: %+v", calls)
	}

	code, _ := newPairCode(h.bot.pairPath, h.clock)
	st, _ := os.Stat(h.bot.pairPath)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("pair file perm %v", st.Mode().Perm())
	}
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	for i := 0; i < pairAttempts-1; i++ {
		calls := only(h.send(h.msg(privChat, 999, "/pair "+wrong)), "sendMessage")
		if len(calls) != 1 || str(calls[0].Params["text"]) != "Неверный код." {
			t.Fatalf("wrong code %d: %+v", i, calls)
		}
	}
	// The fifth wrong attempt (from anyone) burns the code; the right one no longer works.
	if c := only(h.send(h.msg(privChat, 999, "/pair "+wrong)), "sendMessage"); len(c) != 1 || !strings.Contains(str(c[0].Params["text"]), "сгорел") {
		t.Fatalf("burn: %+v", c)
	}
	if calls := h.send(h.msg(privChat, userID, "/pair@agterm_bot "+code)); len(calls) != 0 || h.bot.st.paired() {
		t.Fatalf("burned code accepted: %+v", calls)
	}

	// Expiry.
	code, _ = newPairCode(h.bot.pairPath, h.clock)
	h.clock = h.clock.Add(pairTTL + time.Second)
	if c := only(h.send(h.msg(privChat, userID, "/pair "+code)), "sendMessage"); len(c) != 1 || !strings.Contains(str(c[0].Params["text"]), "истёк") || h.bot.st.paired() {
		t.Fatalf("expired code: %+v", c)
	}

	// Success, single use.
	code, _ = newPairCode(h.bot.pairPath, h.clock)
	calls := only(h.send(h.msg(privChat, userID, "/pair@agterm_bot "+code)), "sendMessage")
	if len(calls) != 1 || !strings.Contains(markup(calls[0]), btnSessions) || h.bot.st.ChatID != privChat || h.bot.st.UserID != userID || h.bot.st.Mode != "private" {
		t.Fatalf("pair: %+v %+v", calls, h.bot.st)
	}
	if _, err := os.Stat(h.bot.pairPath); !os.IsNotExist(err) {
		t.Error("used code left on disk")
	}
	saved, err := loadTGState(h.bot.statePath)
	if err != nil || saved.ChatID != privChat || saved.UserID != userID {
		t.Errorf("state not saved: %+v %v", saved, err)
	}
	if st, _ := os.Stat(h.bot.statePath); st.Mode().Perm() != 0o600 {
		t.Errorf("state perm %v", st.Mode().Perm())
	}
	if calls := h.send(h.msg(privChat, 777, "/pair "+code)); len(calls) != 0 {
		t.Errorf("code reused: %+v", calls)
	}
}

func TestTGIgnoresStrangers(t *testing.T) {
	h := newBotHarness(t)
	h.pair(privChat, false)
	h.bot.st.Current = sidA
	for _, m := range []*tgMessage{
		h.msg(privChat, 999, "rm -rf /"),        // someone else in the chat
		h.msg(555, userID, "hello"),             // the user, but another chat
		{Chat: tgChat{ID: privChat}, Text: "x"}, // no sender (channel post)
	} {
		if calls := h.send(m); len(calls) != 0 {
			t.Errorf("answered a stranger: %+v", calls)
		}
	}
	h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: "q", From: tgUser{ID: 999}, Message: h.msg(privChat, 0, ""), Data: "k|" + sidA + "|1"}})
	if calls := h.tg.take(); len(calls) != 0 {
		t.Errorf("stranger's button answered: %+v", calls)
	}
	if typed := h.ag.typed(); len(typed) != 0 {
		t.Errorf("stranger typed: %q", typed)
	}
}

func TestTGForumTopics(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	if h.bot.st.Mode != "forum" {
		t.Fatal(h.bot.st.Mode)
	}
	h.flush() // the sync scheduled by pairing
	created := sessionTopics(only(h.tg.take(), "createForumTopic"))
	if len(created) != 2 || str(created[0].Params["name"]) != "EM · Fix bug" || str(created[1].Params["name"]) != "EM · shell" || num(created[0].Params["chat_id"]) != forumChat {
		t.Fatalf("topics: %+v", created)
	}
	threadA := h.bot.st.Topics[sidA].Thread
	if threadA == 0 || h.bot.st.Topics[sidB] == nil || !h.bot.st.TitlesV2 {
		t.Fatalf("mapping: %+v", h.bot.st.Topics)
	}
	settle := func() []tgCall {
		h.clock = h.clock.Add(4 * time.Second)
		for i := 0; i < 4 && len(h.pending) > 0; i++ {
			h.flush()
		}
		return only(h.tg.take(), "editForumTopic")
	}

	// Status changes and agterm renaming the session do not touch the title.
	h.ag.setTree(t, fakeSession{sidA, "✳ Claude is thinking", "active"}, fakeSession{sidB, "shell", "completed"})
	h.bot.onEvent(agtermEvent{Kind: "status", Session: sidA})
	h.bot.onEvent(agtermEvent{Kind: "tree.changed"})
	if edits := settle(); len(edits) != 0 {
		t.Fatalf("renamed on status/name change: %+v", edits)
	}
	// Moved to another workspace: retitled, keeping the name it was created with.
	h.ag.setTreeWS(t, "DXP", fakeSession{sidA, "✳ Claude is thinking", "active"}, fakeSession{sidB, "shell", "idle"})
	h.bot.onEvent(agtermEvent{Kind: "tree.changed"})
	if edits := settle(); len(edits) != 2 || str(edits[0].Params["name"]) != "DXP · Fix bug" || str(edits[1].Params["name"]) != "DXP · shell" || num(edits[0].Params["message_thread_id"]) != threadA {
		t.Fatalf("workspace move: %+v", edits)
	}
	// Nothing changed: no API call.
	h.bot.scheduleSync()
	h.flush()
	if c := h.tg.take(); len(only(c, "editForumTopic"))+len(only(c, "createForumTopic")) != 0 {
		t.Fatalf("idle sync called the API: %+v", c)
	}

	// New session with a long name -> new topic, the session name shortened.
	long := fakeSession{sidC, strings.Repeat("я", 200), "completed"}
	h.ag.setTreeWS(t, "DXP", fakeSession{sidA, "◐ Fix bug", "active"}, fakeSession{sidB, "shell", "idle"}, long)
	h.bot.onEvent(agtermEvent{Kind: "session.created", Session: sidC})
	h.flush()
	created = only(h.tg.take(), "createForumTopic")
	if len(created) != 1 || len([]rune(str(created[0].Params["name"]))) != tgMaxTopicName ||
		!strings.HasPrefix(str(created[0].Params["name"]), "DXP · яяя") || !strings.HasSuffix(str(created[0].Params["name"]), "я…") {
		t.Fatalf("new topic: %+v", created)
	}

	// Closed session: its topic is deleted, its state dropped.
	h.bot.st.MsgRefs = []tgMsgRef{{ID: 5, Session: sidA}, {ID: 6, Session: sidB}}
	h.bot.onEvent(agtermEvent{Kind: "session.closed", Session: strings.ToLower(sidA)})
	calls := h.tg.take()
	if d := only(calls, "deleteForumTopic"); len(d) != 1 || num(d[0].Params["message_thread_id"]) != threadA || len(only(calls, "editForumTopic")) != 0 {
		t.Fatalf("delete: %+v", calls)
	}
	if h.bot.st.Topics[sidA] != nil || len(h.bot.st.Archived) != 0 || len(h.bot.st.MsgRefs) != 1 || h.bot.st.MsgRefs[0].Session != sidB {
		t.Errorf("state after delete: %+v %+v %+v", h.bot.st.Topics, h.bot.st.Archived, h.bot.st.MsgRefs)
	}
	// A late message in that (deleted) topic is not sent anywhere.
	m := h.msg(forumChat, userID, "still there?")
	m.IsTopicMessage, m.MessageThreadID = true, threadA
	if c := only(h.send(m), "sendMessage"); len(c) != 1 || h.ag.typed() != nil {
		t.Errorf("deleted topic: %+v", c)
	}
	// The same id coming back (undo) gets a new topic.
	h.ag.setTreeWS(t, "DXP", fakeSession{sidA, "◐ Fix bug", "active"}, fakeSession{sidB, "shell", "idle"}, long)
	h.bot.onEvent(agtermEvent{Kind: "session.created", Session: sidA})
	h.flush()
	if c := only(h.tg.take(), "createForumTopic"); len(c) != 1 || h.bot.st.Topics[sidA] == nil || h.bot.st.Topics[sidA].Thread == threadA {
		t.Fatalf("reopened session: %+v", c)
	}

	// A session gone while the bot was not looking is deleted on the next sync;
	// an empty tree (agterm restarting) deletes nothing.
	h.ag.setTree(t)
	h.bot.scheduleSync()
	h.flush()
	if c := only(h.tg.take(), "deleteForumTopic"); len(c) != 0 {
		t.Fatalf("empty tree deleted topics: %+v", c)
	}
	h.ag.setTreeWS(t, "DXP", fakeSession{sidB, "shell", "idle"})
	h.bot.scheduleSync()
	h.flush()
	if c := only(h.tg.take(), "deleteForumTopic"); len(c) != 2 {
		t.Fatalf("vanished sessions not deleted: %+v", c)
	}
}

// Topics titled by an older version ("🟠 EM · Fix bug") lose the emoji once.
func TestTGTitleMigration(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.bot.st.Topics = map[string]*tgTopicRef{
		sidA: {Thread: 11, Title: "🟠 EM · Fix bug", Workspace: "EM", Name: "Fix bug"},
		sidB: {Thread: 12, Title: "EM · shell", Workspace: "EM", Name: "shell"},
	}
	h.bot.st.TitlesV2 = false
	h.tg.take()
	h.bot.syncTopics(context.Background())
	edits := only(h.tg.take(), "editForumTopic")
	if len(edits) != 1 || num(edits[0].Params["message_thread_id"]) != 11 || str(edits[0].Params["name"]) != "EM · Fix bug" || !h.bot.st.TitlesV2 {
		t.Fatalf("migration: %+v", edits)
	}
	saved, _ := loadTGState(h.bot.statePath)
	if !saved.TitlesV2 || saved.Topics[sidA].Title != "EM · Fix bug" {
		t.Errorf("not saved: %+v", saved.Topics[sidA])
	}
	h.bot.syncTopics(context.Background())
	if e := only(h.tg.take(), "editForumTopic"); len(e) != 0 {
		t.Errorf("migrated twice: %+v", e)
	}
}

// The "topic edited" line a bot rename leaves is deleted when Telegram reports it.
func TestTGRenameNoticeDeleted(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	thread := h.bot.st.Topics[sidA].Thread
	h.ag.setTreeWS(t, "DXP", fakeSession{sidA, "◐ Fix bug", "blocked"}, fakeSession{sidB, "shell", "idle"})
	h.clock = h.clock.Add(time.Minute)
	h.bot.syncTopics(context.Background())
	h.tg.take()
	notice := func(from int64, th int64) []tgCall {
		h.bot.handleUpdate(context.Background(), rawUpdate(t, fmt.Sprintf(`{"message":{"message_id":900,"message_thread_id":%d,"is_topic_message":true,"from":{"id":%d,"is_bot":true},"chat":{"id":%d,"is_forum":true},"forum_topic_edited":{"name":"DXP · Fix bug"}}}`, th, from, forumChat)))
		return h.tg.take()
	}
	if d := only(notice(h.bot.botID, thread), "deleteMessage"); len(d) != 1 || num(d[0].Params["message_id"]) != 900 {
		t.Fatalf("not deleted: %+v", d)
	}
	if c := notice(555, thread); len(c) != 0 { // someone else's rename stays
		t.Errorf("deleted a human's rename notice: %+v", c)
	}
	if c := notice(h.bot.botID, 424242); len(c) != 0 { // a topic the bot did not just rename
		t.Errorf("deleted an unrelated notice: %+v", c)
	}
	h.clock = h.clock.Add(2 * time.Minute)
	if c := notice(h.bot.botID, thread); len(c) != 0 {
		t.Errorf("deleted a stale notice: %+v", c)
	}
}

func TestTopicTitle(t *testing.T) {
	if got := topicName(sessionRef{Session{Name: "⠋ Тесты Price IT", Status: "blocked"}, "DXP"}); got != "DXP · Тесты Price IT" {
		t.Error(got)
	}
	if got := topicName(sessionRef{Session{Name: "x", Status: "idle"}, ""}); got != "x" {
		t.Error(got)
	}
	got := archiveName(&tgTopicRef{Workspace: strings.Repeat("W", 300), Name: "n"})
	if len([]rune(got)) != tgMaxTopicName || !strings.HasSuffix(got, "…") {
		t.Errorf("huge workspace: %d %q", len([]rune(got)), got)
	}
}

func TestTGOutboxLimits(t *testing.T) {
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	o := newTGOutbox(nil)
	o.now = func() time.Time { return clock }
	var waits []time.Duration
	take := func(chat int64) time.Duration {
		d := o.reserve(chat)
		waits = append(waits, d)
		return d
	}
	// One chat: a second apart, queued in order.
	if take(5) != 0 || take(5) != time.Second || take(5) != 2*time.Second {
		t.Fatalf("private chat: %v", waits)
	}
	// Another chat is independent.
	if take(6) != 0 {
		t.Fatal("chats share a slot")
	}
	// A group: at most 20 a minute, even with time passing a second per send.
	var last time.Duration
	for i := 0; i < 21; i++ {
		last = take(-100)
		clock = clock.Add(time.Second)
	}
	// Send 21 comes at 20 s and has to wait until a minute after send 1.
	if last != 40*time.Second {
		t.Errorf("21st group send waits %v, want 40s", last)
	}
}

func TestTGForumTextRouting(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	threadA := h.bot.st.Topics[sidA].Thread

	inTopic := func(thread int64, text string) *tgMessage {
		m := h.msg(forumChat, userID, text)
		m.MessageID, m.IsTopicMessage, m.MessageThreadID = 77, true, thread
		return m
	}
	calls := h.send(inTopic(threadA, "fix it"))
	if typed := h.ag.typed(); len(typed) != 2 || typed[0] != sidA+"=fix it" || typed[1] != sidA+"=\r" {
		t.Fatalf("typed %q", typed)
	}
	if r := only(calls, "setMessageReaction"); len(r) != 1 || num(r[0].Params["message_id"]) != 77 || len(only(calls, "sendMessage")) != 0 {
		t.Fatalf("expected only a reaction: %+v", calls)
	}
	// Unknown slash commands are text for the agent.
	h.send(inTopic(threadA, "/compact"))
	if typed := h.ag.typed(); len(typed) != 2 || typed[0] != sidA+"=/compact" {
		t.Errorf("slash command: %q", typed)
	}

	// General topic: a hint, nothing typed.
	if c := only(h.send(h.msg(forumChat, userID, "hello")), "sendMessage"); len(c) != 1 || h.ag.typed() != nil {
		t.Errorf("general: %+v", c)
	}
	// A topic of no session.
	if c := only(h.send(inTopic(9999, "x")), "sendMessage"); len(c) != 1 || num(c[0].Params["message_thread_id"]) != 9999 {
		t.Errorf("foreign topic: %+v", c)
	}
	// Too long.
	if c := only(h.send(inTopic(threadA, strings.Repeat("x", maxTypeLen+1))), "sendMessage"); len(c) != 1 || !strings.Contains(str(c[0].Params["text"]), "16 КБ") || h.ag.typed() != nil {
		t.Errorf("too long: %+v", c)
	}
	// Media goes the same way (media_test.go covers it); a file Telegram
	// cannot give is reported in the topic.
	m := inTopic(threadA, "")
	m.Photo = json.RawMessage(`[{"file_id":"x"}]`)
	if c := only(h.send(m), "sendMessage"); len(c) != 1 || str(c[0].Params["text"]) != "Не удалось скачать файл." || num(c[0].Params["message_thread_id"]) != threadA {
		t.Errorf("photo: %+v", c)
	}
	// Service message (no text, no media): ignored.
	if c := h.send(inTopic(threadA, "")); len(c) != 0 {
		t.Errorf("service message answered: %+v", c)
	}
	// agterm refuses: the error is reported in the topic.
	os.WriteFile(filepath.Join(h.ag.dir, "type-fails"), nil, 0o644)
	if c := only(h.send(inTopic(threadA, "x")), "sendMessage"); len(c) != 1 || !strings.Contains(str(c[0].Params["text"]), "не отправлено: no such session") {
		t.Errorf("failure: %+v", c)
	}
}

func TestTGPrivateRoutingAndNotifications(t *testing.T) {
	h := newBotHarness(t)
	h.pair(privChat, false)
	h.tg.take()

	// No target yet.
	if c := only(h.send(h.msg(privChat, userID, "hi")), "sendMessage"); len(c) != 1 || h.ag.typed() != nil {
		t.Fatalf("no target: %+v", c)
	}

	h.ag.setText("line1\n\n  <b>&\n" + strings.Repeat("\n", 5) + "Do you want to proceed?\n❯ 1. Yes   \n")
	h.bot.onEvent(agtermEvent{Kind: "status", Window: winA, Session: sidA, Payload: struct {
		Name     string `json:"name"`
		Status   string `json:"status"`
		Previous string `json:"previous"`
		Pane     string `json:"pane"`
	}{Status: "blocked", Previous: "active"}})
	if len(h.pending) != 1 {
		t.Fatalf("notification not scheduled: %d", len(h.pending))
	}
	h.flush()
	sent := only(h.tg.take(), "sendMessage")
	if len(sent) != 1 {
		t.Fatalf("sent %+v", sent)
	}
	text := str(sent[0].Params["text"])
	want := "🟠 Ждёт тебя: <b>Fix bug</b>\n<pre>line1\n  &lt;b&gt;&amp;\nDo you want to proceed?\n❯ 1. Yes</pre>"
	if text != want || sent[0].Params["parse_mode"] != "HTML" {
		t.Errorf("text %q\nwant %q", text, want)
	}
	kb := markup(sent[0])
	for _, k := range []string{`"k|` + sidA + `|1|l"`, `"k|` + sidA + `|esc|l"`, `"s|` + sidA + `|l"`, `"m|` + sidA + `|more|l"`} {
		if !strings.Contains(kb, k) {
			t.Errorf("keyboard lacks %s: %s", k, kb)
		}
	}
	notif := h.bot.st.MsgRefs[len(h.bot.st.MsgRefs)-1]

	// A reply to the notification goes into its session.
	m := h.msg(privChat, userID, "yes, go")
	m.ReplyTo = &tgMessage{MessageID: notif.ID}
	c := only(h.send(m), "sendMessage")
	if typed := h.ag.typed(); len(typed) != 2 || typed[0] != sidA+"=yes, go" || len(c) != 1 || str(c[0].Params["text"]) != "→ Fix bug" {
		t.Fatalf("reply routing: %q %+v", typed, c)
	}

	// Pick a current session with the list's buttons, then plain text goes there.
	list := only(h.send(h.msg(privChat, userID, btnSessions)), "sendMessage")
	if len(list) != 1 || !strings.Contains(markup(list[0]), `"c|`+sidB+`"`) || !strings.Contains(str(list[0].Params["text"]), "Fix bug — ждёт тебя") {
		t.Fatalf("list: %+v", list)
	}
	h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: "q1", From: tgUser{ID: userID}, Message: h.msg(privChat, 0, ""), Data: "c|" + sidB}})
	if a := only(h.tg.take(), "answerCallbackQuery"); len(a) != 1 || str(a[0].Params["text"]) != "Пишу в: shell" {
		t.Fatalf("pick: %+v", a)
	}
	h.send(h.msg(privChat, userID, "ls"))
	if typed := h.ag.typed(); len(typed) != 2 || typed[0] != sidB+"=ls" {
		t.Fatalf("current: %q", typed)
	}

	// Completed: short screen, only the refresh button.
	h.ag.setText(strings.Repeat("x\n", 30))
	h.bot.onStatus(winA, sidA, "completed", "")
	h.flush()
	done := only(h.tg.take(), "sendMessage")
	if len(done) != 1 || !strings.HasPrefix(str(done[0].Params["text"]), "✅ Готово: <b>Fix bug</b>") ||
		strings.Count(str(done[0].Params["text"]), "x\n")+1 != 10 || strings.Contains(markup(done[0]), "k|") {
		t.Errorf("completed: %+v", done)
	}
}

func TestTGNotifyDebounceAndMute(t *testing.T) {
	h := newBotHarness(t)
	h.pair(privChat, false)
	h.tg.take()
	blocked := func() int {
		h.bot.onStatus(winA, sidA, "blocked", "")
		n := len(h.pending)
		h.flush()
		h.tg.take()
		return n
	}
	if blocked() != 1 {
		t.Fatal("first")
	}
	h.clock = h.clock.Add(5 * time.Second)
	if blocked() != 0 {
		t.Error("not debounced")
	}
	h.clock = h.clock.Add(6 * time.Second)
	if blocked() != 1 {
		t.Error("debounced too long")
	}

	// "🔕 Тишина": still delivered, but without sound; then loud again.
	silent := func() bool {
		h.bot.onStatus(winA, sidA, "blocked", "")
		h.flush()
		c := only(h.tg.take(), "sendMessage")
		if len(c) != 1 {
			t.Fatalf("notifications: %+v", c)
		}
		return c[0].Params["disable_notification"] == true
	}
	h.send(h.msg(privChat, userID, btnMute))
	h.clock = h.clock.Add(time.Minute)
	if !silent() {
		t.Error("muted but loud")
	}
	h.send(h.msg(privChat, userID, btnUnmute))
	h.clock = h.clock.Add(time.Minute)
	if silent() {
		t.Error("unmuted but silent")
	}
	// Mute ends by itself after an hour.
	h.send(h.msg(privChat, userID, "/mute"))
	h.clock = h.clock.Add(muteFor + time.Second)
	if silent() {
		t.Error("mute did not expire")
	}
	// Not paired: nothing.
	h2 := newBotHarness(t)
	h2.bot.onStatus(winA, sidA, "blocked", "")
	if len(h2.pending) != 0 {
		t.Error("notified before pairing")
	}
}

func TestTGCallbacks(t *testing.T) {
	h := newBotHarness(t)
	h.pair(privChat, false)
	h.tg.take()
	notif := &tgMessage{MessageID: 55, Chat: tgChat{ID: privChat}, Text: "🟠 Ждёт тебя: Fix bug\nold screen", ReplyMarkup: json.RawMessage(`{"inline_keyboard":[[{"text":"1","callback_data":"k|x|1"}]]}`)}
	click := func(data string) []tgCall {
		h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: "cb", From: tgUser{ID: userID}, Message: notif, Data: data}})
		return h.tg.take()
	}
	toast := func(calls []tgCall) string {
		a := only(calls, "answerCallbackQuery")
		if len(a) != 1 {
			t.Fatalf("answers: %+v", calls)
		}
		return str(a[0].Params["text"])
	}

	if got := toast(click("k|" + strings.ToLower(sidA) + "|1")); got != "Нажато: 1" {
		t.Errorf("toast %q", got)
	}
	if typed := h.ag.typed(); len(typed) != 1 || typed[0] != sidA+"=1" {
		t.Errorf("typed %q", typed)
	}
	// Ctrl+C needs a second tap within a few seconds.
	if got := toast(click("k|" + sidA + "|ctrlc")); !strings.Contains(got, "ещё раз") || h.ag.typed() != nil {
		t.Errorf("first ctrl+c: %q", got)
	}
	h.clock = h.clock.Add(2 * time.Second)
	if got := toast(click("k|" + sidA + "|ctrlc")); got != "Нажато: Ctrl+C" {
		t.Errorf("second ctrl+c: %q", got)
	}
	if typed := h.ag.typed(); len(typed) != 1 || typed[0] != sidA+"=\x03" {
		t.Errorf("ctrl+c typed %q", typed)
	}
	h.clock = h.clock.Add(time.Minute)
	toast(click("k|" + sidA + "|ctrlc"))
	h.clock = h.clock.Add(ctrlcConfirmGap + time.Second)
	if got := toast(click("k|" + sidA + "|ctrlc")); !strings.Contains(got, "ещё раз") {
		t.Errorf("stale confirmation accepted: %q", got)
	}

	// Refresh edits the message in place and keeps its keyboard.
	h.ag.setText("fresh <screen>")
	calls := click("s|" + sidA)
	ed := only(calls, "editMessageText")
	if len(ed) != 1 || num(ed[0].Params["message_id"]) != 55 || !strings.HasPrefix(str(ed[0].Params["text"]), "🟠 Ждёт тебя: Fix bug\n<pre>fresh &lt;screen&gt;</pre>") ||
		!strings.Contains(markup(ed[0]), "k|x|1") || toast(calls) != "Обновлено" {
		t.Errorf("refresh: %+v", calls)
	}
	// More keys and back.
	calls = click("m|" + sidA + "|more")
	if e := only(calls, "editMessageReplyMarkup"); len(e) != 1 || !strings.Contains(markup(e[0]), "|stab") || !strings.Contains(markup(e[0]), "|main") {
		t.Errorf("more: %+v", calls)
	}
	// Closed session, bad data.
	if got := toast(click("k|" + sidC + "|1")); got != "Сессия закрыта" {
		t.Errorf("closed: %q", got)
	}
	if got := toast(click("k|" + sidA + "|rm")); got != "Неизвестная клавиша" || h.ag.typed() != nil {
		t.Errorf("bad key: %q", got)
	}
	toast(click("garbage"))

	// Every callback fits Telegram's 64 bytes.
	for _, kb := range []tgInlineKeyboard{mainKeyboard(sidA, "scratch"), moreKeyboard(sidA, "scratch"), doneKeyboard(sidA, "scratch")} {
		for _, row := range kb.Rows {
			for _, b := range row {
				if len(b.Data) > 64 {
					t.Errorf("callback_data too long: %q", b.Data)
				}
			}
		}
	}
}

func TestTGSessionsForumLinksAndLimits(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	list := only(h.send(h.msg(forumChat, userID, btnSessions)), "sendMessage")
	want := fmt.Sprintf(`"url":"https://t.me/c/1234567890/%d"`, h.bot.st.Topics[sidA].Thread)
	if len(list) != 1 || !strings.Contains(markup(list[0]), want) {
		t.Fatalf("links: %+v", list)
	}

	p, _ := fakeCswap(t, cswapFixture, 0)
	h.bot.srv.limits = newLimits(t, p, t.TempDir())
	lim := only(h.send(h.msg(forumChat, userID, btnLimits)), "sendMessage")
	if len(lim) != 1 || !strings.Contains(str(lim[0].Params["text"]), "использовано") || strings.Contains(str(lim[0].Params["text"]), "|") {
		t.Errorf("limits: %+v", lim)
	}
}

func TestTGTopicDeletedByHand(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	old := h.bot.st.Topics[sidA].Thread
	first := true
	h.tg.hook = func(method string, p map[string]any) (int, string, bool) {
		if method == "sendMessage" && num(p["message_thread_id"]) == old && first {
			first = false
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: message thread not found"}`, true
		}
		return 0, "", false
	}
	h.bot.onStatus(winA, sidA, "blocked", "")
	h.flush()
	calls := h.tg.take()
	if len(only(calls, "createForumTopic")) != 1 || len(only(calls, "sendMessage")) != 2 || h.bot.st.Topics[sidA].Thread == old {
		t.Errorf("recreate: %+v", calls)
	}
}

func TestTGTokenRedaction(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	// Transport errors quote the URL (and so the token).
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	c := newTGClient(dead.URL, testTGToken)
	err := c.call(context.Background(), "getMe", map[string]any{}, nil)
	if err == nil || strings.Contains(err.Error(), testTGToken) || !strings.Contains(err.Error(), "<token>") {
		t.Errorf("transport error: %v", err)
	}
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		t.Errorf("the *url.Error must not be reachable: %T", err)
	}

	// API error text that echoes the token.
	f := newFakeTG(t)
	f.hook = func(string, map[string]any) (int, string, bool) {
		return 401, `{"ok":false,"error_code":401,"description":"Unauthorized: ` + testTGToken + `"}`, true
	}
	c = newTGClient(f.srv.URL, testTGToken)
	err = c.call(context.Background(), "getMe", nil, nil)
	if err == nil || strings.Contains(err.Error(), testTGToken) {
		t.Errorf("api error: %v", err)
	}

	// The poll loop logs its errors redacted, and never message texts.
	h := newBotHarness(t)
	h.bot.api = newTGClient(dead.URL, testTGToken)
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	h.bot.api.sleep = func(context.Context, time.Duration) error {
		if n++; n >= 3 {
			cancel()
		}
		return ctx.Err()
	}
	h.bot.pollLoop(ctx)
	if strings.Contains(logs.String(), testTGToken) || !strings.Contains(logs.String(), "<token>") {
		t.Errorf("log: %s", logs.String())
	}
}

func TestTGRateLimitAndPolling(t *testing.T) {
	h := newBotHarness(t)
	h.pair(privChat, false)
	h.tg.take()

	// sendMessage answered 429 once: retried after retry_after.
	n := 0
	h.tg.hook = func(method string, p map[string]any) (int, string, bool) {
		if method == "sendMessage" && n == 0 {
			n++
			return 429, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`, true
		}
		return 0, "", false
	}
	h.send(h.msg(privChat, userID, btnUnmute))
	if len(h.slept) != 1 || h.slept[0] != 7*time.Second {
		t.Fatalf("slept %v", h.slept)
	}

	// Polling: 429 waits retry_after, errors back off exponentially, updates advance the offset.
	h.slept = nil
	ctx, cancel := context.WithCancel(context.Background())
	polls := 0
	var offsets []int64
	h.tg.hook = func(method string, p map[string]any) (int, string, bool) {
		if method != "getUpdates" {
			return 0, "", false
		}
		polls++
		offsets = append(offsets, num(p["offset"]))
		switch polls {
		case 1:
			return 429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":3}}`, true
		case 2, 3:
			return 502, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`, true
		case 4:
			return 200, fmt.Sprintf(`{"ok":true,"result":[{"update_id":500,"message":{"message_id":9,"from":{"id":%d},"chat":{"id":%d,"type":"private"},"text":"/help"}}]}`, userID, privChat), true
		default:
			cancel()
			return 200, `{"ok":true,"result":[]}`, true
		}
	}
	h.bot.pollLoop(ctx)
	if want := []time.Duration{3 * time.Second, time.Second, 2 * time.Second}; fmt.Sprint(h.slept) != fmt.Sprint(want) {
		t.Errorf("waits %v, want %v", h.slept, want)
	}
	if offsets[len(offsets)-1] != 501 {
		t.Errorf("offsets %v", offsets)
	}
	if saved, _ := loadTGState(h.bot.statePath); saved.Offset != 501 {
		t.Errorf("offset not saved: %d", saved.Offset)
	}
}

func TestLoadTGToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "telegram-token")
	if _, ok, err := loadTGToken(p); ok || err != nil {
		t.Fatal("missing file must just disable the bot")
	}
	os.WriteFile(p, []byte(testTGToken+"\n"), 0o644)
	if _, ok, err := loadTGToken(p); ok || err == nil {
		t.Error("world-readable token accepted")
	}
	os.Chmod(p, 0o600)
	if tok, ok, err := loadTGToken(p); !ok || err != nil || tok != testTGToken {
		t.Errorf("good token: %q %v %v", tok, ok, err)
	}
	os.WriteFile(p, []byte("a/b"), 0o600)
	if _, ok, err := loadTGToken(p); ok || err == nil {
		t.Error("token with a slash accepted")
	}
}

func TestScreenPreAndTopicURL(t *testing.T) {
	long := strings.Repeat(strings.Repeat("<", 100)+"\n", 30)
	got := screenPre(long, 20)
	if len(got) > tgMaxScreen+len("<pre></pre>") || !strings.HasPrefix(got, "<pre>&lt;") || strings.Contains(got, "<<") {
		t.Errorf("cap: %d %.40q", len(got), got)
	}
	if screenPre("\n  \n", 5) != "<i>экран пуст</i>" {
		t.Error("empty")
	}
	if u := topicURL(-1001234567890, 7); u != "https://t.me/c/1234567890/7" {
		t.Error(u)
	}
	if n := topicName(sessionRef{Session{Name: "⠋ Работа", Status: "blocked"}, "EM"}); n != "EM · Работа" {
		t.Error(n)
	}
}

// sessionTopics drops the launcher topic from createForumTopic calls.
func sessionTopics(calls []tgCall) []tgCall {
	var out []tgCall
	for _, c := range calls {
		if str(c.Params["name"]) != launcherTitle {
			out = append(out, c)
		}
	}
	return out
}

// Topics archived by older versions are deleted once; live and launcher topics stay.
func TestTGArchivedTopicsDeletedOnce(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	live, launcher := h.bot.st.Topics[sidA].Thread, h.bot.st.LauncherThread
	h.bot.st.Archived = []tgArchived{{Session: "X", Thread: 501}, {Session: "Y", Thread: 502}, {Session: "Z", Thread: 503}}
	h.bot.st.ArchivedDeletedV1 = false
	fail := true
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "deleteForumTopic" && num(p["message_thread_id"]) == 502 && fail {
			return 502, `{"ok":false,"error_code":502}`, true
		}
		if m == "deleteForumTopic" && num(p["message_thread_id"]) == 503 {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: TOPIC_ID_INVALID"}`, true
		}
		return 0, "", false
	}
	h.bot.syncTopics(context.Background())
	h.tg.take()
	if h.bot.st.ArchivedDeletedV1 || len(h.bot.st.Archived) != 2 {
		t.Fatalf("transient failure must leave the rest for later: %+v", h.bot.st.Archived)
	}
	fail = false
	h.bot.syncTopics(context.Background())
	var deleted []int64
	for _, c := range only(h.tg.take(), "deleteForumTopic") {
		deleted = append(deleted, num(c.Params["message_thread_id"]))
	}
	if fmt.Sprint(deleted) != "[502 503]" || !h.bot.st.ArchivedDeletedV1 || len(h.bot.st.Archived) != 0 {
		t.Fatalf("deleted %v, archived %v", deleted, h.bot.st.Archived)
	}
	if h.bot.st.Topics[sidA].Thread != live || h.bot.st.LauncherThread != launcher {
		t.Error("live or launcher topic touched")
	}
	h.bot.syncTopics(context.Background())
	if d := only(h.tg.take(), "deleteForumTopic"); len(d) != 0 {
		t.Errorf("ran twice: %+v", d)
	}
}

// A topic named after an auto name gets the session's real name once, after
// it has held for a while; then the name is frozen.
func TestTGProvisionalTopicName(t *testing.T) {
	h := newBotHarness(t)
	h.ag.cwd[sidA] = "/Users/someone/someuser"
	h.ag.setTree(t, fakeSession{sidA, "someuser", "idle"}, fakeSession{sidB, "claude", "idle"})
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	if !h.bot.st.Topics[sidA].Provisional || !h.bot.st.Topics[sidB].Provisional {
		t.Fatal("auto names not marked provisional")
	}
	sync := func() []string {
		h.bot.syncTopics(context.Background())
		var out []string
		for _, c := range only(h.tg.take(), "editForumTopic") {
			out = append(out, fmt.Sprintf("%d=%s", num(c.Params["message_thread_id"]), str(c.Params["name"])))
		}
		return out
	}
	threadA := h.bot.st.Topics[sidA].Thread
	// Claude sets a title; it flickers first.
	h.ag.setTree(t, fakeSession{sidA, "⠋ Fix login", "active"}, fakeSession{sidB, "claude", "idle"})
	if e := sync(); len(e) != 0 {
		t.Fatalf("renamed before the name settled: %v", e)
	}
	h.clock = h.clock.Add(5 * time.Second)
	h.ag.setTree(t, fakeSession{sidA, "✳ Fix login bug", "active"}, fakeSession{sidB, "claude", "idle"})
	if e := sync(); len(e) != 0 {
		t.Fatalf("renamed during flicker: %v", e)
	}
	h.clock = h.clock.Add(nameSettle + time.Second)
	if e := sync(); len(e) != 1 || e[0] != fmt.Sprintf("%d=EM · Fix login bug", threadA) {
		t.Fatalf("settle rename: %v", e)
	}
	if h.bot.st.Topics[sidA].Provisional || h.bot.st.Topics[sidA].Name != "Fix login bug" {
		t.Errorf("not frozen: %+v", h.bot.st.Topics[sidA])
	}
	// Frozen now: later titles do not rename it.
	h.ag.setTree(t, fakeSession{sidA, "Another title", "active"}, fakeSession{sidB, "claude", "idle"})
	h.clock = h.clock.Add(time.Minute)
	sync()
	h.clock = h.clock.Add(time.Minute)
	if e := sync(); len(e) != 0 {
		t.Errorf("frozen topic renamed: %v", e)
	}
	// A real name present from the start is never provisional.
	if provisionalName("Fix bug", "/x/proj") || !provisionalName("proj", "/x/proj") || !provisionalName("", "") || !provisionalName("codex", "/a") {
		t.Error("provisionalName rules")
	}
}

// Topics made before the rule: auto-named ones are marked once and then renamed.
func TestTGProvisionalMigration(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	h.ag.cwd[sidA] = "/Users/someone/someuser"
	h.ag.setTree(t, fakeSession{sidA, "Real title", "idle"}, fakeSession{sidB, "shell", "idle"})
	h.bot.st.Topics[sidA].Name, h.bot.st.Topics[sidA].Title = "someuser", "EM · someuser"
	h.bot.st.Topics[sidA].Provisional, h.bot.st.Topics[sidB].Provisional = false, false
	h.bot.st.NamesV3 = false
	h.bot.syncTopics(context.Background())
	if !h.bot.st.NamesV3 || !h.bot.st.Topics[sidA].Provisional || h.bot.st.Topics[sidB].Provisional {
		t.Fatalf("migration: %+v %+v", h.bot.st.Topics[sidA], h.bot.st.Topics[sidB])
	}
	h.tg.take()
	h.clock = h.clock.Add(nameSettle + time.Second)
	h.bot.syncTopics(context.Background())
	if e := only(h.tg.take(), "editForumTopic"); len(e) != 1 || str(e[0].Params["name"]) != "EM · Real title" {
		t.Fatalf("rename after migration: %+v", e)
	}
}
