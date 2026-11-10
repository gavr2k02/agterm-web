package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mediaHarness: a paired bot with fake ffmpeg and whisper scripts and an inbox
// in the temp home. Settings in dir: "transcript" (what whisper hears),
// "scenes" (scene-change frames ffmpeg finds), "whisper-sleep" (seconds).
func mediaHarness(t *testing.T, forum bool) *botHarness {
	h := newBotHarness(t)
	if forum {
		h.pair(forumChat, true)
	} else {
		h.pair(privChat, false)
	}
	h.flush()
	h.tg.take()
	tools := t.TempDir()
	ffmpeg := `#!/bin/sh
d="` + tools + `"
printf '%s\n' "$*" >> "$d/ffmpeg.args"
for a in "$@"; do out="$a"; done
in=""; prev=""; for a in "$@"; do [ "$prev" = "-i" ] && in="$a"; prev="$a"; done
case "$out" in
  *.wav) grep -q NOAUDIO "$in" && exit 1; echo wav > "$out" ;;
  *scene-%02d.jpg) n=$(cat "$d/scenes" 2>/dev/null || echo 0); i=1; while [ $i -le $n ]; do printf x > "$(dirname "$out")/scene-0$i.jpg"; i=$((i+1)); done ;;
  *even-%02d.jpg) i=1; while [ $i -le 6 ]; do printf x > "$(dirname "$out")/even-0$i.jpg"; i=$((i+1)); done ;;
esac
`
	whisper := `#!/bin/sh
d="` + tools + `"
printf '%s\n' "$*" >> "$d/whisper.args"
[ -f "$d/whisper-sleep" ] && sleep "$(cat "$d/whisper-sleep")"
of=""; prev=""; for a in "$@"; do [ "$prev" = "-of" ] && of="$a"; prev="$a"; done
cat "$d/transcript" > "$of.txt"
`
	os.WriteFile(filepath.Join(tools, "ffmpeg"), []byte(ffmpeg), 0o755)
	os.WriteFile(filepath.Join(tools, "whisper"), []byte(whisper), 0o755)
	os.WriteFile(filepath.Join(tools, "transcript"), []byte(" привет,\n мир "), 0o644)
	h.bot.tools = mediaTools{FFmpeg: filepath.Join(tools, "ffmpeg"), Whisper: filepath.Join(tools, "whisper"), WhisperModel: "/models/ggml.bin"}
	h.bot.inbox = filepath.Join(h.home, "agterm-inbox")
	h.toolsDir = tools
	return h
}

func (h *botHarness) file(id string, body string) {
	h.tg.mu.Lock()
	h.tg.files["docs/"+id] = []byte(body)
	h.tg.mu.Unlock()
}

func (h *botHarness) media(chat int64, field string, v any, mod func(m *tgMessage)) *tgMessage {
	raw, _ := json.Marshal(v)
	m := h.msg(chat, userID, "")
	m.MessageID = 50
	switch field {
	case "voice":
		m.Voice = raw
	case "audio":
		m.Audio = raw
	case "video_note":
		m.VideoNote = raw
	case "video":
		m.Video = raw
	case "photo":
		m.Photo = raw
	case "document":
		m.Document = raw
	}
	if mod != nil {
		mod(m)
	}
	return m
}

// run sends a media message and runs the background work.
func (h *botHarness) run(m *tgMessage) []tgCall {
	calls := h.send(m)
	for i := 0; i < 3 && len(h.pending) > 0; i++ {
		h.flush()
	}
	return append(calls, h.tg.take()...)
}

