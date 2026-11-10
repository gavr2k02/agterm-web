package main

// Health of the Telegram connection: tracked by the client, written by the
// service to a small file (so the menu bar can show it even when Telegram is
// unreachable and the service is the only one who knows), and shown by /status.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	healthEvery = 10 * time.Second // the service rewrites the health file this often
	healthStale = 2 * time.Minute  // an older file means the service is not running
)

// tgHealth: no token, no URLs, no texts — only times and an error kind.
type tgHealth struct {
	LastOK      time.Time `json:"lastOk,omitzero"`      // last answer from Telegram
	FailStreak  int       `json:"failStreak,omitempty"` // consecutive failed calls
	FailSince   time.Time `json:"failSince,omitzero"`
	LastErrKind string    `json:"lastErrKind,omitempty"` // timeout | 5xx | 429 | network | auth | conflict

	// Polling on its own: other calls may succeed while getUpdates keeps
	// failing (another instance polling, a revoked token).
	PollStreak int       `json:"pollStreak,omitempty"`
	PollSince  time.Time `json:"pollSince,omitzero"`
	PollErr    string    `json:"pollErr,omitempty"`
}

// failing: Telegram has not answered lately, or polling keeps failing (one
// blip does not count).
func (h tgHealth) failing() bool {
	return h.FailStreak >= 2 || (h.FailStreak >= 1 && h.LastOK.IsZero()) || h.PollStreak >= 2
}

// problem: what is wrong and since when, polling first.
func (h tgHealth) problem() (string, time.Time) {
	if h.PollStreak >= 2 {
		return h.PollErr, h.PollSince
	}
	return h.LastErrKind, h.FailSince
}

// errKind classifies a failed call; "" means Telegram did answer (a 4xx is an
// answer: the connection is fine).
func errKind(httpErr error, code int) string {
	switch {
	case code == 401 || code == 404: // a revoked or wrong token: Telegram answers 401 or 404
		return "auth"
	case code == 409: // another getUpdates or a webhook
		return "conflict"
	case code == 429:
		return "429"
	case code >= 500:
		return "5xx"
	case code > 0:
		return ""
	}
	var ne net.Error
	if errors.Is(httpErr, context.DeadlineExceeded) || (errors.As(httpErr, &ne) && ne.Timeout()) {
		return "timeout"
	}
	return "network"
}

// record notes the outcome of one API call.
func (c *tgClient) record(method, kind string) {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	now := c.now()
	h := &c.health
	if method == "getUpdates" {
		if kind == "" {
			h.PollStreak, h.PollSince, h.PollErr = 0, time.Time{}, ""
		} else {
			if h.PollStreak == 0 {
				h.PollSince = now
			}
			h.PollStreak++
			h.PollErr = kind
		}
	}
	if kind == "" {
		h.LastOK, h.FailStreak, h.FailSince, h.LastErrKind = now, 0, time.Time{}, ""
		return
	}
	if h.FailStreak == 0 {
		h.FailSince = now
	}
	h.FailStreak++
	h.LastErrKind = kind
}

func (c *tgClient) Health() tgHealth {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	return c.health
}

/* ---------- the health file ---------- */

type tgHealthFile struct {
	UpdatedAt time.Time `json:"updatedAt"`
	StartedAt time.Time `json:"startedAt"`
	Paired    bool      `json:"paired"`
	Mode      string    `json:"mode,omitempty"`
	tgHealth
}

func tgHealthPath(tokenFile string) string {
	return filepath.Join(filepath.Dir(tokenFile), "telegram-health.json")
}

func (b *tgBot) writeHealth() {
	b.mu.Lock()
	f := tgHealthFile{UpdatedAt: b.now(), StartedAt: b.started, Paired: b.st.paired(), Mode: b.st.Mode, tgHealth: b.api.Health()}
	b.mu.Unlock()
	raw, _ := json.Marshal(f)
	writeFile0600(b.healthPath, raw)
}

func (b *tgBot) healthLoop(ctx context.Context) {
	b.writeHealth()
	t := time.NewTicker(healthEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.writeHealth()
		}
	}
}

// BotLine is the one-line bot state for the menu bar and the web panel.
type BotLine struct {
	Text string `json:"text"`
	Bad  bool   `json:"bad,omitempty"`
}

var errKindText = map[string]string{"timeout": "таймаут", "5xx": "ошибка Telegram", "429": "лимит запросов", "network": "сеть",
	"auth": "токен недействителен", "conflict": "бот запущен ещё где-то"}

