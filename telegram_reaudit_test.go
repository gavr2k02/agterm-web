package main

// Regression tests for the second Codex audit of the Telegram bot.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 1. An update whose offset cannot be saved is not run.
func TestTGOffsetSaveFailureFailsClosed(t *testing.T) {
	h := newBotHarness(t)
	h.pair(privChat, false)
	h.bot.st.Current = sidA
	h.bot.st.Offset = 500
	h.bot.st.save(h.bot.statePath)
	h.tg.take()
	os.Chmod(h.dir, 0o500)
	defer os.Chmod(h.dir, 0o700)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polls := 0
	var offsets []int64
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "getUpdates" {
			offsets = append(offsets, num(p["offset"]))
			if polls++; polls == 3 {
				cancel()
			}
			return 200, fmt.Sprintf(`{"ok":true,"result":[{"update_id":500,"message":{"from":{"id":%d},"chat":{"id":%d},"text":"run-once"}}]}`, userID, privChat), true
		}
		return 0, "", false
	}
	h.bot.pollLoop(ctx)
	st, _ := loadTGState(h.bot.statePath)
	if typed := h.ag.typed(); len(typed) != 0 || st.Offset != 500 || len(only(h.tg.take(), "sendMessage")) != 0 {
		t.Fatalf("ran without a saved offset: typed %q, disk offset %d", typed, st.Offset)
	}
	if fmt.Sprint(offsets) != "[500 500 500]" || h.bot.st.Offset != 500 {
		t.Errorf("must keep asking for 500: %v, memory %d", offsets, h.bot.st.Offset)
	}
}

// 2. A launch queued before a re-pairing does nothing.
func TestTGStaleLaunchDropped(t *testing.T) {
	h, _ := launcherHarness(t)
	h.pair(privChat, false)
	h.send(h.msg(privChat, userID, "/new ~/proj claude"))
	code, _ := newPairCode(h.bot.pairPath, h.clock)
	h.send(h.msg(999, 999, "/pair "+code))
	h.tg.take()
	h.ag.args()
	h.flush()
	if a := h.ag.args(); strings.Contains(a, "session new") || h.ag.typed() != nil || len(h.tg.take()) != 0 {
		t.Fatalf("stale launch acted: %q", a)
	}
}

// 3. The program goes only into a fresh shell.
func TestTGLaunchIntoBusySession(t *testing.T) {
	h, _ := launcherHarness(t)
	h.ag.fg[sidNew] = []string{"/usr/bin/vim", "important.txt"} // someone got there first
	h.ag.afterNew(t, sidNew, fakeSession{sidA, "◐ Fix bug", "blocked"}, fakeSession{sidNew, "proj", "idle"})
	h.pair(privChat, false)
	h.tg.take()
	h.send(h.msg(privChat, userID, "/new ~/proj claude"))
	h.flush()
	c := only(h.tg.take(), "sendMessage")
	if typed := h.ag.typed(); len(typed) != 0 || len(c) != 1 || !strings.Contains(str(c[0].Params["text"]), "занята") {
		t.Fatalf("typed %q into a busy session: %+v", typed, c)
	}
	for _, tc := range []struct {
		s           Session
		ready, busy bool
	}{
		{Session{ForegroundShell: "zsh"}, true, false},
		{Session{ForegroundShell: "-zsh"}, true, false},
		{Session{Foreground: []string{"-bash"}}, true, false},
		{Session{Foreground: []string{"/opt/homebrew/bin/fish"}}, true, false},
		{Session{}, false, false}, // agterm cannot tell yet
		{Session{Foreground: []string{"zsh", "-c", "x"}}, false, true},
		{Session{Foreground: []string{"claude"}}, false, true},
		{Session{ForegroundShell: "tcsh"}, false, true},
	} {
		if r, b := shellState(tc.s); r != tc.ready || b != tc.busy {
			t.Errorf("%+v: ready=%v busy=%v", tc.s, r, b)
		}
	}
}

// 3b. The launcher holds the pane's write lock while checking and typing.
func TestTGLaunchHoldsPaneLock(t *testing.T) {
	h, proj := launcherHarness(t)
	h.pair(privChat, false)
	h.tg.take()
	mu := h.bot.srv.paneLock(winA, sidNew, "left")
	mu.Lock()
	done := make(chan struct{})
	go func() {
		h.bot.launch(context.Background(), h.bot.gen, launchReq{dir: proj, prog: "claude", wsID: "W1", window: winA, chat: privChat})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("launch typed without the pane lock")
	case <-time.After(300 * time.Millisecond):
	}
	if h.ag.typed() != nil {
		t.Fatal("typed while another writer held the pane")
	}
	mu.Unlock()
	<-done
	if typed := h.ag.typed(); len(typed) != 2 || typed[0] != sidNew+"=claude" {
		t.Errorf("typed %q", typed)
	}
}