func TestVoiceConfirmFlow(t *testing.T) {
	h := mediaHarness(t, false)
	h.bot.st.Current = sidA
	h.file("v1", "OggS voice")
	calls := h.run(h.media(privChat, "voice", tgFile{FileID: "v1", Duration: 4, FileSize: 10}, nil))
	if s := texts(calls, "sendMessage"); len(s) != 1 || s[0] != "🎙 расшифровываю…" {
		t.Fatalf("status %q", s)
	}
	edits := only(calls, "editMessageText")
	if len(edits) != 1 || str(edits[0].Params["text"]) != "🎙 «привет, мир»" || !strings.Contains(markup(edits[0]), "v|s|") {
		t.Fatalf("card %+v", edits)
	}
	card := num(edits[0].Params["message_id"])
	if h.ag.typed() != nil {
		t.Fatal("sent before ✅")
	}
	// Saved privately in the inbox.
	matches, _ := filepath.Glob(filepath.Join(h.bot.inbox, "EM", "Fix_bug-dddddddd", "*-voice.oga"))
	if len(matches) != 1 {
		t.Fatalf("saved: %v", matches)
	}
	if st, _ := os.Stat(matches[0]); st.Mode().Perm() != 0o600 {
		t.Errorf("file perm %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Dir(matches[0])); st.Mode().Perm() != 0o700 {
		t.Errorf("dir perm %v", st.Mode().Perm())
	}
	args, _ := os.ReadFile(filepath.Join(h.toolsDir, "whisper.args"))
	if !strings.Contains(string(args), "-m /models/ggml.bin") || !strings.Contains(string(args), "-l auto") || !strings.Contains(string(args), "-nt") {
		t.Errorf("whisper args %s", args)
	}
	ff, _ := os.ReadFile(filepath.Join(h.toolsDir, "ffmpeg.args"))
	if !strings.Contains(string(ff), "-ar 16000 -ac 1") {
		t.Errorf("ffmpeg args %s", ff)
	}

	click := func(data string) string {
		msg := &tgMessage{MessageID: card, Chat: tgChat{ID: privChat}}
		h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: "c", From: tgUser{ID: userID}, Message: msg, Data: data}})
		a := only(h.tg.take(), "answerCallbackQuery")
		return str(a[0].Params["text"])
	}
	if got := click(fmt.Sprintf("v|s|%d", card)); got != "Отправлено" {
		t.Fatalf("send toast %q", got)
	}
	if typed := h.ag.typed(); len(typed) != 2 || typed[0] != sidA+"=привет, мир" {
		t.Fatalf("typed %q", typed)
	}
	if !h.bot.fromTelegram(sidA, "left", "привет, мир") {
		t.Error("sent text not registered for echo suppression")
	}
	if got := click(fmt.Sprintf("v|s|%d", card)); got != "Устарело" || h.ag.typed() != nil {
		t.Errorf("second ✅: %q", got)
	}
}

func TestVoiceCorrectAndCancel(t *testing.T) {
	h := mediaHarness(t, true)
	thread := h.bot.st.Topics[sidA].Thread
	in := func(m *tgMessage) { m.IsTopicMessage, m.MessageThreadID = true, thread; m.Chat.ID = forumChat }
	h.file("v2", "voice")
	calls := h.run(h.media(forumChat, "voice", tgFile{FileID: "v2", Duration: 2}, in))
	card := num(only(calls, "editMessageText")[0].Params["message_id"])
	click := func(data string) []tgCall {
		msg := &tgMessage{MessageID: card, Chat: tgChat{ID: forumChat}}
		h.bot.handleUpdate(context.Background(), tgUpdate{Callback: &tgCallback{ID: "c", From: tgUser{ID: userID}, Message: msg, Data: data}})
		return h.tg.take()
	}
	ask := only(click(fmt.Sprintf("v|e|%d", card)), "sendMessage")
	if len(ask) != 1 || !strings.Contains(str(ask[0].Params["text"]), "исправленный") || num(ask[0].Params["message_thread_id"]) != thread {
		t.Fatalf("ask %+v", ask)
	}
	// A reply to the bot's request replaces the transcript.
	fix := h.msg(forumChat, userID, "исправленный текст")
	in(fix)
	fix.ReplyTo = &tgMessage{MessageID: num(ask[0].Params["message_id"])}
	if ask[0].Params["message_id"] == nil {
		fix.ReplyTo.MessageID = card
	}
	calls = h.send(fix)
	if typed := h.ag.typed(); len(typed) != 2 || typed[0] != sidA+"=исправленный текст" {
		t.Fatalf("typed %q", typed)
	}
	if e := texts(calls, "editMessageText"); len(e) != 1 || !strings.Contains(e[0], "✏️ «исправленный текст»") {
		t.Errorf("card %q", e)
	}

	// ✖ on another card.
	h.file("v3", "voice")
	calls = h.run(h.media(forumChat, "voice", tgFile{FileID: "v3", Duration: 2}, in))
	card = num(only(calls, "editMessageText")[0].Params["message_id"])
	if e := texts(click(fmt.Sprintf("v|c|%d", card)), "editMessageText"); len(e) != 1 || !strings.HasSuffix(e[0], "отменено") || h.ag.typed() != nil {
		t.Fatalf("cancel %q", e)
	}
}

