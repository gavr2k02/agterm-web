package main

// Regression tests for the Codex audit of the Telegram bot, and the launcher.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func rawUpdate(t *testing.T, raw string) tgUpdate {
	t.Helper()
	var u tgUpdate
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

// Finding 1: an anonymous admin (or any bot / synthetic sender) can neither
// pair nor be listened to.
func TestTGAnonymousAdminRejected(t *testing.T) {
	h := newBotHarness(t)
	code, _ := newPairCode(h.bot.pairPath, h.clock)
	anon := func(text string, thread int64) tgUpdate {
		return rawUpdate(t, fmt.Sprintf(`{"update_id":1,"message":{"message_id":2,"from":{"id":1087968824,"is_bot":true,"username":"GroupAnonymousBot"},"sender_chat":{"id":%d,"type":"supergroup"},"chat":{"id":%d,"type":"supergroup","is_forum":true},"is_topic_message":true,"message_thread_id":%d,"text":%q}}`, forumChat, forumChat, thread, text))
	}
	h.bot.handleUpdate(context.Background(), anon("/pair "+code, 0))
	calls := only(h.tg.take(), "sendMessage")
	if h.bot.st.paired() || len(calls) != 1 || !strings.Contains(str(calls[0].Params["text"]), "не анонимно") {
		t.Fatalf("anonymous pairing: paired=%v %+v", h.bot.st.paired(), calls)
	}
	if !pairPending(h.bot.pairPath, h.clock) {
		t.Fatal("the code must survive an anonymous attempt")
	}
	// Other synthetic or bot senders, also with a real-looking id.
	for _, raw := range []string{
		fmt.Sprintf(`{"message":{"from":{"id":777000},"chat":{"id":%d},"text":"/pair %s"}}`, privChat, code),
		fmt.Sprintf(`{"message":{"from":{"id":136817688},"chat":{"id":%d},"text":"/pair %s"}}`, privChat, code),
		fmt.Sprintf(`{"message":{"from":{"id":%d,"is_bot":true},"chat":{"id":%d},"text":"/pair %s"}}`, userID, privChat, code),
		fmt.Sprintf(`{"message":{"from":{"id":%d},"sender_chat":{"id":-100777},"chat":{"id":%d},"text":"/pair %s"}}`, userID, privChat, code),
	} {
		h.bot.handleUpdate(context.Background(), rawUpdate(t, raw))
		if h.bot.st.paired() {
			t.Fatalf("paired by %s", raw)
		}
	}
	h.tg.take()

	// The owner pairs as themselves; an anonymous admin's text is then ignored.
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	h.bot.handleUpdate(context.Background(), anon("echo attacker", h.bot.st.Topics[sidA].Thread))
	if typed := h.ag.typed(); len(typed) != 0 {
		t.Fatalf("anonymous admin typed %q", typed)
	}
	// Owner's id but posted on behalf of the group (sender_chat): ignored too.
	h.bot.handleUpdate(context.Background(), rawUpdate(t, fmt.Sprintf(`{"message":{"message_id":3,"from":{"id":%d},"sender_chat":{"id":%d},"chat":{"id":%d,"is_forum":true},"is_topic_message":true,"message_thread_id":%d,"text":"x"}}`, userID, forumChat, forumChat, h.bot.st.Topics[sidA].Thread)))
	h.bot.handleUpdate(context.Background(), rawUpdate(t, fmt.Sprintf(`{"callback_query":{"id":"q","from":{"id":%d,"is_bot":true},"message":{"message_id":1,"chat":{"id":%d}},"data":"k|%s|1|l"}}`, userID, forumChat, sidA)))
	if typed := h.ag.typed(); len(typed) != 0 || len(h.tg.take()) != 0 {
		t.Fatalf("group-signed message or bot callback acted: %q", typed)
	}
}

