package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTGHealthTracking(t *testing.T) {
	f := newFakeTG(t)
	c := newTGClient(f.srv.URL, testTGToken)
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }
	reply := func(code int, body string) {
		f.hook = func(string, map[string]any) (int, string, bool) { return code, body, true }
	}
	call := func() { c.call(context.Background(), "getMe", nil, nil) }

	reply(200, `{"ok":true,"result":true}`)
	call()
	if h := c.Health(); !h.LastOK.Equal(clock) || h.FailStreak != 0 {
		t.Fatalf("ok: %+v", h)
	}
	clock = clock.Add(time.Minute)
	reply(502, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`)
	call()
	if h := c.Health(); h.FailStreak != 1 || h.LastErrKind != "5xx" || !h.FailSince.Equal(clock) || h.failing() {
		t.Fatalf("one 502 (a blip): %+v failing=%v", h, h.failing())
	}
	clock = clock.Add(time.Minute)
	reply(429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`)
	call()
	if h := c.Health(); h.FailStreak != 2 || h.LastErrKind != "429" || !h.FailSince.Equal(clock.Add(-time.Minute)) || !h.failing() {
		t.Fatalf("streak: %+v", h)
	}
	// A 4xx is an answer: the connection is fine.
	reply(400, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
	call()
	if h := c.Health(); h.FailStreak != 0 || !h.LastOK.Equal(clock) {
		t.Fatalf("400: %+v", h)
	}
	// Network failure and timeout.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	c2 := newTGClient(dead.URL, testTGToken)
	c2.call(context.Background(), "getMe", nil, nil)
	if h := c2.Health(); h.LastErrKind != "network" || h.FailStreak != 1 {
		t.Errorf("network: %+v", h)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	c3 := newTGClient(slow.URL, testTGToken)
	c3.http.Timeout = 50 * time.Millisecond
	c3.call(context.Background(), "getMe", nil, nil)
	if h := c3.Health(); h.LastErrKind != "timeout" {
		t.Errorf("timeout: %+v", h)
	}
	// Shutting down is not a failure.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c4 := newTGClient(slow.URL, testTGToken)
	c4.call(ctx, "getMe", nil, nil)
	if h := c4.Health(); h.FailStreak != 0 {
		t.Errorf("cancel counted: %+v", h)
	}
}

func TestBotLine(t *testing.T) {
	dir := t.TempDir()
	tok, hp := filepath.Join(dir, "telegram-token"), filepath.Join(dir, "telegram-health.json")
	now := time.Date(2026, 11, 9, 13, 0, 0, 0, time.Local)
	write := func(f tgHealthFile) {
		b, _ := json.Marshal(f)
		os.WriteFile(hp, b, 0o600)
	}
	check := func(want string, bad bool) {
		t.Helper()
		got := botLine(tok, hp, now)
		if got.Text != want || got.Bad != bad {
			t.Errorf("got %+v, want %q bad=%v", got, want, bad)
		}
	}
	check("Telegram-бот: выключен", false)
	os.WriteFile(tok, []byte(testTGToken), 0o600)
	check("Telegram-бот: ✗ служба не отвечает", true)
	write(tgHealthFile{UpdatedAt: now.Add(-3 * time.Minute), Paired: true, tgHealth: tgHealth{LastOK: now}})
	check("Telegram-бот: ✗ служба не отвечает", true)
	write(tgHealthFile{UpdatedAt: now.Add(-5 * time.Second), Paired: true, tgHealth: tgHealth{LastOK: now.Add(-time.Minute)}})
	check("Telegram-бот: ✓ на связи", false)
	write(tgHealthFile{UpdatedAt: now, Paired: false, tgHealth: tgHealth{LastOK: now}})
	check("Telegram-бот: ✓ на связи, не привязан", false)
	write(tgHealthFile{UpdatedAt: now})
	check("Telegram-бот: подключается…", false)
	since := time.Date(2026, 11, 9, 12, 1, 0, 0, time.Local)
	write(tgHealthFile{UpdatedAt: now, Paired: true, tgHealth: tgHealth{LastOK: since.Add(-time.Minute), FailStreak: 5, FailSince: since, LastErrKind: "timeout"}})
	check("Telegram-бот: ✗ нет связи с 12:01 (таймаут)", true)

	// In the menu bar (red) and in /api/limits.
	l := &Limits{FetchedAt: now.Format(time.RFC3339), Bot: botLine(tok, hp, now)}
	menu := SwiftBarMenu(l, now)
	if !strings.Contains(menu, "Telegram-бот: ✗ нет связи с 12:01 (таймаут) | color=#ff5a5a size=12\n") {
		t.Errorf("menu:\n%s", menu)
	}
	src := newLimits(t, filepath.Join(dir, "no-cswap"), dir)
	src.BotTokenFile, src.BotHealthFile = tok, hp
	if r := src.Collect(context.Background()); r.Bot == nil || !r.Bot.Bad {
		t.Errorf("collect: %+v", r.Bot)
	}
	if r := newLimits(t, filepath.Join(dir, "no-cswap"), dir).Collect(context.Background()); r.Bot != nil {
		t.Error("bot line without a bot set up")
	}
}

func TestTGHealthFile(t *testing.T) {
	h := newBotHarness(t)
	h.pair(forumChat, true)
	h.bot.api.record("getMe", "")
	h.bot.writeHealth()
	raw, err := os.ReadFile(h.bot.healthPath)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(h.bot.healthPath); st.Mode().Perm() != 0o600 {
		t.Errorf("perm %v", st.Mode().Perm())
	}
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "http") {
		t.Errorf("health file leaks: %s", raw)
	}
	var f tgHealthFile
	json.Unmarshal(raw, &f)
	if !f.Paired || f.Mode != "forum" || f.LastOK.IsZero() || !f.UpdatedAt.Equal(h.clock) {
		t.Errorf("health: %+v", f)
	}
}

