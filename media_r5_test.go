package main

// Regression tests for the fifth Codex audit (media).

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInboxRefusesSymlinks(t *testing.T) {
	h := mediaHarness(t, false)
	h.bot.st.Current = sidA
	outside := t.TempDir()
	os.Chmod(outside, 0o755)
	os.MkdirAll(h.bot.inbox, 0o700)
	os.Symlink(outside, filepath.Join(h.bot.inbox, "EM")) // the workspace level is a link
	h.file("doc", "content")
	c := texts(h.run(h.media(privChat, "document", tgFile{FileID: "doc", FileName: "report.txt"}, nil)), "sendMessage")
	if m, _ := filepath.Glob(filepath.Join(outside, "*", "*")); len(m) != 0 {
		t.Fatalf("written outside the inbox: %v", m)
	}
	if st, _ := os.Stat(outside); st.Mode().Perm() != 0o755 {
		t.Error("outside folder chmodded")
	}
	if len(c) != 1 || c[0] != "Не удалось сохранить файл." || h.ag.typed() != nil {
		t.Errorf("reply %q", c)
	}
	// The inbox root itself as a link is refused too.
	h2 := mediaHarness(t, false)
	os.Symlink(t.TempDir(), h2.bot.inbox)
	s, _ := h2.bot.findSession(context.Background(), sidA)
	if _, err := h2.bot.inboxDir(s); err == nil {
		t.Error("symlinked inbox root accepted")
	}
}

func TestConversionOutputsNotPlantable(t *testing.T) {
	h := mediaHarness(t, false)
	h.bot.st.Current = sidA
	s, _ := h.bot.findSession(context.Background(), sidA)
	dir, err := h.bot.inboxDir(s)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "unrelated.txt")
	os.WriteFile(victim, []byte("keep me"), 0o600)
	// Links at every name the old version wrote next to the download.
	for _, n := range []string{"-voice.16k.wav", "-voice.16k.txt", "-voice.oga", "-video.mp4-frames"} {
		os.Symlink(victim, filepath.Join(dir, stamp(h.clock)+n))
	}
	h.file("v", "voice")
	h.run(h.media(privChat, "voice", tgFile{FileID: "v", Duration: 1}, nil))
	h.file("vid", "video")
	h.run(h.media(privChat, "video", tgFile{FileID: "vid", Duration: 1}, nil))
	if b, _ := os.ReadFile(victim); string(b) != "keep me" {
		t.Fatalf("victim overwritten: %q", b)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".work-*")); len(m) != 0 {
		t.Errorf("work folders left: %v", m)
	}
	// An existing name (here a link) is never written through: a new name is used.
	h.file("x", "new")
	d := t.TempDir()
	os.Symlink(victim, filepath.Join(d, "x"))
	path, err := h.bot.download(context.Background(), tgFile{FileID: "x"}, d, "x")
	if err != nil || path == filepath.Join(d, "x") {
		t.Fatalf("download %q %v", path, err)
	}
}

func TestAlbumDroppedOnRepair(t *testing.T) {
	h := mediaHarness(t, false)
	h.bot.st.Current = sidA
	h.file("p", "JPEG")
	h.send(h.media(privChat, "photo", []tgFile{{FileID: "p", Width: 2, Height: 2}}, func(m *tgMessage) { m.MediaGroupID = "old" }))
	code, _ := newPairCode(h.bot.pairPath, h.clock)
	h.send(h.msg(999, 999, "/pair "+code))
	h.tg.take()
	h.flush()
	if typed := h.ag.typed(); typed != nil {
		t.Fatalf("old pairing's album typed %q", typed)
	}
}

func TestSlowAlbumStaysOne(t *testing.T) {
	h := mediaHarness(t, false)
	h.bot.st.Current = sidA
	h.file("p1", "1")
	h.file("p2", "2")
	h.bot.after = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	h.bot.api.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/docs/p2") {
			time.Sleep(1800 * time.Millisecond) // longer than albumWait
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	id := int64(80)
	photo := func(f string) *tgMessage {
		id++
		return h.media(privChat, "photo", []tgFile{{FileID: f, Width: 2, Height: 2}}, func(m *tgMessage) { m.MediaGroupID = "one"; m.MessageID = id })
	}
	h.send(photo("p1"))
	h.send(photo("p2"))
	// Wait until the fake has the whole submit (text, then Enter), plus a
	// while longer for a second submit that must not come.
	read := func() string {
		b, _ := os.ReadFile(filepath.Join(h.ag.dir, "typed"))
		return string(b)
	}
	for deadline := time.Now().Add(6 * time.Second); time.Now().Before(deadline) && !strings.Contains(read(), "=\r"); {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(albumWait + 300*time.Millisecond)
	typed := h.ag.typed()
	if len(typed) != 2 || strings.Count(typed[0], "-photo") != 2 {
		t.Fatalf("album split or lost: %q", typed)
	}
}

func r5Card(t *testing.T, h *botHarness) int64 {
	t.Helper()
	h.file("voice", "voice")
	edits := only(h.run(h.media(privChat, "voice", tgFile{FileID: "voice", Duration: 1}, nil)), "editMessageText")
	if len(edits) == 0 {
		t.Fatal("no card")
	}
	return num(edits[len(edits)-1].Params["message_id"])
}

func r5Click(h *botHarness, chat, user, card int64, action string) []tgCall {
	h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: "cb", From: tgUser{ID: user}, Message: &tgMessage{MessageID: card, Chat: tgChat{ID: chat}}, Data: fmt.Sprintf("v|%s|%d", action, card)}})
	return h.tg.take()
}