// Finding 2: a code that cannot be used up does not pair, and two racing
// right answers pair once.
func TestPairCodeFailsClosed(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "telegram-pair.json")
	now := time.Now()
	code, _ := newPairCode(p, now)
	os.Chmod(d, 0o500)
	first, second := checkPairCode(p, code, now), checkPairCode(p, code, now)
	os.Chmod(d, 0o700)
	if first == nil || second == nil {
		t.Fatalf("pairing succeeded without using the code up: %v %v", first, second)
	}

	code, _ = newPairCode(p, now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if checkPairCode(p, code, now) == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Errorf("%d racing checks succeeded, want 1", ok)
	}

	// Through the bot: refused, with a reply, and nothing paired.
	h := newBotHarness(t)
	code, _ = newPairCode(h.bot.pairPath, h.clock)
	os.Chmod(h.dir, 0o500)
	calls := only(h.send(h.msg(privChat, userID, "/pair "+code)), "sendMessage")
	os.Chmod(h.dir, 0o700)
	if h.bot.st.paired() || len(calls) != 1 || !strings.Contains(str(calls[0].Params["text"]), "не связан") {
		t.Fatalf("bot paired with an unusable code: %+v", calls)
	}
}

// Finding 3: the pane of the notification is the pane keys, replies and refresh act on.
func TestTGPaneFollowsNotification(t *testing.T) {
	h := newBotHarness(t)
	h.pair(privChat, false)
	h.tg.take()
	h.ag.args()
	h.bot.onStatus(winA, sidA, "blocked", "right")
	h.flush()
	sent := only(h.tg.take(), "sendMessage")
	if len(sent) != 1 || !strings.Contains(markup(sent[0]), `"k|`+sidA+`|enter|r"`) || !strings.Contains(h.ag.args(), "--pane right") {
		t.Fatalf("notification: %+v", sent)
	}
	ref := h.bot.st.MsgRefs[len(h.bot.st.MsgRefs)-1]
	if ref.Pane != "right" {
		t.Errorf("ref pane %q", ref.Pane)
	}
	notif := &tgMessage{MessageID: ref.ID, Chat: tgChat{ID: privChat}, Text: "🟠 Ждёт тебя: Fix bug"}
	for _, data := range []string{"k|" + sidA + "|enter|r", "s|" + sidA + "|r"} {
		h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: "cb", From: tgUser{ID: userID}, Message: notif, Data: data}})
		if a := h.ag.args(); !strings.Contains(a, "--pane right") || strings.Contains(a, "--pane left") {
			t.Errorf("%s acted on %q", data, a)
		}
	}
	h.tg.take()
	m := h.msg(privChat, userID, "go on")
	m.ReplyTo = &tgMessage{MessageID: ref.ID}
	h.send(m)
	if a := h.ag.args(); !strings.Contains(a, "session type --stdin --target "+sidA+" --window "+winA+" --pane right") {
		t.Errorf("reply went to %q", a)
	}
	// Old buttons without a pane still mean left.
	h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: "cb", From: tgUser{ID: userID}, Message: notif, Data: "k|" + sidA + "|1"}})
	if a := h.ag.args(); !strings.Contains(a, "--pane left") {
		t.Errorf("legacy button: %q", a)
	}

	// Forum: text in a topic goes to the pane the agent reported its status from.
	f := newBotHarness(t)
	f.pair(forumChat, true)
	f.flush()
	f.tg.take()
	f.bot.onEvent(agtermEvent{Kind: "status", Session: sidA, Payload: struct {
		Name     string `json:"name"`
		Status   string `json:"status"`
		Previous string `json:"previous"`
		Pane     string `json:"pane"`
	}{Status: "active", Previous: "active", Pane: "right"}})
	f.ag.args()
	tm := f.msg(forumChat, userID, "continue")
	tm.IsTopicMessage, tm.MessageThreadID = true, f.bot.st.Topics[sidA].Thread
	f.send(tm)
	if a := f.ag.args(); !strings.Contains(a, "--pane right") {
		t.Errorf("forum text went to %q", a)
	}
}