// 4. A partly delivered program line is never typed again.
func TestTGLaunchNoRetype(t *testing.T) {
	h, _ := launcherHarness(t)
	script, _ := os.ReadFile(h.ag.path)
	marker := `    printf '%s=' "$t" >> "$d/typed"; cat >> "$d/typed"; printf '\n' >> "$d/typed"`
	replacement := `    payload=$(cat)
    if [ "$payload" = "$(printf '\r')" ] && [ ! -f "$d/enter-failed" ]; then
      touch "$d/enter-failed"; echo '{"ok":false,"error":"temporary control failure"}'; exit 1
    fi
    printf '%s=%s\n' "$t" "$payload" >> "$d/typed"`
	if !strings.Contains(string(script), marker) {
		t.Fatal("marker absent")
	}
	os.WriteFile(h.ag.path, []byte(strings.Replace(string(script), marker, replacement, 1)), 0o755)
	h.pair(privChat, false)
	h.tg.take()
	h.send(h.msg(privChat, userID, "/new ~/proj claude"))
	h.flush()
	c := only(h.tg.take(), "sendMessage")
	if typed := h.ag.typed(); len(typed) != 1 || typed[0] != sidNew+"=claude" || len(c) != 1 || !strings.Contains(str(c[0].Params["text"]), "Не удалось запустить Claude, сессия открыта") {
		t.Fatalf("typed %q, reports %+v", typed, c)
	}
}

// 5. The canonical folder is launched, and checked again right before.
func TestTGLaunchSymlinkSwap(t *testing.T) {
	h, proj := launcherHarness(t)
	h.pair(privChat, false)
	h.tg.take()
	link := filepath.Join(h.home, "link")
	os.Symlink(proj, link)
	h.send(h.msg(privChat, userID, "/new ~/link shell"))
	h.flush()
	raw, _ := os.ReadFile(filepath.Join(h.ag.dir, "new.args"))
	if !strings.Contains(string(raw), "--cwd\n"+proj+"\n") {
		t.Fatalf("launched %q, want the canonical %s", raw, proj)
	}
	h.tg.take()
	os.Remove(filepath.Join(h.ag.dir, "new.args"))

	h.send(h.msg(privChat, userID, "/new ~/link shell"))
	os.Remove(link)
	os.Symlink(realTempDir(t), link) // swapped while queued — but the canonical path was kept
	os.RemoveAll(proj)               // and the original target is gone
	h.flush()
	if _, err := os.Stat(filepath.Join(h.ag.dir, "new.args")); err == nil {
		t.Fatal("launched after the folder changed")
	}
	if c := only(h.tg.take(), "sendMessage"); len(c) != 1 || !strings.Contains(str(c[0].Params["text"]), "Папка изменилась") {
		t.Errorf("report %+v", c)
	}
}

// 6. A cooldown that arrives after a send reserved its slot still holds it.
func TestTGOutboxCooldownHoldsReserved(t *testing.T) {
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	o := newTGOutbox(nil)
	o.now = func() time.Time { return clock }
	o.reserve(privChat)                 // A goes now
	d := o.reserve(privChat)            // B waits a second
	o.cooldown(privChat, 2*time.Second) // A gets 429 retry_after=2
	clock = clock.Add(d)
	if w := o.coolWait(privChat); w != time.Second {
		t.Fatalf("B would go %v early", w)
	}

	// End to end with real time.
	f := newFakeTG(t)
	o = newTGOutbox(newTGClient(f.srv.URL, testTGToken))
	o.groupMax = 0
	first, release, reserved := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var seen []time.Time
	f.hook = func(m string, p map[string]any) (int, string, bool) {
		mu.Lock()
		seen = append(seen, time.Now())
		n := len(seen)
		mu.Unlock()
		if n == 1 {
			close(first)
			<-release
			return 429, `{"ok":false,"error_code":429,"parameters":{"retry_after":2}}`, true
		}
		return 0, "", false
	}
	o.sleep = func(ctx context.Context, d time.Duration) error {
		once.Do(func() { close(reserved) })
		return sleepCtx(ctx, d)
	}
	a, b := make(chan error, 1), make(chan error, 1)
	go func() { a <- o.call(context.Background(), privChat, "sendMessage", map[string]any{}, nil) }()
	<-first
	go func() { b <- o.call(context.Background(), privChat, "sendMessage", map[string]any{}, nil) }()
	<-reserved
	close(release)
	if err := <-b; err != nil {
		t.Fatal(err)
	}
	if err := <-a; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[1].Sub(seen[0]) < 1900*time.Millisecond {
		t.Fatalf("next send %v after the 429", seen[1].Sub(seen[0]))
	}
}

