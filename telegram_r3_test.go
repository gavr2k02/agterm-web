package main

// Regression tests for the third Codex audit of the Telegram bot.

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
)

// 1. A launch waiting for the pane lock stops if the bot is re-paired meanwhile.
func TestTGLaunchStopsOnRepairWhileWaiting(t *testing.T) {
	h, p := launcherHarness(t)
	h.pair(privChat, false)
	h.tg.take()
	gen := h.bot.gen
	mu := h.bot.srv.paneLock(winA, sidNew, "left")
	mu.Lock()
	done := make(chan struct{})
	go func() {
		h.bot.launch(context.Background(), gen, launchReq{dir: p, prog: "claude", wsID: "W1", window: winA, chat: privChat})
		close(done)
	}()
	select {
	case <-done:
		mu.Unlock()
		t.Fatal("did not wait for the pane")
	case <-time.After(300 * time.Millisecond):
	}
	code, _ := newPairCode(h.bot.pairPath, h.clock)
	h.send(h.msg(999, 999, "/pair "+code))
	h.tg.take()
	mu.Unlock()
	<-done
	if typed := h.ag.typed(); len(typed) != 0 || len(h.tg.take()) != 0 {
		t.Fatalf("old pairing's launch typed %q", typed)
	}
}

// 2. Sends queued behind a 429 cooldown keep their one-second spacing.
func TestTGOutboxNoBurstAfterCooldown(t *testing.T) {
	f := newFakeTG(t)
	o := newTGOutbox(newTGClient(f.srv.URL, testTGToken))
	o.groupMax = 0
	first, release, reserved := make(chan struct{}), make(chan struct{}), make(chan struct{}, 3)
	var mu sync.Mutex
	var at []time.Time
	f.hook = func(m string, p map[string]any) (int, string, bool) {
		mu.Lock()
		at = append(at, time.Now())
		n := len(at)
		mu.Unlock()
		if n == 1 {
			close(first)
			<-release
			return 429, `{"ok":false,"error_code":429,"parameters":{"retry_after":2}}`, true
		}
		return 0, "", false
	}
	o.sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case reserved <- struct{}{}:
		default:
		}
		return sleepCtx(ctx, d)
	}
	done := make(chan error, 3)
	go func() { done <- o.call(context.Background(), privChat, "sendMessage", map[string]any{}, nil) }()
	<-first
	for i := 0; i < 2; i++ {
		go func() { done <- o.call(context.Background(), privChat, "sendMessage", map[string]any{}, nil) }()
		<-reserved
	}
	close(release)
	for i := 0; i < 3; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 4 || at[1].Sub(at[0]) < 1900*time.Millisecond {
		t.Fatalf("calls at %v", at)
	}
	for i := 2; i < 4; i++ {
		if gap := at[i].Sub(at[i-1]); gap < 950*time.Millisecond {
			t.Errorf("gap %d after the cooldown: %v", i, gap)
		}
	}
}