// Finding 4: the offset past an update is on disk before the update acts.
func TestTGOffsetSavedBeforeEffects(t *testing.T) {
	h := newBotHarness(t)
	h.pair(privChat, false)
	h.bot.st.Current = sidA
	h.bot.st.Offset = 500
	h.tg.take()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	var seen []int64
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "getUpdates" {
			if n++; n == 1 {
				return 200, fmt.Sprintf(`{"ok":true,"result":[{"update_id":500,"message":{"from":{"id":%d},"chat":{"id":%d},"text":"run-once"}},{"update_id":501,"message":{"from":{"id":%d},"chat":{"id":%d},"text":"/help"}}]}`, userID, privChat, userID, privChat), true
			}
			cancel()
			return 200, `{"ok":true,"result":[]}`, true
		}
		if m == "sendMessage" { // the "→ Fix bug" of 500, then the help of 501
			st, _ := loadTGState(h.bot.statePath)
			seen = append(seen, st.Offset)
		}
		return 0, "", false
	}
	h.bot.pollLoop(ctx)
	if fmt.Sprint(seen) != "[501 502]" {
		t.Errorf("offset on disk while each update ran: %v, want [501 502]", seen)
	}
}

// Finding 5: work started for an earlier pairing never lands in the new one.
func TestTGRepairDuringSync(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.tg.take()
	h.bot.after = func(time.Duration, func()) {}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "createForumTopic" && num(p["chat_id"]) == forumChat {
			once.Do(func() { close(entered); <-release })
		}
		return 0, "", false
	}
	done := make(chan struct{})
	go func() { h.bot.syncTopics(context.Background()); close(done) }()
	<-entered
	const newChat int64 = -1005555555555
	code, _ := newPairCode(h.bot.pairPath, h.clock)
	m := h.msg(newChat, userID, "/pair "+code)
	m.Chat.IsForum = true
	h.bot.handleUpdate(context.Background(), tgUpdate{Message: m})
	close(release)
	<-done
	var oldThreads []int64
	for _, c := range only(h.tg.take(), "createForumTopic") {
		if num(c.Params["chat_id"]) == forumChat {
			oldThreads = append(oldThreads, 0) // count only
		}
	}
	if h.bot.st.ChatID != newChat || len(h.bot.st.Topics) != 0 || h.bot.st.LauncherThread != 0 {
		t.Fatalf("old pairing's topics landed in the new one: %+v launcher=%d", h.bot.st.Topics, h.bot.st.LauncherThread)
	}
	if len(oldThreads) != 1 {
		t.Errorf("old sync went on after the pairing changed: %d creates in the old chat", len(oldThreads))
	}
}