func TestVoiceAutoSendAndTimeout(t *testing.T) {
	h := mediaHarness(t, false)
	h.bot.st.Current = sidA
	os.WriteFile(h.bot.settingsPath, []byte(`{"voiceAutoSend": true}`), 0o600)
	h.file("a1", "mp3")
	calls := h.run(h.media(privChat, "audio", tgFile{FileID: "a1", Duration: 3, FileName: "note.m4a"}, nil))
	if typed := h.ag.typed(); len(typed) != 2 || typed[0] != sidA+"=привет, мир" {
		t.Fatalf("typed %q", typed)
	}
	if e := texts(calls, "editMessageText"); len(e) != 1 || !strings.Contains(e[0], "→ отправлено") {
		t.Fatalf("card %q", e)
	}
	if m, _ := filepath.Glob(filepath.Join(h.bot.inbox, "*", "*", "*-audio.m4a")); len(m) != 1 {
		t.Errorf("audio kept its extension: %v", m)
	}
	// Whisper stuck: the timeout (2 × duration + slack) ends it.
	os.WriteFile(filepath.Join(h.toolsDir, "whisper-sleep"), []byte("5"), 0o644)
	h.bot.speechSlack = 300 * time.Millisecond
	h.file("a2", "mp3")
	start := time.Now()
	calls = h.run(h.media(privChat, "voice", tgFile{FileID: "a2", Duration: 0}, nil))
	if time.Since(start) > 4*time.Second {
		t.Errorf("timeout not enforced: %v", time.Since(start))
	}
	if e := texts(calls, "editMessageText"); len(e) != 1 || !strings.Contains(e[0], "не удалось расшифровать") || h.ag.typed() != nil {
		t.Errorf("timeout card %q", e)
	}
}