// 3a. Concurrent launcher clicks and periodic checks do not race (run with -race).
func TestTGLauncherDraftNoRace(t *testing.T) {
	h, _ := launcherHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	h.clock = h.clock.Add(launcherCheckEvery + time.Second)
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "pinChatMessage" {
			return 502, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`, true
		}
		return 0, "", false
	}
	msg := &tgMessage{MessageID: h.bot.st.LauncherMsg, Chat: tgChat{ID: forumChat}, IsTopicMessage: true, MessageThreadID: h.bot.st.LauncherThread}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			h.bot.syncTopics(context.Background())
		}
	}()
	for i := 0; i < 20; i++ {
		data := []string{"L|d|o", "L|d|0", "L|p|claude", "L|r"}[i%4]
		h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: fmt.Sprint(i), From: tgUser{ID: userID}, Message: msg, Data: data}})
	}
	wg.Wait()
}

// 3b. A slow periodic check cannot leave older buttons than the user's last step.
func TestTGLauncherCheckKeepsNewestKeyboard(t *testing.T) {
	h, _ := launcherHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	h.clock = h.clock.Add(launcherCheckEvery + time.Second)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var final string
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "editMessageReplyMarkup" || m == "editMessageText" {
			if m == "editMessageReplyMarkup" {
				once.Do(func() { close(entered); <-release })
			}
			raw, _ := json.Marshal(p["reply_markup"])
			mu.Lock()
			final = string(raw)
			mu.Unlock()
		}
		return 0, "", false
	}
	done := make(chan struct{})
	go func() { h.bot.syncTopics(context.Background()); close(done) }()
	<-entered
	msg := &tgMessage{MessageID: h.bot.st.LauncherMsg, Chat: tgChat{ID: forumChat}, IsTopicMessage: true, MessageThreadID: h.bot.st.LauncherThread}
	h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: "c", From: tgUser{ID: userID}, Message: msg, Data: "L|d|0"}})
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(final, "L|p|claude") || strings.Contains(final, "L|d|0") {
		t.Fatalf("launcher ended with %s", final)
	}
}

// 4. A token Telegram rejects, or another poller, shows red — even while
// other calls succeed.
func TestTGPollingFailuresShowRed(t *testing.T) {
	for code, want := range map[int]string{401: "Telegram-бот: ✗ токен недействителен", 404: "Telegram-бот: ✗ токен недействителен", 409: "Telegram-бот: ✗ конфликт: бот запущен ещё где-то"} {
		h := newBotHarness(t)
		h.pair(privChat, false)
		tok := filepath.Join(h.dir, "telegram-token")
		os.WriteFile(tok, []byte(testTGToken), 0o600)
		h.bot.api.now = func() time.Time { return h.clock }
		h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
			if m == "getUpdates" {
				return code, fmt.Sprintf(`{"ok":false,"error_code":%d,"description":"x"}`, code), true
			}
			return 0, "", false
		}
		for i := 0; i < 3; i++ {
			h.bot.api.call(context.Background(), "getUpdates", map[string]any{}, nil)
			h.bot.api.call(context.Background(), "sendMessage", map[string]any{}, nil) // succeeds
		}
		h.bot.writeHealth()
		if line := botLine(tok, h.bot.healthPath, h.clock); !line.Bad || line.Text != want {
			t.Errorf("%d: %+v", code, line)
		}
		if s := h.bot.statusText(context.Background()); !strings.Contains(s, "Telegram: сбои") {
			t.Errorf("%d: status %s", code, s)
		}
	}
}

// /status is not answered to anyone but the owner in the paired chat.
func TestTGStatusUnauthorizedMatrix(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.flush()
	h.tg.take()
	for _, raw := range []string{
		fmt.Sprintf(`{"message":{"from":{"id":999},"chat":{"id":%d},"text":"/status"}}`, forumChat),
		fmt.Sprintf(`{"message":{"from":{"id":%d},"chat":{"id":555},"text":"/status"}}`, userID),
		fmt.Sprintf(`{"message":{"from":{"id":%d,"is_bot":true},"chat":{"id":%d},"text":"/status"}}`, userID, forumChat),
		fmt.Sprintf(`{"message":{"from":{"id":%d},"sender_chat":{"id":%d},"chat":{"id":%d},"text":"/status"}}`, userID, forumChat, forumChat),
		fmt.Sprintf(`{"edited_message":{"from":{"id":%d},"chat":{"id":%d},"text":"/status"}}`, userID, forumChat),
		fmt.Sprintf(`{"message":{"from":{"id":999},"forward_origin":{"type":"user","sender_user":{"id":%d}},"chat":{"id":%d},"text":"/status"}}`, userID, forumChat),
	} {
		h.bot.handleUpdate(context.Background(), rawUpdate(t, raw))
	}
	if c := h.tg.take(); len(c) != 0 {
		t.Fatal(c)
	}
}