// Finding 6: transient failures are retried; archive steps are recorded only when done.
func TestTGTransientRetries(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	bad := 1
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "sendMessage" && bad > 0 {
			bad--
			return 502, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`, true
		}
		return 0, "", false
	}
	h.bot.onStatus(winA, sidA, "blocked", "")
	h.flush() // first try: 502
	if len(only(h.tg.take(), "sendMessage")) != 1 || len(h.pending) != 1 {
		t.Fatalf("no retry scheduled: %d", len(h.pending))
	}
	h.flush() // retry: delivered
	if len(only(h.tg.take(), "sendMessage")) != 1 || len(h.pending) != 0 {
		t.Fatal("retry not delivered or kept retrying")
	}
	// Permanent errors are not retried; transient ones give up after tgRetries.
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "sendMessage" {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, true
		}
		return 0, "", false
	}
	h.clock = h.clock.Add(time.Minute)
	h.bot.onStatus(winA, sidA, "blocked", "")
	h.flush()
	if len(h.pending) != 0 {
		t.Error("a 400 was retried")
	}
	h.tg.take()
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "sendMessage" {
			return 503, `{"ok":false,"error_code":503,"description":"x"}`, true
		}
		return 0, "", false
	}
	h.clock = h.clock.Add(time.Minute)
	h.bot.onStatus(winA, sidA, "blocked", "")
	for i := 0; i < 20 && len(h.pending) > 0; i++ {
		h.flush()
	}
	if n := len(only(h.tg.take(), "sendMessage")); n != tgRetries+1 {
		t.Errorf("%d tries, want %d", n, tgRetries+1)
	}

	// Close: a transient failure keeps the "closing" mark (saved for a
	// restart); the next sync deletes the topic and drops its state.
	threadA := h.bot.st.Topics[sidA].Thread
	failing := true
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "deleteForumTopic" && failing {
			return 502, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`, true
		}
		return 0, "", false
	}
	h.bot.onEvent(agtermEvent{Kind: "session.closed", Session: sidA})
	if ref := h.bot.st.Topics[sidA]; ref == nil || !ref.Closing {
		t.Fatalf("failed delete dropped the state: %+v", ref)
	}
	saved, _ := loadTGState(h.bot.statePath)
	if saved.Topics[sidA] == nil || !saved.Topics[sidA].Closing {
		t.Error("closing mark not saved for a restart")
	}
	// Text in the closing topic goes nowhere.
	m := h.msg(forumChat, userID, "x")
	m.IsTopicMessage, m.MessageThreadID = true, threadA
	h.send(m)
	if h.ag.typed() != nil {
		t.Error("typed into a closed session's topic")
	}
	failing = false
	h.ag.setTree(t, fakeSession{sidB, "shell", "idle"})
	h.tg.take()
	h.bot.syncTopics(context.Background())
	if d := only(h.tg.take(), "deleteForumTopic"); len(d) != 1 || num(d[0].Params["message_thread_id"]) != threadA || h.bot.st.Topics[sidA] != nil {
		t.Fatalf("retry: %+v %+v", d, h.bot.st.Topics[sidA])
	}
}