func TestVideoNoteAutoSend(t *testing.T) {
	h := mediaHarness(t, true)
	thread := h.bot.st.Topics[sidA].Thread
	in := func(m *tgMessage) {
		m.IsTopicMessage, m.MessageThreadID, m.Chat.ID = true, thread, forumChat
		m.Caption = "смотри"
	}
	os.WriteFile(filepath.Join(h.toolsDir, "scenes"), []byte("3"), 0o644)
	h.file("vn", "mp4")
	calls := h.run(h.media(forumChat, "video_note", tgFile{FileID: "vn", Duration: 42}, in))
	if s := texts(calls, "sendMessage"); len(s) != 1 || s[0] != "🎥 обрабатываю…" {
		t.Fatalf("status %q", s)
	}
	// The fake logs one line per line typed: put the paste back together.
	full := strings.Join(h.ag.typed(), "\n")
	if !strings.HasPrefix(full, sidA+"="+pasteStart) || !strings.HasSuffix(full, pasteEnd+"\n"+sidA+"=\r") {
		t.Fatalf("not one bracketed paste + Enter: %q", full)
	}
	sent := strings.TrimSuffix(strings.TrimPrefix(full, sidA+"="+pasteStart), pasteEnd+"\n"+sidA+"=\r")
	lines := strings.Split(sent, "\n")
	if !strings.HasPrefix(lines[0], "[Видео 0:42 из Telegram: "+h.bot.inbox) || !strings.HasSuffix(lines[0], "-video.mp4]") ||
		lines[1] != "Речь: «привет, мир»" || lines[2] != "Кадры (посмотри их):" ||
		!strings.Contains(lines[3], "-video.mp4-frames-") || len(lines) != 7 || lines[6] != "Подпись: смотри" {
		t.Fatalf("sent %q", lines)
	}
	if e := texts(calls, "editMessageText"); len(e) != 1 || !strings.HasPrefix(e[0], "🎥 0:42 · кадров: 3 · отправлено\n«привет, мир»") {
		t.Fatalf("card %q", e)
	}
	// No scene changes: evenly spaced frames; no sound track: no "Речь".
	os.WriteFile(filepath.Join(h.toolsDir, "scenes"), []byte("0"), 0o644)
	h.file("v2", "NOAUDIO mp4")
	h.run(h.media(forumChat, "video", tgFile{FileID: "v2", Duration: 12, FileName: "clip.mov"}, in))
	full = strings.Join(h.ag.typed(), "\n")
	if strings.Contains(full, "Речь") || strings.Count(full, "even-0") != 6 || !strings.Contains(full, ".mov") {
		t.Fatalf("fallback %q", full)
	}
	// videoAutoSend off: a card instead.
	os.WriteFile(h.bot.settingsPath, []byte(`{"videoAutoSend": false}`), 0o600)
	h.file("v3", "mp4")
	calls = h.run(h.media(forumChat, "video", tgFile{FileID: "v3", Duration: 5}, in))
	if h.ag.typed() != nil || !strings.Contains(markup(only(calls, "editMessageText")[0]), "v|s|") {
		t.Fatalf("card expected: %+v", calls)
	}
}

func TestPhotosFilesAndAlbums(t *testing.T) {
	h := mediaHarness(t, false)
	h.bot.st.Current = sidA
	h.file("small", "s")
	h.file("big", "JPEGDATA")
	h.run(h.media(privChat, "photo", []tgFile{{FileID: "small", Width: 90, Height: 90}, {FileID: "big", Width: 1280, Height: 960}}, func(m *tgMessage) { m.Caption = "экран" }))
	typed := h.ag.typed()
	if len(typed) != 2 || !strings.HasPrefix(typed[0], sidA+"=[Фото из Telegram: "+h.bot.inbox) || !strings.HasSuffix(typed[0], "-photo.jpg] экран") {
		t.Fatalf("photo %q", typed)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(typed[0], sidA+"=[Фото из Telegram: "), "] экран")
	if b, _ := os.ReadFile(path); string(b) != "JPEGDATA" {
		t.Error("not the largest size")
	}

	// A document with a hostile name.
	h.file("doc", "%PDF")
	h.run(h.media(privChat, "document", tgFile{FileID: "doc", FileName: "../../etc/pass wd;rm.PDF", FileSize: 4, MimeType: "application/pdf"}, nil))
	typed = h.ag.typed()
	if len(typed) != 2 || !strings.Contains(typed[0], "[Файл из Telegram: "+h.bot.inbox) || !strings.Contains(typed[0], "-pass_wd_rm.pdf (pass_wd_rm.pdf, 4 Б)]") || strings.Contains(typed[0], "..") {
		t.Fatalf("document %q", typed)
	}
	// An image sent as a file is a photo, keeping its extension.
	h.file("png", "PNG")
	h.run(h.media(privChat, "document", tgFile{FileID: "png", FileName: "shot.png", MimeType: "image/png"}, nil))
	if typed = h.ag.typed(); len(typed) != 2 || !strings.Contains(typed[0], "[Фото из Telegram: ") || !strings.HasSuffix(typed[0], "-shot.png]") {
		t.Fatalf("image document %q", typed)
	}

	// Album: one message after the wait, all paths.
	h.file("p1", "1")
	h.file("p2", "2")
	msgID := int64(70)
	album := func(id string) *tgMessage {
		msgID++
		return h.media(privChat, "photo", []tgFile{{FileID: id, Width: 10, Height: 10}}, func(m *tgMessage) { m.MediaGroupID = "G1"; m.MessageID = msgID })
	}
	h.send(album("p1"))
	m2 := album("p2")
	m2.Caption = "два"
	h.send(m2)
	if h.ag.typed() != nil {
		t.Fatal("album sent before the wait")
	}
	h.flush()
	typed = h.ag.typed()
	if len(typed) != 2 || strings.Count(typed[0], "-photo") != 2 || !strings.HasPrefix(typed[0], sidA+"=[Фото из Telegram: ") || !strings.HasSuffix(typed[0], "] два") {
		t.Fatalf("album %q", typed)
	}
}

