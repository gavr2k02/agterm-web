package main

// Voice, audio, video, photos and files from Telegram into agterm sessions.
//
// Files are downloaded into ~/agterm-inbox/<workspace>/<session>/ (0700/0600)
// and the session gets a line pointing at them. Voice and audio are
// transcribed (ffmpeg → 16 kHz wav → whisper-cli) and wait for a confirmation
// card unless "voiceAutoSend" is on; videos are attachments: transcript of the
// sound track plus keyframes, sent right away unless "videoAutoSend" is off.
// Photos and files are sent right away; an album becomes one message.
// Nothing about the contents is logged.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	tgDownloadMax  = 20 << 20 // Telegram does not let bots download more
	albumWait      = 1500 * time.Millisecond
	inboxKeepDays  = 7
	ffmpegTimeout  = 2 * time.Minute
	maxFrames      = 8
	fallbackFrames = 6
	frameMaxSide   = 1280
	transcriptShow = 300 // characters of a transcript on a card
)

var errTooBig = errors.New("file bigger than 20 MB")

type tgFile struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	Duration     int    `json:"duration"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
}

type mediaSettings struct {
	VoiceAutoSend *bool   `json:"voiceAutoSend"`
	VideoAutoSend *bool   `json:"videoAutoSend"`
	InboxKeepDays *int    `json:"inboxKeepDays"`
	VoiceLanguage *string `json:"voiceLanguage"`
}

func (b *tgBot) mediaSettings() (voiceAuto, videoAuto bool, keepDays int, lang string) {
	voiceAuto, videoAuto, keepDays, lang = false, true, inboxKeepDays, "auto"
	raw, err := os.ReadFile(b.settingsPath)
	if err != nil {
		return
	}
	var s mediaSettings
	if json.Unmarshal(raw, &s) != nil {
		return
	}
	if s.VoiceAutoSend != nil {
		voiceAuto = *s.VoiceAutoSend
	}
	if s.VideoAutoSend != nil {
		videoAuto = *s.VideoAutoSend
	}
	if s.InboxKeepDays != nil && *s.InboxKeepDays > 0 {
		keepDays = *s.InboxKeepDays
	}
	if s.VoiceLanguage != nil && regexp.MustCompile(`^[a-z]{2,3}$|^auto$`).MatchString(*s.VoiceLanguage) {
		lang = *s.VoiceLanguage
	}
	return
}

// mediaTools: the programs media needs (absolute paths: launchd's PATH is minimal).
type mediaTools struct {
	FFmpeg, Whisper, WhisperModel string
}

func defaultTool(name string, candidates ...string) string {
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return candidates[0]
}

/* ---------- files ---------- */

var reUnsafe = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)

// safeName keeps a file name readable but harmless: no directories, no
// leading dots, letters/digits/._- only, at most 80 characters, extension kept.
func safeName(name, fallback string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	ext, stem := strings.ToLower(filepath.Ext(name)), strings.TrimSuffix(name, filepath.Ext(name))
	if len(ext) > 10 || !regexp.MustCompile(`^\.[a-z0-9]+$`).MatchString(ext) {
		ext, stem = "", name // not an extension: part of the name
	}
	stem = strings.Trim(reUnsafe.ReplaceAllString(stem, "_"), "._-")
	if stem == "" && ext != "" { // ".hidden"
		stem, ext = ext[1:], ""
	}
	if r := []rune(stem); len(r) > 80 {
		stem = string(r[:80])
	}
	if stem == "" {
		stem = fallback
	}
	return stem + ext
}

var errInboxLink = errors.New("inbox: a symlink or a file where a folder should be")

// inboxDir: ~/agterm-inbox/<workspace>/<session name>-<id prefix>/. Each
// level is checked without following links: a symlink anywhere on the way is
// refused (writes must stay inside the inbox), missing levels are made 0700.
func (b *tgBot) inboxDir(s *sessionRef) (string, error) {
	ws := safeName(s.Workspace, "workspace")
	sess := safeName(truncRunes(sessionName(s.Session), 30), "session") + "-" + strings.ToLower(s.ID[:8])
	dir := b.inbox
	for _, part := range []string{"", ws, sess} {
		if part != "" {
			dir = filepath.Join(dir, part)
		}
		st, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			if st, err = os.Lstat(dir); err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return "", errInboxLink
		}
	}
	return dir, nil
}

// workDir: a fresh private folder (random name, 0700) inside dir for
// conversion outputs; the caller removes it.
func workDir(dir string) (string, error) { return os.MkdirTemp(dir, ".work-") }

// download fetches a Telegram file into dir/name (0600, never overwriting).
// Errors never carry the token (it is in the download URL).
func (b *tgBot) download(ctx context.Context, f tgFile, dir, name string) (string, error) {
	if f.FileSize > tgDownloadMax {
		return "", errTooBig
	}
	var info struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := b.api.call(ctx, "getFile", map[string]any{"file_id": f.FileID}, &info); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "too big") {
			return "", errTooBig
		}
		return "", err
	}
	if info.FileSize > tgDownloadMax {
		return "", errTooBig
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.api.base+"/file/bot"+b.api.token+"/"+info.FilePath, nil)
	if err != nil {
		return "", errors.New(b.api.redact(err.Error()))
	}
	resp, err := b.api.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %s", b.api.redact(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	path := filepath.Join(dir, name)
	for i := 2; ; i++ { // two files in the same second
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			break
		}
		ext := filepath.Ext(name)
		path = filepath.Join(dir, fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), i, ext))
	}
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, tgDownloadMax+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > tgDownloadMax {
		err = errTooBig
	}
	if err != nil {
		os.Remove(path)
		return "", fmt.Errorf("download: %s", b.api.redact(err.Error()))
	}
	return path, nil
}

func stamp(t time.Time) string { return t.Local().Format("20060102-150405") }

func sizeText(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " МБ"
	case n >= 1<<10:
		return strconv.FormatInt(n>>10, 10) + " КБ"
	}
	return strconv.FormatInt(n, 10) + " Б"
}

func durText2(sec int) string { return fmt.Sprintf("%d:%02d", sec/60, sec%60) }

/* ---------- dispatch ---------- */

func parseFile(raw json.RawMessage) *tgFile {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var f tgFile
	if json.Unmarshal(raw, &f) != nil || f.FileID == "" {
		return nil
	}
	return &f
}

// handleMedia routes a media message like text and does the slow part in
// the background, so polling goes on.
func (b *tgBot) handleMedia(ctx context.Context, m *tgMessage) {
	if b.voiceCorrection(ctx, m) {
		return
	}
	s, pane, ok := b.resolveTarget(ctx, m)
	if !ok {
		return
	}
	b.mu.Lock()
	gen := b.gen
	b.mu.Unlock()
	run := func(f func(context.Context)) {
		b.after(0, func() {
			ctx, cancel := b.pairingContext(b.ctx, gen)
			defer cancel()
			f(ctx)
		})
	}
	switch {
	case parseFile(m.Voice) != nil:
		f := parseFile(m.Voice)
		run(func(ctx context.Context) { b.handleSpeech(ctx, m, s, pane, *f, "voice", ".oga") })
	case parseFile(m.Audio) != nil:
		f := parseFile(m.Audio)
		ext := strings.ToLower(filepath.Ext(f.FileName))
		if ext == "" {
			ext = ".mp3"
		}
		run(func(ctx context.Context) { b.handleSpeech(ctx, m, s, pane, *f, "audio", ext) })
	case parseFile(m.VideoNote) != nil:
		f := parseFile(m.VideoNote)
		run(func(ctx context.Context) { b.handleVideo(ctx, m, s, pane, *f) })
	case parseFile(m.Video) != nil:
		f := parseFile(m.Video)
		run(func(ctx context.Context) { b.handleVideo(ctx, m, s, pane, *f) })
	case len(m.Photo) > 0 && string(m.Photo) != "null":
		var sizes []tgFile
		json.Unmarshal(m.Photo, &sizes)
		if len(sizes) == 0 {
			return
		}
		best := sizes[0]
		for _, z := range sizes {
			if z.Width*z.Height > best.Width*best.Height {
				best = z
			}
		}
		b.handleAttachment(ctx, m, s, pane, best, "photo", ".jpg", true)
	case parseFile(m.Document) != nil:
		f := parseFile(m.Document)
		b.handleAttachment(ctx, m, s, pane, *f, "file", "", strings.HasPrefix(f.MimeType, "image/"))
	default:
		b.reply(ctx, m, "Такое пока не умею: пришли текст, голосовое, видео, фото или файл.")
	}
}

/* ---------- photos and files ---------- */

// album collects the parts of a Telegram media group. Parts are registered
// before they download, so a slow download keeps the album open; it is sent
// once nothing is downloading and nothing new came for albumWait. It belongs
// to the pairing it started under.
type album struct {
	gen         uint64
	s           *sessionRef
	pane        string
	first       *tgMessage
	items       map[int64]string // by message id: "Фото …" / "Файл …"
	captions    map[int64]string
	outstanding int // parts still downloading
	seq         int // bumped on every change; a check only flushes the latest
}

// handleAttachment saves a photo or file and sends its path to the session
// right away; parts of an album are collected and sent as one.
func (b *tgBot) handleAttachment(ctx context.Context, m *tgMessage, s *sessionRef, pane string, f tgFile, kind, ext string, image bool) {
	if f.FileSize > tgDownloadMax {
		b.reply(ctx, m, "Файл больше 20 МБ, Telegram не даёт боту его скачать.")
		return
	}
	group := m.MediaGroupID
	if group != "" {
		b.albumPart(group, m, s, pane, "", false) // register before the download
	}
	item, ok := b.saveAttachment(ctx, m, s, f, kind, ext, image)
	if group != "" {
		b.albumPart(group, m, s, pane, item, true)
		return
	}
	if ok {
		b.sendAttachments(ctx, m, s, pane, []string{item}, strings.TrimSpace(m.Caption))
	}
}

func (b *tgBot) saveAttachment(ctx context.Context, m *tgMessage, s *sessionRef, f tgFile, kind, ext string, image bool) (string, bool) {
	dir, err := b.inboxDir(s)
	if err != nil {
		log.Printf("telegram: inbox: %v", err)
		b.reply(ctx, m, "Не удалось сохранить файл.")
		return "", false
	}
	name := stamp(b.now()) + "-" + kind + ext
	if kind == "file" {
		name = stamp(b.now()) + "-" + safeName(f.FileName, "file")
	}
	path, err := b.download(ctx, f, dir, name)
	if errors.Is(err, errTooBig) {
		b.reply(ctx, m, "Файл больше 20 МБ, Telegram не даёт боту его скачать.")
		return "", false
	}
	if err != nil {
		log.Printf("telegram: %v", err)
		b.reply(ctx, m, "Не удалось скачать файл.")
		return "", false
	}
	if !image {
		return fmt.Sprintf("Файл из Telegram: %s (%s, %s)", path, safeName(f.FileName, "file"), sizeText(f.FileSize)), true
	}
	return "Фото из Telegram: " + path, true
}

// albumPart registers a part (done=false) or its finished download (done=true,
// item "" when it failed), then schedules a check.
func (b *tgBot) albumPart(group string, m *tgMessage, s *sessionRef, pane, item string, done bool) {
	b.mu.Lock()
	a := b.albums[group]
	if a == nil {
		a = &album{gen: b.gen, s: s, pane: pane, first: m, items: map[int64]string{}, captions: map[int64]string{}}
		b.albums[group] = a
	}
	if done {
		a.outstanding--
		if item != "" {
			a.items[m.MessageID] = item
		}
		if c := strings.TrimSpace(m.Caption); c != "" {
			a.captions[m.MessageID] = c
		}
	} else {
		a.outstanding++
	}
	a.seq++
	seq := a.seq
	b.mu.Unlock()
	if done {
		b.after(albumWait, func() { b.albumCheck(group, seq) })
	}
}

func (b *tgBot) albumCheck(group string, seq int) {
	b.mu.Lock()
	a := b.albums[group]
	if a == nil || a.seq != seq || a.outstanding > 0 || b.albums[group] != a {
		b.mu.Unlock()
		return
	}
	delete(b.albums, group)
	stale := a.gen != b.gen
	b.mu.Unlock()
	if stale || len(a.items) == 0 { // re-paired meanwhile, or nothing downloaded
		return
	}
	ctx, cancel := b.pairingContext(b.ctx, a.gen)
	defer cancel()
	ids := make([]int64, 0, len(a.items))
	for id := range a.items {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var items, caps []string
	for _, id := range ids {
		items = append(items, a.items[id])
		if c := a.captions[id]; c != "" {
			caps = append(caps, c)
		}
	}
	if !b.genIs(a.gen) {
		return
	}
	b.sendAttachments(ctx, a.first, a.s, a.pane, items, strings.Join(caps, "\n"))
}

func (b *tgBot) sendAttachments(ctx context.Context, m *tgMessage, s *sessionRef, pane string, items []string, caption string) {
	text := "[" + strings.Join(items, "; ") + "]"
	if len(items) > 1 && strings.HasPrefix(items[0], "Фото") {
		var paths []string
		for _, it := range items {
			paths = append(paths, strings.TrimPrefix(it, "Фото из Telegram: "))
		}
		text = "[Фото из Telegram: " + strings.Join(paths, ", ") + "]"
	}
	if caption != "" {
		text += " " + caption
	}
	if err := b.submitTo(ctx, s, pane, text); err != nil {
		b.reply(ctx, m, "→ не отправлено: "+errText(err))
		return
	}
	b.ack(ctx, m, s)
}

/* ---------- speech ---------- */

// tgCard is a confirmation card (✅ / ✏️ / ✖), saved with the state. Once
// sent or cancelled it stays as a tombstone for a while, so a late reply to
// it (or to its "пришли исправленный текст") is answered, not sent somewhere
// else.
type tgCard struct {
	Session string    `json:"session"`
	Pane    string    `json:"pane"`
	Text    string    `json:"text"`  // what ✅ sends
	Shown   string    `json:"shown"` // the card's first line(s)
	Chat    int64     `json:"chat"`
	Thread  int64     `json:"thread,omitempty"`
	Status  string    `json:"status"`            // pending | editing | sent | cancelled
	Prompts []int64   `json:"prompts,omitempty"` // "пришли исправленный текст" messages
	At      time.Time `json:"at"`
}

func (c *tgCard) open() bool { return c.Status == "pending" || c.Status == "editing" }

// cardFor: the card a message id belongs to (the card itself or one of its
// prompts); b.mu held.
func (b *tgBot) cardForLocked(chat, id int64) (int64, *tgCard) {
	for cid, c := range b.st.Cards {
		if c.Chat != chat {
			continue
		}
		if cid == id {
			return cid, c
		}
		for _, p := range c.Prompts {
			if p == id {
				return cid, c
			}
		}
	}
	return 0, nil
}

func (b *tgBot) statusCard(ctx context.Context, m *tgMessage, text string) int64 {
	p := b.params(m, text, nil)
	msg, err := b.send(ctx, p)
	if err != nil {
		return 0
	}
	return msg.MessageID
}

func (b *tgBot) editCard(ctx context.Context, chat, id int64, text string, kb any) {
	if id == 0 {
		return
	}
	p := map[string]any{"chat_id": chat, "message_id": id, "text": text, "parse_mode": "HTML"}
	if kb != nil {
		p["reply_markup"] = kb
	}
	if err := b.out.call(ctx, chat, "editMessageText", p, nil); err != nil && !notModified(err) {
		log.Printf("%v", err)
	}
}

func confirmKeyboard(card int64) tgInlineKeyboard {
	id := strconv.FormatInt(card, 10)
	return tgInlineKeyboard{[][]tgButton{{
		{Text: "✅ Отправить", Data: "v|s|" + id}, {Text: "✏️ Исправить", Data: "v|e|" + id}, {Text: "✖ Отмена", Data: "v|c|" + id},
	}}}
}

func quoteShort(s string) string {
	return "«" + html.EscapeString(truncRunes(strings.TrimSpace(s), transcriptShow)) + "»"
}

// handleSpeech: voice or audio → transcript → card (or straight in with voiceAutoSend).
func (b *tgBot) handleSpeech(ctx context.Context, m *tgMessage, s *sessionRef, pane string, f tgFile, kind, ext string) {
	card := b.statusCard(ctx, m, "🎙 расшифровываю…")
	fail := func(msg string) { b.editCard(ctx, m.Chat.ID, card, "🎙 "+html.EscapeString(msg), nil) }
	dir, err := b.inboxDir(s)
	if err != nil {
		fail("не удалось сохранить файл")
		return
	}
	path, err := b.download(ctx, f, dir, stamp(b.now())+"-"+kind+ext)
	if errors.Is(err, errTooBig) {
		fail("файл больше 20 МБ, Telegram не даёт боту его скачать")
		return
	}
	if err != nil {
		log.Printf("telegram: %v", err)
		fail("не удалось скачать")
		return
	}
	text, err := b.transcribe(ctx, path, f.Duration)
	if err != nil {
		log.Printf("telegram: transcription: %v", err)
		fail("не удалось расшифровать")
		return
	}
	if strings.TrimSpace(text) == "" {
		fail("речи не слышно")
		return
	}
	voiceAuto, _, _, _ := b.mediaSettings()
	if voiceAuto {
		if err := b.submitTo(ctx, s, pane, text); err != nil {
			fail("не отправлено: " + errText(err))
			return
		}
		b.editCard(ctx, m.Chat.ID, card, "🎙 "+quoteShort(text)+"\n→ отправлено", nil)
		return
	}
	b.offer(ctx, m, card, s, pane, text, "🎙 "+quoteShort(text))
}

// offer turns a card into ✅ / ✏️ / ✖ for text waiting to be sent.
func (b *tgBot) offer(ctx context.Context, m *tgMessage, card int64, s *sessionRef, pane, text, shown string) {
	if card == 0 {
		return
	}
	b.mu.Lock()
	if b.st.Cards == nil {
		b.st.Cards = map[int64]*tgCard{}
	}
	b.st.Cards[card] = &tgCard{Session: s.ID, Pane: pane, Text: text, Shown: shown, Chat: m.Chat.ID, Thread: m.MessageThreadID, Status: "pending", At: b.now()}
	b.saveLocked()
	b.mu.Unlock()
	b.editCard(ctx, m.Chat.ID, card, shown, confirmKeyboard(card))
}

func (b *tgBot) setCardStatus(card int64, status string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.st.Cards[card]; c != nil {
		c.Status = status
		b.saveLocked()
	}
}

// sendCard types text into the card's session.
func (b *tgBot) sendCard(ctx context.Context, c *tgCard, text string) error {
	s, err := b.findSession(ctx, c.Session)
	if err != nil {
		return err
	}
	if s == nil {
		return &CtlError{Msg: "сессия закрыта"}
	}
	return b.submitTo(ctx, s, c.Pane, text)
}

// voiceCallback handles ✅ / ✏️ / ✖ on a card.
func (b *tgBot) voiceCallback(ctx context.Context, c *tgCallback, parts []string) string {
	if len(parts) != 2 {
		return ""
	}
	id, _ := strconv.ParseInt(parts[1], 10, 64)
	b.mu.Lock()
	card := b.st.Cards[id]
	var cp tgCard
	if card != nil {
		cp = *card
	}
	b.mu.Unlock()
	if card == nil || !cp.open() || c.Message.MessageID != id || cp.Chat != c.Message.Chat.ID {
		return "Устарело"
	}
	switch parts[0] {
	case "s":
		b.setCardStatus(id, "sent")
		if err := b.sendCard(ctx, &cp, cp.Text); err != nil {
			b.setCardStatus(id, cp.Status)
			return "Не отправлено: " + errText(err)
		}
		b.editCard(ctx, cp.Chat, id, cp.Shown+"\n✅ отправлено", nil)
		return "Отправлено"
	case "e":
		params := map[string]any{"chat_id": cp.Chat, "text": "Пришли исправленный текст ответом на это сообщение.",
			"reply_parameters": map[string]any{"message_id": id, "allow_sending_without_reply": true}}
		if cp.Thread != 0 {
			params["message_thread_id"] = cp.Thread
		}
		msg, err := b.send(ctx, params)
		b.mu.Lock()
		if card := b.st.Cards[id]; card != nil && card.open() {
			card.Status = "editing"
			if err == nil {
				card.Prompts = append(card.Prompts, msg.MessageID)
			}
			b.saveLocked()
		}
		b.mu.Unlock()
		return ""
	case "c":
		b.setCardStatus(id, "cancelled")
		b.editCard(ctx, cp.Chat, id, cp.Shown+"\n✖ отменено", nil)
		return "Отменено"
	}
	return ""
}

// voiceCorrection: a text reply to a card (or to its "пришли исправленный
// текст") replaces the transcript and is sent to the card's session. A reply
// to a card that is already sent or cancelled is answered and goes nowhere —
// never to whatever session would take plain text.
func (b *tgBot) voiceCorrection(ctx context.Context, m *tgMessage) bool {
	if m.ReplyTo == nil {
		return false
	}
	b.mu.Lock()
	id, card := b.cardForLocked(m.Chat.ID, m.ReplyTo.MessageID)
	var cp tgCard
	if card != nil {
		cp = *card
		if card.open() && m.Text != "" {
			card.Status = "sent"
			b.saveLocked()
		}
	}
	b.mu.Unlock()
	switch {
	case card == nil:
		return false
	case cp.Status == "sent":
		b.reply(ctx, m, "Эта расшифровка уже отправлена.")
		return true
	case !cp.open():
		b.reply(ctx, m, "Эта расшифровка уже отменена.")
		return true
	case m.Text == "":
		b.reply(ctx, m, "Пришли исправление текстом.")
		return true
	}
	if err := b.sendCard(ctx, &cp, m.Text); err != nil {
		b.setCardStatus(id, cp.Status)
		b.reply(ctx, m, "→ не отправлено: "+errText(err))
		return true
	}
	b.editCard(ctx, cp.Chat, id, "✏️ "+quoteShort(m.Text)+"\n✅ отправлено", nil)
	b.mu.Lock()
	forum := b.st.Mode == "forum"
	b.mu.Unlock()
	if forum {
		b.out.call(ctx, m.Chat.ID, "setMessageReaction", map[string]any{
			"chat_id": m.Chat.ID, "message_id": m.MessageID,
			"reaction": []map[string]string{{"type": "emoji", "emoji": "👌"}},
		}, nil)
	}
	return true
}

// pruneCards forgets cards (and tombstones) older than the inbox keeps files:
// an old card answers "устарело".
func (b *tgBot) pruneCards(now time.Time, keep time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := false
	for id, c := range b.st.Cards {
		if now.Sub(c.At) > keep {
			delete(b.st.Cards, id)
			changed = true
		}
	}
	if changed {
		b.saveLocked()
	}
}

// transcribe: one at a time; ffmpeg to 16 kHz mono wav, whisper-cli to text,
// with a timeout of twice the duration plus 30 s.
func (b *tgBot) transcribe(ctx context.Context, path string, duration int) (string, error) {
	b.transcribeMu.Lock()
	defer b.transcribeMu.Unlock()
	_, _, _, lang := b.mediaSettings()
	// Outputs go into a fresh private folder, never at a predictable name next
	// to the download (where a planted symlink could redirect the write).
	work, err := workDir(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	wav := filepath.Join(work, "audio.wav")
	if err := runTool(ctx, ffmpegTimeout, b.tools.FFmpeg, "-nostdin", "-v", "error", "-y", "-i", path, "-vn", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", wav); err != nil {
		return "", fmt.Errorf("ffmpeg: %w", err)
	}
	base := filepath.Join(work, "audio")
	limit := time.Duration(2*max(duration, 0))*time.Second + b.speechSlack
	args := []string{"-m", b.tools.WhisperModel, "-f", wav, "-l", lang, "-nt", "-np", "-otxt", "-of", base, "-t", "4"}
	if lang == "auto" {
		args = append(args, "--prompt", "Расшифровка голосового сообщения на русском языке.")
	}
	if err := runTool(ctx, limit, b.tools.Whisper, args...); err != nil {
		return "", fmt.Errorf("whisper: %w", err)
	}
	raw, err := os.ReadFile(base + ".txt")
	if err != nil {
		return "", err
	}
	return strings.Join(strings.Fields(string(raw)), " "), nil
}

// runTool runs a program (absolute path, no shell) with a timeout; its
// output is not logged (it may quote the contents).
func runTool(ctx context.Context, limit time.Duration, prog string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out after %v", limit)
	}
	if err != nil {
		return fmt.Errorf("%v", err) // not stderr: it can contain the transcript
	}
	return nil
}

/* ---------- video ---------- */

// handleVideo: video or video note → transcript of its sound + keyframes,
// sent as one message (or offered on a card with videoAutoSend off).
func (b *tgBot) handleVideo(ctx context.Context, m *tgMessage, s *sessionRef, pane string, f tgFile) {
	card := b.statusCard(ctx, m, "🎥 обрабатываю…")
	fail := func(msg string) { b.editCard(ctx, m.Chat.ID, card, "🎥 "+html.EscapeString(msg), nil) }
	dir, err := b.inboxDir(s)
	if err != nil {
		fail("не удалось сохранить файл")
		return
	}
	ext := strings.ToLower(filepath.Ext(f.FileName))
	if ext == "" {
		ext = ".mp4"
	}
	path, err := b.download(ctx, f, dir, stamp(b.now())+"-video"+ext)
	if errors.Is(err, errTooBig) {
		fail("файл больше 20 МБ, Telegram не даёт боту его скачать")
		return
	}
	if err != nil {
		log.Printf("telegram: %v", err)
		fail("не удалось скачать")
		return
	}
	speech, speechErr := b.transcribe(ctx, path, f.Duration)
	if speechErr != nil {
		log.Printf("telegram: video transcription: %v", speechErr) // a video without sound is fine
		speech = ""
	}
	frames := b.keyframes(ctx, path, f.Duration)
	// Nothing could be taken out of it: the video itself is all there is.
	extractFailed := speechErr != nil && len(frames) == 0
	dur := durText2(f.Duration)
	var lines []string
	lines = append(lines, "[Видео "+dur+" из Telegram: "+path+"]") // the original, always
	if speech != "" {
		lines = append(lines, "Речь: «"+speech+"»")
	}
	if len(frames) > 0 {
		lines = append(lines, "Кадры (посмотри их):")
		lines = append(lines, frames...)
	}
	if c := strings.TrimSpace(m.Caption); c != "" {
		lines = append(lines, "Подпись: "+c)
	}
	text := strings.Join(lines, "\n")
	shown := fmt.Sprintf("🎥 %s · кадров: %d", dur, len(frames))
	if speech != "" {
		shown += "\n" + quoteShort(speech)
	}
	_, videoAuto, _, _ := b.mediaSettings()
	if !videoAuto {
		b.offer(ctx, m, card, s, pane, text, shown)
		return
	}
	if err := b.submitTo(ctx, s, pane, text); err != nil {
		fail("не отправлено: " + errText(err))
		return
	}
	done := fmt.Sprintf("🎥 %s · кадров: %d · отправлено", dur, len(frames))
	if extractFailed {
		done = "🎥 " + dur + " · ⚠️ не удалось извлечь речь/кадры, отправил путь к видео"
	}
	if speech != "" {
		done += "\n" + quoteShort(speech)
	}
	b.editCard(ctx, m.Chat.ID, card, done, nil)
}

// keyframes: scene changes (at most maxFrames), or, when there are none,
// evenly spaced frames; JPEG, longest side at most frameMaxSide.
func (b *tgBot) keyframes(ctx context.Context, video string, duration int) []string {
	// A new folder with a random suffix (0700): nothing in it can be planted.
	dir, err := os.MkdirTemp(filepath.Dir(video), filepath.Base(video)+"-frames-")
	if err != nil {
		return nil
	}
	scale := fmt.Sprintf("scale='if(gt(iw,ih),min(%d,iw),-2)':'if(gt(iw,ih),-2,min(%d,ih))'", frameMaxSide, frameMaxSide)
	list := func() []string {
		ents, _ := os.ReadDir(dir)
		var out []string
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".jpg") {
				p := filepath.Join(dir, e.Name())
				os.Chmod(p, 0o600)
				out = append(out, p)
			}
		}
		sort.Strings(out)
		return out
	}
	runTool(ctx, ffmpegTimeout, b.tools.FFmpeg, "-nostdin", "-v", "error", "-y", "-i", video,
		"-vf", "select='gt(scene,0.3)',"+scale, "-vsync", "vfr", "-frames:v", strconv.Itoa(maxFrames), "-q:v", "3", filepath.Join(dir, "scene-%02d.jpg"))
	if got := list(); len(got) > 0 {
		return got
	}
	fps := float64(fallbackFrames) / float64(max(duration, 1))
	runTool(ctx, ffmpegTimeout, b.tools.FFmpeg, "-nostdin", "-v", "error", "-y", "-i", video,
		"-vf", "fps="+strconv.FormatFloat(fps, 'f', 4, 64)+","+scale, "-frames:v", strconv.Itoa(fallbackFrames), "-q:v", "3", filepath.Join(dir, "even-%02d.jpg"))
	return list()
}

/* ---------- cleanup ---------- */

// cleanInbox deletes inbox files older than the keep period (and the folders
// that become empty).
func (b *tgBot) cleanInbox(now time.Time) {
	_, _, keep, _ := b.mediaSettings()
	b.pruneCards(now, time.Duration(keep)*24*time.Hour)
	cutoff := now.Add(-time.Duration(keep) * 24 * time.Hour)
	var dirs []string
	filepath.WalkDir(b.inbox, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != b.inbox {
				dirs = append(dirs, p)
			}
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(p)
		}
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) }) // deepest first
	for _, d := range dirs {
		os.Remove(d) // only succeeds when empty
	}
}

func (b *tgBot) inboxLoop(ctx context.Context) {
	b.cleanInbox(time.Now())
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.cleanInbox(time.Now())
		}
	}
}