// Finding 7: a 429 retry queues again in the outbox, and its retry_after holds
// the whole chat; the client itself does not retry.
func TestTGOutboxRetryAfter(t *testing.T) {
	f := newFakeTG(t)
	n := 0
	f.hook = func(m string, p map[string]any) (int, string, bool) {
		if n++; n == 1 {
			return 429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":5}}`, true
		}
		return 0, "", false
	}
	api := newTGClient(f.srv.URL, testTGToken)
	o := newTGOutbox(api)
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	var waits []time.Duration
	o.now = func() time.Time { return clock }
	o.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		clock = clock.Add(d)
		return nil
	}
	if err := o.call(context.Background(), privChat, "sendMessage", map[string]any{"chat_id": privChat, "text": "a"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := o.call(context.Background(), privChat, "sendMessage", map[string]any{"chat_id": privChat, "text": "b"}, nil); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(waits) != "[5s 1s]" {
		t.Errorf("waits %v, want [5s 1s]", waits)
	}
	n = 0
	if err := api.call(context.Background(), "sendMessage", map[string]any{}, nil); err == nil || n != 1 {
		t.Errorf("client retried by itself: %v, %d calls", err, n)
	}
}

// Finding 8: a long session name and a full screen still make one valid message.
func TestTGLongNameNotification(t *testing.T) {
	h := newBotHarness(t)
	h.pair(privChat, false)
	h.ag.setTree(t, fakeSession{sidA, strings.Repeat("<я>", 700), "blocked"})
	h.ag.setText(strings.Repeat(strings.Repeat("&", 173)+"\n", 20))
	h.tg.take()
	h.bot.onStatus(winA, sidA, "blocked", "")
	h.flush()
	calls := only(h.tg.take(), "sendMessage")
	if len(calls) != 1 {
		t.Fatal(calls)
	}
	text := str(calls[0].Params["text"])
	// Telegram counts characters after entity parsing; raw length is an upper bound.
	if n := utf8.RuneCountInString(text); n > 4096 || !utf8.ValidString(text) || !strings.HasSuffix(text, "</pre>") || !strings.Contains(text, "…</b>") {
		t.Errorf("%d chars: %.120q…", n, text)
	}
}

/* ---------- launcher ---------- */

const sidNew = "ABCDEF12-7777-7777-7777-777777777777"

func launcherHarness(t *testing.T) (*botHarness, string) {
	h := newBotHarness(t)
	proj := filepath.Join(h.home, "proj")
	os.MkdirAll(proj, 0o755)
	os.MkdirAll(filepath.Join(h.home, "other"), 0o755)
	h.ag.cwd[sidA] = proj
	h.ag.setTree(t, fakeSession{sidA, "◐ Fix bug", "blocked"}, fakeSession{sidB, "shell", "idle"})
	h.ag.cwd[sidNew] = proj
	h.ag.afterNew(t, sidNew, fakeSession{sidA, "◐ Fix bug", "blocked"}, fakeSession{sidB, "shell", "idle"}, fakeSession{sidNew, "proj", "idle"})
	return h, proj
}

func TestTGLauncherFlow(t *testing.T) {
	h, proj := launcherHarness(t)
	os.WriteFile(h.bot.launcherFile, []byte(`{"favorites":["~/other","~/missing","/etc"]}`), 0o600)
	h.pair(forumChat, true)
	h.flush()
	calls := h.tg.take()
	var lt int64
	for _, c := range only(calls, "createForumTopic") {
		if str(c.Params["name"]) == launcherTitle {
			lt = h.bot.st.LauncherThread
		}
	}
	pins := only(calls, "pinChatMessage")
	if lt == 0 || h.bot.st.LauncherMsg == 0 || len(pins) != 1 || num(pins[0].Params["message_id"]) != h.bot.st.LauncherMsg {
		t.Fatalf("launcher topic: %+v", calls)
	}
	var menu tgCall
	for _, c := range only(calls, "sendMessage") {
		if num(c.Params["message_thread_id"]) == lt {
			menu = c
		}
	}
	// Favorites first (existing, inside home only), then folders of open sessions.
	kb := markup(menu)
	if !strings.Contains(kb, `"callback_data":"L|d|0","text":"📂 ~/other"`) || !strings.Contains(kb, `"callback_data":"L|d|1","text":"📂 ~/proj"`) ||
		strings.Contains(kb, "missing") || strings.Contains(kb, "/etc") || !strings.Contains(kb, "L|d|o") {
		t.Fatalf("step 1 keyboard: %s", kb)
	}

	launcherMsg := &tgMessage{MessageID: h.bot.st.LauncherMsg, Chat: tgChat{ID: forumChat, IsForum: true}, IsTopicMessage: true, MessageThreadID: lt}
	click := func(data string) []tgCall {
		h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: "cb", From: tgUser{ID: userID}, Message: launcherMsg, Data: data}})
		return h.tg.take()
	}
	edited := func(calls []tgCall) string {
		e := only(calls, "editMessageText")
		if len(e) != 1 || num(e[0].Params["message_id"]) != h.bot.st.LauncherMsg {
			t.Fatalf("launcher not edited in place: %+v", calls)
		}
		return str(e[0].Params["text"]) + markup(e[0])
	}
	if s := edited(click("L|d|1")); !strings.Contains(s, "Шаг 2") || !strings.Contains(s, "~/proj") || !strings.Contains(s, "L|p|claude-continue") {
		t.Fatalf("step 2: %s", s)
	}
	if s := edited(click("L|p|claude")); !strings.Contains(s, "Шаг 3") || !strings.Contains(s, `"callback_data":"L|w|0","text":"● EM"`) || !strings.Contains(s, "L|go") {
		t.Fatalf("step 3: %s", s)
	}
	h.ag.args()
	if a := only(click("L|go"), "answerCallbackQuery"); len(a) != 1 || str(a[0].Params["text"]) != "Запускаю…" {
		t.Fatalf("go toast: %+v", a)
	}
	h.flush()
	args := h.ag.args()
	newArgs, _ := os.ReadFile(filepath.Join(h.ag.dir, "new.args"))
	if want := "session\nnew\n--cwd\n" + proj + "\n--no-select\n--name\nproj\n--workspace\nW1\n--window\n" + winA + "\n--json\n"; string(newArgs) != want {
		t.Errorf("session new argv:\n%q\nwant\n%q", newArgs, want)
	}
	if typed := h.ag.typed(); len(typed) != 2 || typed[0] != sidNew+"=claude" || typed[1] != sidNew+"=\r" {
		t.Errorf("typed %q (args %q)", typed, args)
	}
	calls = h.tg.take()
	var report string
	for _, c := range only(calls, "sendMessage") {
		if num(c.Params["message_thread_id"]) == lt {
			report = str(c.Params["text"])
		}
	}
	newTopic := h.bot.st.Topics[sidNew]
	if newTopic == nil || !strings.Contains(report, "✅ Запущено") || !strings.Contains(report, fmt.Sprintf("https://t.me/c/1234567890/%d", newTopic.Thread)) {
		t.Errorf("report %q, topic %+v", report, newTopic)
	}
	if s := edited(calls); !strings.Contains(s, "Шаг 1") {
		t.Errorf("launcher not reset: %s", s)
	}
	if len(h.bot.st.Recent) != 1 || h.bot.st.Recent[0].Path != proj {
		t.Errorf("recent: %+v", h.bot.st.Recent)
	}

	// The launcher topic is never deleted, even when every session is gone.
	h.ag.setTree(t, fakeSession{sidB, "shell", "idle"})
	h.bot.syncTopics(context.Background())
	for _, c := range only(h.tg.take(), "deleteForumTopic") {
		if num(c.Params["message_thread_id"]) == lt {
			t.Fatal("launcher topic deleted")
		}
	}
}