// 7. Pin failures are retried; a deleted launcher is found and recreated; an unpin is repaired.
func TestTGLauncherRepair(t *testing.T) {
	h, _ := launcherHarness(t)
	h.pair(forumChat, true)
	pinFails := true
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "pinChatMessage" && pinFails {
			return 502, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`, true
		}
		return 0, "", false
	}
	h.flush()
	h.tg.take()
	if h.bot.st.LauncherMsg == 0 || h.bot.st.LauncherPinned {
		t.Fatal("pin failure recorded as done")
	}
	pinFails = false
	h.bot.syncTopics(context.Background())
	if c := only(h.tg.take(), "pinChatMessage"); len(c) != 1 || !h.bot.st.LauncherPinned {
		t.Fatalf("pin not retried: %+v", c)
	}
	// Pinned and checked recently: no calls.
	h.bot.syncTopics(context.Background())
	if c := h.tg.take(); len(only(c, "pinChatMessage"))+len(only(c, "editMessageReplyMarkup")) != 0 {
		t.Fatalf("checked too often: %+v", c)
	}
	// Ten minutes later: checked (no-op edit) and re-pinned, which repairs an unpin.
	h.clock = h.clock.Add(launcherCheckEvery + time.Second)
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "editMessageReplyMarkup" {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified: specified new message content and reply markup are exactly the same"}`, true
		}
		return 0, "", false
	}
	h.bot.syncTopics(context.Background())
	if c := h.tg.take(); len(only(c, "editMessageReplyMarkup")) != 1 || len(only(c, "pinChatMessage")) != 1 || len(only(c, "createForumTopic")) != 0 {
		t.Fatalf("check: %+v", c)
	}
	// Deleted by hand: found on the next check and recreated.
	old := h.bot.st.LauncherThread
	h.clock = h.clock.Add(launcherCheckEvery + time.Second)
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "editMessageReplyMarkup" {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`, true
		}
		return 0, "", false
	}
	h.bot.syncTopics(context.Background())
	if h.bot.st.LauncherThread == 0 || h.bot.st.LauncherThread == old || !h.bot.st.LauncherPinned {
		t.Errorf("not recreated: %d (was %d)", h.bot.st.LauncherThread, old)
	}
}

// A double tap on ▶️ starts one session.
func TestTGLaunchDoubleTap(t *testing.T) {
	h, p := launcherHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	h.bot.draft = &launchDraft{step: 3, dir: p, prog: "claude", wss: []wsOption{{ID: "W1", Window: winA, Name: "EM"}}, ws: 0}
	m := &tgMessage{MessageID: h.bot.st.LauncherMsg, Chat: tgChat{ID: forumChat}, MessageThreadID: h.bot.st.LauncherThread, IsTopicMessage: true}
	var toasts []string
	for i := 0; i < 2; i++ {
		h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: fmt.Sprint(i), From: tgUser{ID: userID}, Message: m, Data: "L|go"}})
		for _, c := range only(h.tg.take(), "answerCallbackQuery") {
			toasts = append(toasts, str(c.Params["text"]))
		}
	}
	h.ag.args()
	h.flush()
	if a := h.ag.args(); strings.Count(a, "session new ") != 1 || fmt.Sprint(toasts) != "[Запускаю… Уже запускаю]" {
		t.Fatalf("%d launches, toasts %v", strings.Count(a, "session new "), toasts)
	}
}

// 3c. Someone typing into the new session before the launcher does (here: the
// web panel, while the launcher waits for the tree) makes it back off, even
// before agterm reports the new foreground.
func TestTGLaunchBacksOffAfterForeignInput(t *testing.T) {
	h, p := launcherHarness(t)
	h.pair(privChat, false)
	h.tg.take()
	script, _ := os.ReadFile(h.ag.path)
	s := strings.Replace(string(script), `    cp "$d/tree-after-new.json" "$d/tree.json"`, `    cp "$d/tree-after-new.json" "$d/tree.json"
    touch "$d/new-created"`, 1)
	s = strings.Replace(s, `  "tree --window") printf`, `  "tree --window")
    if [ -f "$d/new-created" ] && [ ! -f "$d/release-tree" ]; then
      touch "$d/tree-paused"
      while [ ! -f "$d/release-tree" ]; do sleep 0.01; done
    fi
    printf`, 1)
	os.WriteFile(h.ag.path, []byte(s), 0o755)
	done := make(chan struct{})
	go func() {
		h.bot.launch(context.Background(), h.bot.gen, launchReq{dir: p, prog: "claude", wsID: "W1", window: winA, chat: privChat})
		close(done)
	}()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(h.ag.dir, "tree-paused")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tree did not pause")
		}
	}
	if err := h.bot.srv.typeInto(context.Background(), winA, sidNew, "left", []string{"vim important.txt", "\r"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(h.ag.dir, "release-tree"), nil, 0o600)
	<-done
	typed := h.ag.typed()
	c := only(h.tg.take(), "sendMessage")
	if len(typed) != 2 || typed[0] != sidNew+"=vim important.txt" || len(c) != 1 || !strings.Contains(str(c[0].Params["text"]), "занята") {
		t.Fatalf("typed %q, report %+v", typed, c)
	}
}