func TestTGStatusCommand(t *testing.T) {
	h := newBotHarness(t)
	// Not paired: nobody gets an answer (no information leak).
	for _, text := range []string{"/status", btnStatus} {
		if c := h.send(h.msg(privChat, userID, text)); len(c) != 0 {
			t.Fatalf("answered before pairing: %+v", c)
		}
	}
	code, _ := newPairCode(h.bot.pairPath, h.clock)
	m := h.msg(forumChat, userID, "/pair "+code)
	m.Chat.IsForum, m.Chat.Title = true, "Workspace"
	h.send(m)
	h.flush()
	h.tg.take()
	if c := h.send(h.msg(forumChat, 999, "/status")); len(c) != 0 {
		t.Fatalf("answered a stranger: %+v", c)
	}
	h.bot.started = h.clock.Add(-(2*time.Hour + 14*time.Minute))
	h.bot.api.record("getMe", "")
	h.clock = h.clock.Add(3 * time.Second)
	h.bot.api.now = func() time.Time { return h.clock }
	p, _ := fakeCswap(t, cswapFixture, 0)
	h.bot.srv.limits = newLimits(t, p, t.TempDir())

	for _, text := range []string{"/status", btnStatus} {
		c := only(h.send(h.msg(forumChat, userID, text)), "sendMessage")
		if len(c) != 1 {
			t.Fatalf("%s: %+v", text, c)
		}
		got := str(c[0].Params["text"])
		hm := func(t time.Time) string { return t.In(time.Local).Format("15:04") }
		for _, want := range []string{
			"ℹ️ Статус\n",
			"Mac: на связи · служба работает 2ч 14м (с " + hm(h.bot.started) + ")\n",
			"agterm: 2 сессии — 🟠 1 ждёт · ⚪ 1\n",
			"Telegram: связь ок, последний ответ API",
			"Привязка: группа «Workspace» (темы), с " + hm(h.bot.st.PairedAt) + "\n",
			"Тишина: выкл\n",
			"Лимиты: Claude ",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s lacks %q:\n%s", text, want, got)
			}
		}
	}
	h.send(h.msg(forumChat, userID, btnMute))
	if got := str(only(h.send(h.msg(forumChat, userID, "/status")), "sendMessage")[0].Params["text"]); !strings.Contains(got, "Тишина: до ") {
		t.Errorf("mute: %s", got)
	}
}

func TestPluralAndDur(t *testing.T) {
	for n, want := range map[int]string{1: "сессия", 2: "сессии", 4: "сессии", 5: "сессий", 11: "сессий", 12: "сессий", 21: "сессия", 22: "сессии", 0: "сессий"} {
		if got := plural(n, "сессия", "сессии", "сессий"); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
	for d, want := range map[time.Duration]string{30 * time.Second: "меньше минуты", 5 * time.Minute: "5м", 134 * time.Minute: "2ч 14м", 27 * time.Hour: "1д 3ч"} {
		if got := durText(d); got != want {
			t.Errorf("%v: %s", d, got)
		}
	}
}