func TestTGLauncherOtherFolderAndNewWorkspace(t *testing.T) {
	h, _ := launcherHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	lt := h.bot.st.LauncherThread
	launcherMsg := &tgMessage{MessageID: h.bot.st.LauncherMsg, Chat: tgChat{ID: forumChat}, IsTopicMessage: true, MessageThreadID: lt}
	click := func(data string) {
		h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: "cb", From: tgUser{ID: userID}, Message: launcherMsg, Data: data}})
	}
	say := func(text string) []tgCall {
		m := h.msg(forumChat, userID, text)
		m.IsTopicMessage, m.MessageThreadID = true, lt
		return h.send(m)
	}
	os.Symlink(t.TempDir(), filepath.Join(h.home, "escape"))
	os.WriteFile(filepath.Join(h.home, "file"), nil, 0o644)

	click("L|d|o")
	h.tg.take()
	for _, bad := range []string{"~/nope", "/etc", "~/escape", "~/../..", "~/file", ""} {
		c := only(say(bad), "sendMessage")
		if bad != "" && (len(c) != 1 || !strings.Contains(str(c[0].Params["text"]), "Не подходит")) {
			t.Errorf("%q accepted: %+v", bad, c)
		}
	}
	if h.ag.typed() != nil {
		t.Fatal("launcher text typed into a session")
	}
	if e := only(say("~/other"), "editMessageText"); len(e) != 1 || !strings.Contains(str(e[0].Params["text"]), "Шаг 2") {
		t.Fatalf("other folder: %+v", e)
	}
	click("L|p|shell")
	click("L|w|n")
	h.tg.take()
	say("Новый WS; rm -rf")
	click("L|go")
	h.flush()
	newArgs, _ := os.ReadFile(filepath.Join(h.ag.dir, "new.args"))
	if !strings.Contains(string(newArgs), "--cwd\n"+filepath.Join(h.home, "other")+"\n") || !strings.Contains(string(newArgs), "--workspace-name\nНовый WS; rm -rf\n--create-workspace\n") {
		t.Errorf("argv %q", newArgs)
	}
	if typed := h.ag.typed(); len(typed) != 0 {
		t.Errorf("terminal launch typed %q", typed)
	}
	h.tg.take()
	// Outside the flow, text in the launcher topic only gets a hint.
	if c := only(say("hello"), "sendMessage"); len(c) != 1 || !strings.Contains(str(c[0].Params["text"]), "/new") || h.ag.typed() != nil {
		t.Errorf("hint: %+v", c)
	}
}