// botLine reads the health file the service writes. tokenFile tells whether
// the bot is set up at all.
func botLine(tokenFile, healthFile string, now time.Time) *BotLine {
	if _, err := os.Stat(tokenFile); err != nil {
		return &BotLine{Text: "Telegram-бот: выключен"}
	}
	raw, err := os.ReadFile(healthFile)
	var f tgHealthFile
	if err != nil || json.Unmarshal(raw, &f) != nil || now.Sub(f.UpdatedAt) > healthStale {
		return &BotLine{Text: "Telegram-бот: ✗ служба не отвечает", Bad: true}
	}
	if f.failing() {
		kind, since := f.problem()
		switch kind {
		case "auth":
			return &BotLine{Text: "Telegram-бот: ✗ токен недействителен", Bad: true}
		case "conflict":
			return &BotLine{Text: "Telegram-бот: ✗ конфликт: бот запущен ещё где-то", Bad: true}
		}
		text := errKindText[kind]
		if text == "" {
			text = "ошибка"
		}
		return &BotLine{Text: fmt.Sprintf("Telegram-бот: ✗ нет связи с %s (%s)", localHM(since.Format(time.RFC3339), now), text), Bad: true}
	}
	if f.LastOK.IsZero() {
		return &BotLine{Text: "Telegram-бот: подключается…"}
	}
	if !f.Paired {
		return &BotLine{Text: "Telegram-бот: ✓ на связи, не привязан"}
	}
	return &BotLine{Text: "Telegram-бот: ✓ на связи"}
}

/* ---------- /status ---------- */

func durText(d time.Duration) string {
	m := int(d / time.Minute)
	switch {
	case m < 1:
		return "меньше минуты"
	case m < 60:
		return fmt.Sprintf("%dм", m)
	case m < 1440:
		return fmt.Sprintf("%dч %dм", m/60, m%60)
	}
	return fmt.Sprintf("%dд %dч", m/1440, m%1440/60)
}

// plural picks the Russian form for n: one (1, 21), few (2–4), many.
func plural(n int, one, few, many string) string {
	if n%10 == 1 && n%100 != 11 {
		return one
	}
	if n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14) {
		return few
	}
	return many
}

func (b *tgBot) statusText(ctx context.Context) string {
	now := b.now()
	clock := func(t time.Time) string { return t.In(time.Local).Format("15:04") }
	lines := []string{"ℹ️ Статус"}
	lines = append(lines, fmt.Sprintf("Mac: на связи · служба работает %s (с %s)", durText(now.Sub(b.started)), clock(b.started)))

	if all, err := b.sessions(ctx); err != nil {
		lines = append(lines, "agterm: недоступен ("+errText(err)+")")
	} else {
		n := map[string]int{}
		for _, s := range all {
			n[s.Status]++
		}
		var parts []string
		if c := n["blocked"]; c > 0 {
			parts = append(parts, fmt.Sprintf("🟠 %d %s", c, plural(c, "ждёт", "ждут", "ждут")))
		}
		if c := n["active"]; c > 0 {
			parts = append(parts, fmt.Sprintf("🔵 %d %s", c, plural(c, "работает", "работают", "работают")))
		}
		if c := n["completed"]; c > 0 {
			parts = append(parts, fmt.Sprintf("✅ %d готово", c))
		}
		if c := n["idle"]; c > 0 {
			parts = append(parts, fmt.Sprintf("⚪ %d", c))
		}
		line := fmt.Sprintf("agterm: %d %s", len(all), plural(len(all), "сессия", "сессии", "сессий"))
		if len(parts) > 0 {
			line += " — " + strings.Join(parts, " · ")
		}
		lines = append(lines, line)
	}

	h := b.api.Health()
	switch {
	case h.failing():
		kind, since := h.problem()
		lines = append(lines, fmt.Sprintf("Telegram: сбои с %s (%s)", clock(since), errKindText[kind]))
	case h.LastOK.IsZero():
		lines = append(lines, "Telegram: связь ок")
	default:
		lines = append(lines, fmt.Sprintf("Telegram: связь ок, последний ответ API %d с назад", int(now.Sub(h.LastOK)/time.Second)))
	}

	b.mu.Lock()
	st := *b.st
	b.mu.Unlock()
	where := "личный чат"
	if st.ChatID < 0 {
		where = "группа"
		if st.ChatTitle != "" {
			where += " «" + st.ChatTitle + "»"
		}
		if st.Mode == "forum" {
			where += " (темы)"
		}
	}
	if !st.PairedAt.IsZero() {
		where += ", с " + clock(st.PairedAt)
	}
	lines = append(lines, "Привязка: "+where)
	if now.Before(st.MutedUntil) {
		lines = append(lines, "Тишина: до "+clock(st.MutedUntil))
	} else {
		lines = append(lines, "Тишина: выкл")
	}
	b.mu.Lock()
	nm, un := len(b.mirrored), append([]string(nil), b.unmirrored...)
	b.mu.Unlock()
	if nm > 0 || len(un) > 0 {
		line := fmt.Sprintf("Зеркало: %d %s", nm, plural(nm, "сессия", "сессии", "сессий"))
		if len(un) > 0 {
			line += "; не зеркалится: " + strings.Join(un, ", ") + " (перезапусти сессию)"
		}
		lines = append(lines, line)
	}
	if b.srv.limits != nil {
		title, _ := menuTitle(b.srv.limits.Get(ctx), now)
		lines = append(lines, "Лимиты: "+title)
	}
	return strings.Join(lines, "\n")
}