func TestStaleCorrectionGoesNowhere(t *testing.T) {
	h := mediaHarness(t, false)
	h.bot.st.Current = sidA
	card := r5Card(t, h)
	r5Click(h, privChat, userID, card, "e")
	prompt := h.bot.st.Cards[card].Prompts[0]
	r5Click(h, privChat, userID, card, "c")
	h.bot.st.Current = sidB
	m := h.msg(privChat, userID, "for A")
	m.ReplyTo = &tgMessage{MessageID: prompt}
	c := texts(h.send(m), "sendMessage")
	if h.ag.typed() != nil || len(c) != 1 || c[0] != "Эта расшифровка уже отменена." {
		t.Fatalf("stale correction: %q", c)
	}

	// A correction that is still open survives a restart and goes to its
	// card's session (A), whatever the current session is by then.
	h.bot.st.Current = sidA
	card = r5Card(t, h)
	h.bot.st.Current = sidB
	r5Click(h, privChat, userID, card, "e")
	prompt = h.bot.st.Cards[card].Prompts[0]
	old := h.bot
	b2, err := newTGBot(old.api, old.srv, filepath.Join(h.dir, "telegram-token"))
	if err != nil {
		t.Fatal(err)
	}
	b2.now, b2.after, b2.out = old.now, old.after, old.out
	h.bot = b2
	m = h.msg(privChat, userID, "исправлено")
	m.ReplyTo = &tgMessage{MessageID: prompt}
	h.send(m)
	if typed := h.ag.typed(); len(typed) != 2 || typed[0] != sidA+"=исправлено" {
		t.Fatalf("after restart: %q", typed)
	}
	// Once sent, a further reply is answered, not sent.
	m.Text = "ещё раз"
	if c := texts(h.send(m), "sendMessage"); h.ag.typed() != nil || len(c) != 1 || c[0] != "Эта расшифровка уже отправлена." {
		t.Errorf("second reply: %q", c)
	}
}

func TestCardsEndWithPairingAndAge(t *testing.T) {
	h := mediaHarness(t, false)
	h.bot.st.Current = sidA
	card := r5Card(t, h)
	code, _ := newPairCode(h.bot.pairPath, h.clock)
	h.send(h.msg(privChat, 999, "/pair "+code))
	h.tg.take()
	if a := only(r5Click(h, privChat, 999, card, "s"), "answerCallbackQuery"); len(a) != 1 || str(a[0].Params["text"]) != "Устарело" || h.ag.typed() != nil {
		t.Fatalf("old card used by the new owner: %+v", a)
	}

	h2 := mediaHarness(t, false)
	h2.bot.st.Current = sidA
	card = r5Card(t, h2)
	h2.clock = h2.clock.Add(30 * 24 * time.Hour)
	h2.bot.cleanInbox(h2.clock)
	if a := only(r5Click(h2, privChat, userID, card, "s"), "answerCallbackQuery"); str(a[0].Params["text"]) != "Устарело" || h2.ag.typed() != nil {
		t.Errorf("month-old card still works")
	}
}

func TestVideoExtractionFailure(t *testing.T) {
	h := mediaHarness(t, false)
	h.bot.st.Current = sidA
	h.file("broken", "corrupt")
	os.WriteFile(h.bot.tools.FFmpeg, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	calls := h.run(h.media(privChat, "video", tgFile{FileID: "broken", Duration: 5}, nil))
	typed := strings.Join(h.ag.typed(), "\n")
	if !strings.Contains(typed, "[Видео 0:05 из Telegram: "+h.bot.inbox) || !strings.Contains(typed, "-video.mp4]") {
		t.Fatalf("video path missing: %q", typed)
	}
	if e := texts(calls, "editMessageText"); len(e) != 1 || !strings.Contains(e[0], "⚠️ не удалось извлечь речь/кадры, отправил путь к видео") {
		t.Fatalf("card %q", e)
	}
}