func TestTGNewCommand(t *testing.T) {
	h, proj := launcherHarness(t)
	h.pair(privChat, false)
	h.tg.take()

	h.send(h.msg(privChat, userID, "/new ~/proj codex"))
	h.flush()
	newArgs, _ := os.ReadFile(filepath.Join(h.ag.dir, "new.args"))
	if !strings.Contains(string(newArgs), "--cwd\n"+proj+"\n") || !strings.Contains(string(newArgs), "--workspace\nW1\n") {
		t.Errorf("argv %q", newArgs)
	}
	if typed := h.ag.typed(); len(typed) != 2 || typed[0] != sidNew+"=codex" {
		t.Errorf("typed %q", typed)
	}
	reports := only(h.tg.take(), "sendMessage")
	if len(reports) != 1 || !strings.Contains(str(reports[0].Params["text"]), "✅ Запущено: <b>proj</b>") || h.bot.st.Current != sidNew {
		t.Errorf("private report: %+v current=%s", reports, h.bot.st.Current)
	}

	// Program words are only the four constants; anything else is the workspace name.
	const sid2 = "ABCDEF12-8888-8888-8888-888888888888"
	h.ag.cwd[sid2] = proj
	h.ag.afterNew(t, sid2, fakeSession{sidA, "◐ Fix bug", "blocked"}, fakeSession{sid2, "proj", "idle"})
	h.send(h.msg(privChat, userID, "/new ~/proj claude;id Other"))
	h.flush()
	newArgs, _ = os.ReadFile(filepath.Join(h.ag.dir, "new.args"))
	if !strings.Contains(string(newArgs), "--workspace-name\nclaude;id Other\n") {
		t.Errorf("argv %q", newArgs)
	}
	if typed := h.ag.typed(); len(typed) != 2 || typed[0] != sid2+"=claude" {
		t.Errorf("typed %q", typed)
	}
	h.tg.take()

	for _, bad := range []string{"/new", "/new /etc", "/new ~/nope claude"} {
		if c := only(h.send(h.msg(privChat, userID, bad)), "sendMessage"); len(c) != 1 || len(h.pending) != 0 {
			t.Errorf("%q: %+v", bad, c)
		}
	}
	// agterm refuses.
	os.WriteFile(filepath.Join(h.ag.dir, "new-fails"), nil, 0o644)
	h.send(h.msg(privChat, userID, "/new ~/proj"))
	h.flush()
	if c := only(h.tg.take(), "sendMessage"); len(c) != 1 || !strings.Contains(str(c[0].Params["text"]), "Не запустилось: no such workspace") {
		t.Errorf("failure: %+v", c)
	}
}

func TestTGLauncherRecreatedWhenDeleted(t *testing.T) {
	h, _ := launcherHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	old := h.bot.st.LauncherThread
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "editMessageText" {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`, true
		}
		return 0, "", false
	}
	msg := &tgMessage{MessageID: h.bot.st.LauncherMsg, Chat: tgChat{ID: forumChat}, IsTopicMessage: true, MessageThreadID: old}
	h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: "cb", From: tgUser{ID: userID}, Message: msg, Data: "L|d|0"}})
	h.tg.hook = nil
	h.flush()
	if h.bot.st.LauncherThread == 0 || h.bot.st.LauncherThread == old {
		t.Errorf("launcher not recreated: %d (was %d)", h.bot.st.LauncherThread, old)
	}
}

// realTempDir: a temp dir in canonical form (macOS /var is /private/var).
func realTempDir(t *testing.T) string {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestResolveDir(t *testing.T) {
	home := realTempDir(t)
	os.MkdirAll(filepath.Join(home, "a b", "c"), 0o755)
	for in, want := range map[string]string{
		"~":          home,
		"~/a b":      filepath.Join(home, "a b"),
		"a b/c":      filepath.Join(home, "a b", "c"),
		"~/a b/c/..": filepath.Join(home, "a b"),
	} {
		if got, err := resolveDir(in, home); err != nil || got != want {
			t.Errorf("%q -> %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/", "~/..", "~/missing", filepath.Dir(home)} {
		if got, err := resolveDir(bad, home); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}