func TestMediaLimitsAndErrors(t *testing.T) {
	h := mediaHarness(t, false)
	h.bot.st.Current = sidA
	// Over 20 MB by the message's own size: not even asked for.
	c := texts(h.run(h.media(privChat, "document", tgFile{FileID: "huge", FileName: "x.bin", FileSize: 21 << 20}, nil)), "sendMessage")
	if len(c) != 1 || !strings.Contains(c[0], "больше 20 МБ") {
		t.Fatalf("size: %q", c)
	}
	// Telegram refuses getFile as too big.
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "getFile" {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: file is too big"}`, true
		}
		return 0, "", false
	}
	c = texts(h.run(h.media(privChat, "document", tgFile{FileID: "x", FileName: "x.bin"}, nil)), "sendMessage")
	if len(c) != 1 || !strings.Contains(c[0], "больше 20 МБ") {
		t.Fatalf("too big: %q", c)
	}
	h.tg.hook = nil
	// Download errors never carry the token.
	dead := httptest.NewServer(nil)
	dead.Close()
	f := newFakeTG(t)
	f.files["docs/z"] = []byte("z")
	h.bot.api = newTGClient(f.srv.URL, testTGToken)
	h.bot.api.base = dead.URL // getFile answers, the download fails
	_, err := h.bot.download(context.Background(), tgFile{FileID: "z"}, t.TempDir(), "z")
	if err == nil || strings.Contains(err.Error(), testTGToken) {
		t.Errorf("download error: %v", err)
	}
	// Strangers' files are not touched.
	h2 := mediaHarness(t, false)
	h2.file("s", "x")
	if c := h2.run(h2.media(privChat, "document", tgFile{FileID: "s", FileName: "s"}, func(m *tgMessage) { m.From.ID = 999 })); len(c) != 0 {
		t.Errorf("stranger's file handled: %+v", c)
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"report.PDF":                      "report.pdf",
		"../../etc/passwd":                "passwd",
		"..\\..\\win.ini":                 "win.ini",
		".hidden":                         "hidden",
		"Отчёт за май (1).docx":           "Отчёт_за_май_1.docx",
		"a;b|c$d.tar.gz":                  "a_b_c_d.tar.gz",
		"":                                "file",
		strings.Repeat("я", 200) + ".txt": strings.Repeat("я", 80) + ".txt",
		"evil.ex e":                       "evil.ex_e",
	} {
		if got := safeName(in, "file"); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInboxCleanup(t *testing.T) {
	h := mediaHarness(t, false)
	old := filepath.Join(h.bot.inbox, "EM", "s-1", "old.jpg")
	fresh := filepath.Join(h.bot.inbox, "EM", "s-2", "new.jpg")
	for _, p := range []string{old, fresh} {
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte("x"), 0o600)
	}
	os.Chtimes(old, time.Now().Add(-8*24*time.Hour), time.Now().Add(-8*24*time.Hour))
	h.bot.cleanInbox(time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old file kept")
	}
	if _, err := os.Stat(filepath.Dir(old)); !os.IsNotExist(err) {
		t.Error("empty folder kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh file deleted")
	}
	os.WriteFile(h.bot.settingsPath, []byte(`{"inboxKeepDays": 1}`), 0o600)
	os.Chtimes(fresh, time.Now().Add(-2*24*time.Hour), time.Now().Add(-2*24*time.Hour))
	h.bot.cleanInbox(time.Now())
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Error("keep setting ignored")
	}
}
