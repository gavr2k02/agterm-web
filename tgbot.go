package main

// Telegram bot: agterm notifications and replies from a phone.
//
// Forum mode (a group with topics): every agterm session has a topic; text in a
// topic goes into its session, notifications land in it, topic names carry the
// status. Private mode: notifications in one chat, a reply to a notification (or
// plain text, to the session picked in "📋 Сессии") goes into the session.
//
// Only the paired user in the paired chat is listened to; everything else is
// dropped silently. Message texts are never logged.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	btnSessions = "📋 Сессии"
	btnLimits   = "📊 Лимиты"
	btnMute     = "🔕 Тишина 1ч"
	btnUnmute   = "🔔 Включить"
	btnStatus   = "ℹ️ Статус"

	tgMaxScreen     = 3500
	tgMaxHeader     = 200 // characters of a session name in a message header
	tgRetries       = 8   // 5 s doubling to 2 min: about ten minutes
	tgSyncRetry     = 30 * time.Second
	tgSaveRetry     = 30 * time.Second // state file not writable: wait before fetching again
	tgMaxTopicName  = 128
	tgMaxButtons    = 90
	ctrlcConfirmGap = 5 * time.Second
	muteFor         = time.Hour
	syncEvery       = time.Minute
	syncDebounce    = time.Second
)

// tgKeys: callback key name -> button label and the bytes it types.
var tgKeys = map[string][2]string{
	"1": {"1", "1"}, "2": {"2", "2"}, "3": {"3", "3"},
	"enter": {"Enter", "\r"}, "esc": {"Esc", "\x1b"},
	"up": {"↑", "\x1b[A"}, "down": {"↓", "\x1b[B"}, "left": {"←", "\x1b[D"}, "right": {"→", "\x1b[C"},
	"tab": {"Tab", "\t"}, "stab": {"⇧Tab", "\x1b[Z"}, "bs": {"⌫", "\x7f"},
	"y": {"y", "y"}, "n": {"n", "n"}, "ctrlc": {"Ctrl+C", "\x03"},
}

var statusEmoji = map[string]string{"blocked": "🟠", "active": "🔵", "completed": "✅", "idle": "⚪"}

// Claude Code prefixes its title with a spinner glyph (same set as web/app.js).
var glyphPrefix = regexp.MustCompile(`^[\x{2800}-\x{28FF}\x{2722}-\x{273D}\x{25D0}-\x{25D3}·*•\s]+`)

type tgBot struct {
	api       *tgClient
	out       *tgOutbox // every chat-scoped call goes through it
	srv       *server
	hub       *hub
	statePath string
	pairPath  string

	now            func() time.Time
	after          func(d time.Duration, f func())
	notifyDelay    time.Duration // let the screen settle before reading it
	notifyDebounce time.Duration // per session and kind
	renameGap      time.Duration // between renames of one topic

	ctx context.Context

	mu            sync.Mutex
	st            *tgState
	lastNotify    map[string]time.Time
	lastRename    map[string]time.Time
	ctrlcArmed    map[string]time.Time
	syncScheduled bool
	gen           uint64              // bumped on every (re)pairing; older async results are dropped
	genCtxs       []genCtx            // contexts of work for the current pairing
	lastPane      map[string]string   // session -> pane its agent last reported a status from
	renamed       map[int64]time.Time // thread -> when the bot last renamed it
	nameCand      map[string]nameCand // session -> real name waiting to settle
	iconWarned    map[string]bool     // icons already reported as not allowed

	// Mirroring (mirror.go).
	mirrorMu     sync.Mutex
	mirrors      map[string]*transcriptMirror // followed transcripts by path
	mirrorQ      []*mirrorOp
	mirrorIDs    map[string]int64 // op key -> sent message id
	mirrorGen    uint64
	mirrorMapped time.Time
	mirrored     map[string]bool // sessions whose conversation is mirrored
	unmirrored   []string        // Claude sessions that cannot be told apart
	lastSrc      map[string]string
	sentTexts    map[string][]sentText
	claudeDir    string // hook mappings
	projectsDir  string // ~/.claude/projects
	settingsPath string

	// Media (media.go).
	tools         mediaTools
	inbox         string // ~/agterm-inbox
	albums        map[string]*album
	transcribeMu  sync.Mutex    // one transcription at a time
	speechSlack   time.Duration // whisper timeout: 2 × duration + this
	botID         int64         // the bot's own user id (the token's prefix)
	draft         *launchDraft
	draftVer      int       // bumped whenever draft is replaced
	launcherCheck time.Time // last time the launcher message was found alive
	started       time.Time // when the service started (for /status)
	healthPath    string

	home         string // folders the launcher may use live under it
	launcherFile string // {"favorites": [...]}
	pollSleep    func(ctx context.Context, d time.Duration) error
	launchMu     sync.Mutex

	topicMu sync.Mutex // one topic sync at a time, so no topic is created twice
}

// loadTGToken reads the bot token. ok is false when the file does not exist
// (the bot is off); a file others can read is refused.
func loadTGToken(path string) (token string, ok bool, err error) {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", false, fmt.Errorf("%s must be readable by you only (chmod 600 it)", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	t := strings.TrimSpace(string(b))
	if t == "" || strings.ContainsAny(t, " \t\r\n/?#") {
		return "", false, fmt.Errorf("%s: not a bot token", path)
	}
	return t, true, nil
}

// botIDOf: a bot token is "<bot id>:<secret>".
func botIDOf(token string) int64 {
	id, _ := strconv.ParseInt(strings.SplitN(token, ":", 2)[0], 10, 64)
	return id
}

func tgStatePath(tokenFile string) string {
	return filepath.Join(filepath.Dir(tokenFile), "telegram.json")
}
func tgPairPath(tokenFile string) string {
	return filepath.Join(filepath.Dir(tokenFile), "telegram-pair.json")
}

func newTGBot(api *tgClient, srv *server, tokenFile string) (*tgBot, error) {
	statePath := tgStatePath(tokenFile)
	st, err := loadTGState(statePath)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real // folders are compared and passed in canonical form
	}
	b := &tgBot{
		api: api, out: newTGOutbox(api), srv: srv, hub: srv.hub,
		statePath: statePath, pairPath: tgPairPath(tokenFile),
		now:            time.Now,
		after:          nil, // set below: needs b for recovery
		notifyDelay:    time.Second,
		notifyDebounce: 10 * time.Second,
		renameGap:      3 * time.Second,
		ctx:            context.Background(),
		st:             st,
		lastNotify:     map[string]time.Time{},
		lastRename:     map[string]time.Time{},
		ctrlcArmed:     map[string]time.Time{},
		lastPane:       map[string]string{},
		renamed:        map[int64]time.Time{},
		nameCand:       map[string]nameCand{},
		iconWarned:     map[string]bool{},
		mirrors:        map[string]*transcriptMirror{},
		mirrorIDs:      map[string]int64{},
		lastSrc:        map[string]string{},
		sentTexts:      map[string][]sentText{},
		claudeDir:      filepath.Join(filepath.Dir(tokenFile), "claude-sessions"),
		projectsDir:    filepath.Join(home, ".claude", "projects"),
		settingsPath:   filepath.Join(filepath.Dir(tokenFile), "telegram-settings.json"),
		inbox:          filepath.Join(home, "agterm-inbox"),
		speechSlack:    30 * time.Second,
		albums:         map[string]*album{},
		tools: mediaTools{
			FFmpeg:       defaultTool("ffmpeg", "/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"),
			Whisper:      defaultTool("whisper-cli", "/opt/homebrew/bin/whisper-cli", "/usr/local/bin/whisper-cli"),
			WhisperModel: filepath.Join(home, ".local", "share", "whisper", "ggml-large-v3-turbo.bin"),
		},
		botID:        botIDOf(api.token),
		home:         home,
		launcherFile: filepath.Join(filepath.Dir(tokenFile), "launcher.json"),
		pollSleep:    sleepCtx,
		started:      time.Now(),
		healthPath:   tgHealthPath(tokenFile),
	}
	b.after = func(d time.Duration, f func()) { time.AfterFunc(d, func() { b.safely("timer", f) }) }
	b.loadMirror()
	return b, nil
}

func (b *tgBot) run(ctx context.Context) {
	b.ctx = ctx
	go b.forever(ctx, "events", b.eventLoop)
	go b.forever(ctx, "health", b.healthLoop)
	go b.forever(ctx, "mirror", b.mirrorLoop)
	go b.forever(ctx, "inbox", b.inboxLoop)
	b.scheduleSync()
	b.forever(ctx, "poll", b.pollLoop)
}

// forever runs a long-lived loop and, if it panics, logs it and starts it
// again: one bad message must not take the whole service down.
func (b *tgBot) forever(ctx context.Context, name string, loop func(context.Context)) {
	for ctx.Err() == nil {
		b.safely(name, func() { loop(ctx) })
		if ctx.Err() == nil {
			sleepCtx(ctx, 5*time.Second)
		}
	}
}

// safely runs f, turning a panic into a log line.
func (b *tgBot) safely(name string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("telegram: %s: panic: %v (recovered)", name, r)
		}
	}()
	f()
}

// saveLocked writes the state; b.mu must be held.
func (b *tgBot) saveLocked() error {
	err := b.st.save(b.statePath)
	if err != nil {
		log.Printf("telegram: save state: %v", err)
	}
	return err
}

/* ---------- polling ---------- */

func (b *tgBot) pollLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		b.mu.Lock()
		off := b.st.Offset
		b.mu.Unlock()
		var ups []tgUpdate
		err := b.api.call(ctx, "getUpdates", map[string]any{
			"offset": off, "timeout": 50, "allowed_updates": []string{"message", "callback_query"},
		}, &ups)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait := backoff
			var te *tgError
			if errors.As(err, &te) && te.RetryAfter > 0 {
				wait = te.RetryAfter
			} else {
				backoff = min(backoff*2, time.Minute)
			}
			log.Printf("%v (retry in %v)", err, wait)
			if b.api.sleep(ctx, wait) != nil {
				return
			}
			continue
		}
		backoff = time.Second
		for _, u := range ups {
			// At most once: the offset past this update is on disk before the
			// update can type anything, so a crash loses it rather than repeats
			// it. If it cannot be saved, the update is not run (fail closed): the
			// batch stops and is fetched again later.
			b.mu.Lock()
			skip := u.UpdateID < b.st.Offset
			var saveErr error
			if !skip {
				prev := b.st.Offset
				b.st.Offset = u.UpdateID + 1
				if saveErr = b.saveLocked(); saveErr != nil {
					b.st.Offset = prev
				}
			}
			b.mu.Unlock()
			if saveErr != nil {
				b.api.sleep(ctx, tgSaveRetry)
				break
			}
			if !skip {
				b.handleUpdate(ctx, u)
			}
		}
	}
}

func (b *tgBot) handleUpdate(ctx context.Context, u tgUpdate) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("telegram: update %d: panic: %v", u.UpdateID, r) // the service keeps running
		}
	}()
	if u.Message != nil {
		b.handleMessage(ctx, u.Message)
	}
	if u.Callback != nil {
		b.handleCallback(ctx, u.Callback)
	}
}

// pairingContext derives a context that ends when the pairing changes from
// gen (or already has), so work for an old pairing is cut short.
func (b *tgBot) pairingContext(parent context.Context, gen uint64) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gen != gen {
		cancel()
		return ctx, cancel
	}
	live := b.genCtxs[:0]
	for _, g := range b.genCtxs {
		if g.ctx.Err() == nil {
			live = append(live, g)
		}
	}
	b.genCtxs = append(live, genCtx{ctx, cancel})
	return ctx, cancel
}

type genCtx struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (b *tgBot) genIs(gen uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gen == gen
}

func (b *tgBot) authorized(user, chat int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st.paired() && user == b.st.UserID && chat == b.st.ChatID
}

// command splits "/cmd@bot arg" into "/cmd" and "arg"; "" for plain text.
func command(text string) (string, string) {
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	cmd, arg, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@")
	return strings.ToLower(cmd), strings.TrimSpace(arg)
}

func (b *tgBot) handleMessage(ctx context.Context, m *tgMessage) {
	if m.From == nil || b.dropRenameNotice(ctx, m) {
		return
	}
	cmd, arg := command(m.Text)
	if !m.fromHuman() {
		// Anonymous admins share one placeholder identity; bots and channels are
		// not people. None of them can pair or be listened to.
		if cmd == "/pair" && pairPending(b.pairPath, b.now()) {
			b.reply(ctx, m, "Отправь /pair от своего аккаунта, не анонимно.")
		}
		return
	}
	if cmd == "/pair" {
		b.handlePair(ctx, m, arg)
		return
	}
	if !b.authorized(m.From.ID, m.Chat.ID) {
		return
	}
	if m.Text == "" {
		if m.hasMedia() {
			b.handleMedia(ctx, m)
		}
		return // service messages (topic created, member joined, ...)
	}
	switch {
	case m.Text == btnSessions || cmd == "/sessions":
		b.sendSessions(ctx, m)
	case m.Text == btnLimits || cmd == "/limits":
		b.sendLimits(ctx, m)
	case m.Text == btnMute || cmd == "/mute":
		b.mu.Lock()
		b.st.MutedUntil = b.now().Add(muteFor)
		until := b.st.MutedUntil
		b.saveLocked()
		b.mu.Unlock()
		b.reply(ctx, m, "🔕 Без уведомлений до "+until.Local().Format("15:04"))
	case m.Text == btnUnmute || cmd == "/unmute":
		b.mu.Lock()
		b.st.MutedUntil = time.Time{}
		b.saveLocked()
		b.mu.Unlock()
		b.reply(ctx, m, "🔔 Уведомления включены")
	case m.Text == btnStatus || cmd == "/status":
		b.reply(ctx, m, b.statusText(ctx))
	case cmd == "/new":
		b.handleNew(ctx, m, arg)
	case cmd == "/start" || cmd == "/help":
		b.sendHelp(ctx, m)
	default:
		b.routeText(ctx, m)
	}
}

func (b *tgBot) handlePair(ctx context.Context, m *tgMessage, code string) {
	err := checkPairCode(b.pairPath, code, b.now())
	switch {
	case errors.Is(err, errNoPairCode):
		return // nothing pending: stay silent
	case errors.Is(err, errPairExpired):
		b.reply(ctx, m, "Код истёк. Новый: agterm-web -telegram-pair")
		return
	case errors.Is(err, errPairBurned):
		log.Printf("telegram: pairing code burned after %d wrong attempts", pairAttempts)
		b.reply(ctx, m, "Слишком много неверных попыток, код сгорел.")
		return
	case errors.Is(err, errPairWrong):
		b.reply(ctx, m, "Неверный код.")
		return
	case err != nil:
		log.Printf("telegram: pairing refused: %v", err)
		b.reply(ctx, m, "Не удалось использовать код, бот не связан. Подробности в логе.")
		return
	}
	mode := "private"
	if m.Chat.IsForum {
		mode = "forum"
	}
	b.mu.Lock()
	if b.st.ChatID != m.Chat.ID {
		b.st.Topics, b.st.Archived, b.st.MsgRefs, b.st.Current = nil, nil, nil, ""
		b.st.LauncherThread, b.st.LauncherMsg, b.st.LauncherPinned = 0, 0, false
	}
	b.st.ChatID, b.st.UserID, b.st.Mode = m.Chat.ID, m.From.ID, mode
	b.st.ChatTitle, b.st.PairedAt = m.Chat.Title, b.now()
	b.gen++                                         // whatever is in flight for the old pairing is dropped when it lands
	b.st.Cards, b.albums = nil, map[string]*album{} // cards and albums of the old pairing
	for _, g := range b.genCtxs {
		g.cancel() // and launches of the old pairing stop
	}
	b.genCtxs = nil
	b.saveLocked()
	b.mu.Unlock()
	log.Printf("telegram: paired (%s mode)", mode)
	text := "Готово, бот связан с этим чатом."
	if mode == "forum" {
		text += " У каждой сессии будет своя тема: пиши в тему — текст уйдёт в сессию."
	} else {
		text += " Ответь на уведомление — текст уйдёт в его сессию. «📋 Сессии» — выбрать, куда писать без ответа."
	}
	b.send(ctx, b.params(m, text, replyKeyboard()))
	if mode == "forum" {
		b.scheduleSync()
	}
}

func replyKeyboard() map[string]any {
	return map[string]any{
		"keyboard": [][]map[string]string{
			{{"text": btnSessions}, {"text": btnLimits}, {"text": btnStatus}},
			{{"text": btnMute}, {"text": btnUnmute}},
		},
		"resize_keyboard": true,
		"is_persistent":   true,
	}
}

func (b *tgBot) sendHelp(ctx context.Context, m *tgMessage) {
	b.send(ctx, b.params(m, "agterm: уведомления о сессиях, которые ждут тебя, и ответы в них.\n"+
		btnSessions+" — список сессий, "+btnLimits+" — лимиты Claude и Codex, "+btnMute+" — без уведомлений на час.\n"+
		"/new <папка> [claude|claude-continue|codex|shell] [воркспейс] — новая сессия, "+btnStatus+" (/status) — состояние Mac, agterm и бота.",
		replyKeyboard()))
}

// params builds sendMessage parameters answering m, in its topic.
func (b *tgBot) params(m *tgMessage, text string, markup any) map[string]any {
	p := map[string]any{"chat_id": m.Chat.ID, "text": text}
	if m.IsTopicMessage && m.MessageThreadID != 0 {
		p["message_thread_id"] = m.MessageThreadID
	}
	if markup != nil {
		p["reply_markup"] = markup
	}
	return p
}

func (b *tgBot) send(ctx context.Context, p map[string]any) (*tgMessage, error) {
	var out tgMessage
	if err := b.out.call(ctx, chatOf(p), "sendMessage", p, &out); err != nil {
		log.Printf("%v", err)
		return nil, err
	}
	return &out, nil
}

func chatOf(p map[string]any) int64 {
	id, _ := p["chat_id"].(int64)
	return id
}

func (b *tgBot) reply(ctx context.Context, m *tgMessage, text string) {
	b.send(ctx, b.params(m, text, nil))
}

/* ---------- sessions ---------- */

type sessionRef struct {
	Session
	Workspace string
}

func (b *tgBot) sessions(ctx context.Context) ([]sessionRef, error) {
	tree, err := b.srv.ctl.Tree(ctx)
	if err != nil {
		return nil, err
	}
	var out []sessionRef
	for _, ws := range tree {
		for _, s := range ws.Sessions {
			out = append(out, sessionRef{s, ws.Name})
		}
	}
	return out, nil
}

func (b *tgBot) findSession(ctx context.Context, id string) (*sessionRef, error) {
	all, err := b.sessions(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if strings.EqualFold(all[i].ID, id) {
			return &all[i], nil
		}
	}
	return nil, nil
}

func sessionName(s Session) string {
	n := strings.TrimSpace(glyphPrefix.ReplaceAllString(s.Name, ""))
	if n == "" {
		n = s.Program
	}
	if n == "" {
		n = "сессия"
	}
	return n
}

// topicTitle: "🟠 DXP · Тесты Price IT", at most tgMaxTopicName characters;
// the session name is what gets shortened.
func topicTitle(prefix, workspace, name, suffix string) string {
	head := ""
	if prefix != "" {
		head = prefix + " "
	}
	if workspace != "" {
		head += workspace + " · "
	}
	avail := tgMaxTopicName - utf8.RuneCountInString(head) - utf8.RuneCountInString(suffix)
	if r := []rune(name); len(r) > avail {
		if avail < 1 {
			avail = 1
		}
		name = string(r[:avail-1]) + "…"
	}
	t := head + name + suffix
	if r := []rune(t); len(r) > tgMaxTopicName { // a very long workspace name
		t = string(r[:tgMaxTopicName-1]) + "…"
	}
	return t
}

// topicName is a session topic's stable title, "DXP · Тесты Price IT": set
// when the topic is created and changed only when the session moves to another
// workspace (and on archiving). No status in it — every rename posts a
// "topic edited" line into the topic — and no following agterm's name, which
// Claude Code rewrites all the time. Status lives in "📋 Сессии" and /status.
func topicName(s sessionRef) string {
	return topicTitle("", s.Workspace, sessionName(s.Session), "")
}

func refTitle(r *tgTopicRef) string { return topicTitle("", r.Workspace, r.Name, "") }

func archiveName(t *tgTopicRef) string {
	return topicTitle("📦", t.Workspace, t.Name, " (архив)")
}

// topicURL links to a forum topic: chat -1001234567890 -> t.me/c/1234567890/<thread>.
func topicURL(chat, thread int64) string {
	id := strings.TrimPrefix(strconv.FormatInt(chat, 10), "-100")
	return fmt.Sprintf("https://t.me/c/%s/%d", strings.TrimPrefix(id, "-"), thread)
}

func (b *tgBot) sendSessions(ctx context.Context, m *tgMessage) {
	all, err := b.sessions(ctx)
	if err != nil {
		b.reply(ctx, m, "Не удалось получить сессии: "+errText(err))
		return
	}
	if len(all) == 0 {
		b.reply(ctx, m, "Сессий нет.")
		return
	}
	b.mu.Lock()
	forum := b.st.Mode == "forum"
	topics := map[string]int64{}
	for k, v := range b.st.Topics {
		topics[k] = v.Thread
	}
	chat, current := b.st.ChatID, b.st.Current
	b.mu.Unlock()

	var lines []string
	var rows [][]tgButton
	lastWS := "\x00"
	for _, s := range all {
		if s.Workspace != lastWS {
			lines = append(lines, "", "<b>"+html.EscapeString(s.Workspace)+"</b>")
			lastWS = s.Workspace
		}
		line := statusEmoji[s.Status] + " " + html.EscapeString(sessionName(s.Session))
		if l := statusLabel(s.Status); l != "" {
			line += " — " + l
		}
		lines = append(lines, line)
		if len(rows) >= tgMaxButtons {
			continue
		}
		label := statusEmoji[s.Status] + " " + sessionName(s.Session)
		if forum {
			if t, ok := topics[s.ID]; ok {
				rows = append(rows, []tgButton{{Text: label, URL: topicURL(chat, t)}})
			}
		} else {
			if strings.EqualFold(s.ID, current) {
				label = "✔︎ " + label
			}
			rows = append(rows, []tgButton{{Text: label, Data: "c|" + s.ID}})
		}
	}
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if len(text) > 3800 { // Telegram's limit is 4096 characters
		text = text[:strings.LastIndexByte(text[:3800], '\n')] + "\n…"
	}
	if !forum {
		text += "\n\nКуда писать без ответа на уведомление — выбери кнопкой."
	}
	p := b.params(m, text, nil)
	p["parse_mode"] = "HTML"
	if len(rows) > 0 {
		p["reply_markup"] = tgInlineKeyboard{rows}
	}
	b.send(ctx, p)
}

// statusLabel mirrors the web panel's wording.
func statusLabel(status string) string {
	switch status {
	case "active":
		return "работает"
	case "blocked":
		return "ждёт тебя"
	case "completed":
		return "готово"
	}
	return ""
}

func (b *tgBot) sendLimits(ctx context.Context, m *tgMessage) {
	if b.srv.limits == nil {
		b.reply(ctx, m, "Лимиты недоступны.")
		return
	}
	b.reply(ctx, m, LimitsText(b.srv.limits.Get(ctx), time.Now()))
}

func errText(err error) string {
	var ce *CtlError
	if errors.As(err, &ce) {
		return ce.Msg
	}
	return "agterm недоступен"
}

/* ---------- text into sessions ---------- */

func (b *tgBot) routeText(ctx context.Context, m *tgMessage) {
	if len(m.Text) > maxTypeLen {
		b.reply(ctx, m, "Слишком длинно: больше 16 КБ не отправляю.")
		return
	}
	if strings.ContainsRune(m.Text, 0) {
		b.reply(ctx, m, "В тексте нулевой символ, не отправляю.")
		return
	}
	if b.voiceCorrection(ctx, m) {
		return
	}
	s, pane, ok := b.resolveTarget(ctx, m)
	if !ok {
		return
	}
	if err := b.submitTo(ctx, s, pane, m.Text); err != nil {
		b.reply(ctx, m, "→ не отправлено: "+errText(err))
		return
	}
	b.ack(ctx, m, s)
}

// resolveTarget finds the session (and pane) a message is meant for: its
// topic in a forum, the replied-to notification or the current session in a
// private chat. When there is none it answers the user itself and returns
// ok=false. Text in the launcher topic is handled there.
func (b *tgBot) resolveTarget(ctx context.Context, m *tgMessage) (*sessionRef, string, bool) {
	b.mu.Lock()
	forum := b.st.Mode == "forum"
	var sid, pane string
	if forum {
		if m.IsTopicMessage && m.MessageThreadID != 0 {
			sid = b.st.sessionOfThread(m.MessageThreadID)
		}
	} else {
		if m.ReplyTo != nil {
			if r := b.st.msgRef(m.ReplyTo.MessageID); r != nil {
				sid, pane = r.Session, r.Pane
			}
		}
		if sid == "" {
			sid = b.st.Current
		}
	}
	if pane == "" {
		pane = b.lastPane[strings.ToUpper(sid)] // where the agent runs, if it said
	}
	archived := forum && sid == "" && b.st.archivedThread(m.MessageThreadID)
	launcher := forum && m.IsTopicMessage && b.st.LauncherThread != 0 && m.MessageThreadID == b.st.LauncherThread
	b.mu.Unlock()
	switch {
	case launcher && m.Text != "":
		b.launcherText(ctx, m)
		return nil, "", false
	case launcher:
		b.reply(ctx, m, "Это тема запуска сессий. Пришли файл в тему сессии.")
		return nil, "", false
	case archived:
		b.reply(ctx, m, "Сессия закрыта, тема в архиве.")
		return nil, "", false
	case sid == "" && forum:
		b.reply(ctx, m, "Пиши в тему сессии. Список — "+btnSessions)
		return nil, "", false
	case sid == "":
		b.reply(ctx, m, "Куда отправить? Ответь на уведомление или выбери сессию: "+btnSessions)
		return nil, "", false
	}
	s, err := b.findSession(ctx, sid)
	if err != nil {
		b.reply(ctx, m, "→ не отправлено: "+errText(err))
		return nil, "", false
	}
	if s == nil {
		b.reply(ctx, m, "→ не отправлено: сессия закрыта")
		return nil, "", false
	}
	return s, paneOr(pane), true
}

// submitTo types text into a session as a prompt (multi-line as a bracketed
// paste) and remembers it, so the mirror does not echo it back.
func (b *tgBot) submitTo(ctx context.Context, s *sessionRef, pane, text string) error {
	chunks, err := typeChunks("submit", text)
	if err == nil {
		err = b.srv.typeInto(ctx, s.Window, s.ID, paneOr(pane), chunks)
	}
	if err == nil {
		b.noteSent(s.ID, paneOr(pane), text)
	}
	return err
}

// ack confirms delivery: a reaction in a forum topic (keeps it clean), a
// short reply in a private chat.
func (b *tgBot) ack(ctx context.Context, m *tgMessage, s *sessionRef) {
	b.mu.Lock()
	forum := b.st.Mode == "forum"
	b.mu.Unlock()
	if forum {
		b.out.call(ctx, m.Chat.ID, "setMessageReaction", map[string]any{
			"chat_id": m.Chat.ID, "message_id": m.MessageID,
			"reaction": []map[string]string{{"type": "emoji", "emoji": "👌"}},
		}, nil)
		return
	}
	b.reply(ctx, m, "→ "+sessionName(s.Session))
}

/* ---------- callbacks ---------- */

func (b *tgBot) handleCallback(ctx context.Context, c *tgCallback) {
	if c.Message == nil || !c.From.human() || !b.authorized(c.From.ID, c.Message.Chat.ID) {
		return
	}
	parts := strings.Split(c.Data, "|")
	toast := ""
	if parts[0] == "L" {
		toast = b.launcherCallback(ctx, c, parts[1:])
	} else if parts[0] == "v" {
		toast = b.voiceCallback(ctx, c, parts[1:])
	} else if len(parts) >= 2 && idRe.MatchString(parts[1]) {
		sid := strings.ToUpper(parts[1])
		// The pane is the last field of k/s/m data ("l", "r", "s"); older buttons have none.
		pane := paneOfCode(parts[len(parts)-1])
		switch {
		case parts[0] == "k" && len(parts) >= 3:
			toast = b.pressKey(ctx, sid, parts[2], pane)
		case parts[0] == "s":
			toast = b.refreshScreen(ctx, c, sid, pane)
		case parts[0] == "m" && len(parts) >= 3:
			kb := mainKeyboard(sid, pane)
			if parts[2] == "more" {
				kb = moreKeyboard(sid, pane)
			}
			b.out.call(ctx, c.Message.Chat.ID, "editMessageReplyMarkup", map[string]any{
				"chat_id": c.Message.Chat.ID, "message_id": c.Message.MessageID, "reply_markup": kb,
			}, nil)
		case parts[0] == "c":
			s, err := b.findSession(ctx, sid)
			switch {
			case err != nil:
				toast = errText(err)
			case s == nil:
				toast = "Сессия закрыта"
			default:
				b.mu.Lock()
				b.st.Current = s.ID
				b.saveLocked()
				b.mu.Unlock()
				toast = "Пишу в: " + sessionName(s.Session)
			}
		}
	}
	p := map[string]any{"callback_query_id": c.ID}
	if toast != "" {
		p["text"] = toast
	}
	b.api.call(ctx, "answerCallbackQuery", p, nil)
}

// paneCodes keep callback data short: "k|<uuid>|ctrlc|r" is 45 bytes.
var paneCodes = map[string]string{"left": "l", "right": "r", "scratch": "s"}

func paneCode(pane string) string {
	if c, ok := paneCodes[pane]; ok {
		return c
	}
	return "l"
}

func paneOfCode(c string) string {
	for p, code := range paneCodes {
		if code == c {
			return p
		}
	}
	return "left"
}

func paneOr(p string) string {
	if p == "" || !validPane(p) {
		return "left"
	}
	return p
}

func (b *tgBot) pressKey(ctx context.Context, sid, key, pane string) string {
	k, ok := tgKeys[key]
	if !ok {
		return "Неизвестная клавиша"
	}
	if key == "ctrlc" {
		b.mu.Lock()
		armed, was := b.ctrlcArmed[sid]
		now := b.now()
		if !was || now.Sub(armed) > ctrlcConfirmGap {
			b.ctrlcArmed[sid] = now
			b.mu.Unlock()
			return "Нажми Ctrl+C ещё раз, чтобы отправить"
		}
		delete(b.ctrlcArmed, sid)
		b.mu.Unlock()
	}
	s, err := b.findSession(ctx, sid)
	if err != nil {
		return errText(err)
	}
	if s == nil {
		return "Сессия закрыта"
	}
	if err := b.srv.typeInto(ctx, s.Window, s.ID, paneOr(pane), []string{k[1]}); err != nil {
		return "Не отправлено: " + errText(err)
	}
	return "Нажато: " + k[0]
}

func (b *tgBot) refreshScreen(ctx context.Context, c *tgCallback, sid, pane string) string {
	s, err := b.findSession(ctx, sid)
	if err != nil {
		return errText(err)
	}
	if s == nil {
		return "Сессия закрыта"
	}
	text, err := b.srv.ctl.Text(ctx, s.Window, s.ID, paneOr(pane), 0)
	if err != nil {
		return errText(err)
	}
	header, _, _ := strings.Cut(c.Message.Text, "\n")
	body := html.EscapeString(truncRunes(header, tgMaxHeader)) + "\n" + screenPre(text, 20) + "\n<i>обновлено " + b.now().Local().Format("15:04:05") + "</i>"
	p := map[string]any{"chat_id": c.Message.Chat.ID, "message_id": c.Message.MessageID, "text": body, "parse_mode": "HTML"}
	if len(c.Message.ReplyMarkup) > 0 {
		p["reply_markup"] = c.Message.ReplyMarkup
	}
	if err := b.out.call(ctx, c.Message.Chat.ID, "editMessageText", p, nil); err != nil && !strings.Contains(err.Error(), "not modified") {
		log.Printf("%v", err)
		return "Не удалось обновить"
	}
	return "Обновлено"
}

func keyButton(sid, key, pc string) tgButton {
	return tgButton{Text: tgKeys[key][0], Data: "k|" + sid + "|" + key + "|" + pc}
}

func mainKeyboard(sid, pane string) tgInlineKeyboard {
	pc := paneCode(pane)
	return tgInlineKeyboard{[][]tgButton{
		{keyButton(sid, "1", pc), keyButton(sid, "2", pc), keyButton(sid, "3", pc), keyButton(sid, "enter", pc), keyButton(sid, "esc", pc)},
		{{Text: "🔄 Экран", Data: "s|" + sid + "|" + pc}, {Text: "⌨️ Ещё", Data: "m|" + sid + "|more|" + pc}},
	}}
}

func moreKeyboard(sid, pane string) tgInlineKeyboard {
	pc := paneCode(pane)
	return tgInlineKeyboard{[][]tgButton{
		{keyButton(sid, "up", pc), keyButton(sid, "down", pc), keyButton(sid, "left", pc), keyButton(sid, "right", pc)},
		{keyButton(sid, "tab", pc), keyButton(sid, "stab", pc), keyButton(sid, "bs", pc)},
		{keyButton(sid, "y", pc), keyButton(sid, "n", pc), keyButton(sid, "ctrlc", pc)},
		{{Text: "⬅️", Data: "m|" + sid + "|main|" + pc}},
	}}
}

func doneKeyboard(sid, pane string) tgInlineKeyboard {
	return tgInlineKeyboard{[][]tgButton{{{Text: "🔄 Экран", Data: "s|" + sid + "|" + paneCode(pane)}}}}
}

// truncRunes shortens s to at most n characters, with "…".
func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// screenPre: the last n non-empty lines of a screen as an HTML <pre>, at most tgMaxScreen bytes.
func screenPre(text string, n int) string {
	var lines []string
	for _, l := range strings.Split(stripControl(text), "\n") {
		if l = strings.TrimRight(l, " \t\r"); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 0 {
		return "<i>экран пуст</i>"
	}
	body := html.EscapeString(strings.Join(lines, "\n"))
	for len(body) > tgMaxScreen {
		// Drop whole leading lines so no entity or rune is cut in half.
		i := strings.IndexByte(body, '\n')
		if i < 0 {
			body = "…"
			break
		}
		body = body[i+1:]
	}
	return "<pre>" + body + "</pre>"
}

/* ---------- events and notifications ---------- */

type agtermEvent struct {
	Kind    string `json:"kind"`
	Window  string `json:"window"`
	Session string `json:"session"`
	Payload struct {
		Name     string `json:"name"`
		Status   string `json:"status"`
		Previous string `json:"previous"`
		Pane     string `json:"pane"`
	} `json:"payload"`
}

func (b *tgBot) eventLoop(ctx context.Context) {
	ch, cancel := b.hub.subscribe()
	defer cancel()
	tick := time.NewTicker(syncEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			b.scheduleSync()
		case line := <-ch:
			var ev agtermEvent
			if json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			b.onEvent(ev)
		}
	}
}

func (b *tgBot) onEvent(ev agtermEvent) {
	switch ev.Kind {
	case "status":
		if p := ev.Payload.Pane; p != "" && validPane(p) {
			b.mu.Lock()
			b.lastPane[strings.ToUpper(ev.Session)] = p
			b.mu.Unlock()
		}
		if ev.Payload.Status != ev.Payload.Previous && (ev.Payload.Status == "blocked" || ev.Payload.Status == "completed") {
			b.onStatus(ev.Window, strings.ToUpper(ev.Session), ev.Payload.Status, ev.Payload.Pane)
		}
		b.scheduleSync()
	case "session.closed":
		b.startArchive(b.ctx, strings.ToUpper(ev.Session))
	default:
		b.scheduleSync()
	}
}

func (b *tgBot) onStatus(window, sid, status, pane string) {
	if !idRe.MatchString(sid) {
		return
	}
	now := b.now()
	key := sid + "|" + status
	b.mu.Lock()
	// "✅ Готово" is redundant when the conversation itself is mirrored;
	// "🟠 Ждёт тебя" is not (permission prompts are not in the transcript).
	if !b.st.paired() || now.Sub(b.lastNotify[key]) < b.notifyDebounce || (status == "completed" && b.mirrored[sid]) {
		b.mu.Unlock()
		return
	}
	b.lastNotify[key] = now
	gen := b.gen
	b.mu.Unlock()
	b.after(b.notifyDelay, func() { b.notify(b.ctx, gen, sid, status, pane) })
}

// notify reads the screen and sends the notification. A transient failure
// (network, 5xx, flood control) is retried with backoff for about ten minutes.
func (b *tgBot) notify(ctx context.Context, gen uint64, sid, status, pane string) {
	s, err := b.findSession(ctx, sid)
	if err != nil || s == nil {
		return
	}
	pane = paneOr(pane)
	screen, err := b.srv.ctl.Text(ctx, s.Window, s.ID, pane, 0)
	if err != nil {
		screen = ""
	}
	b.mu.Lock()
	forum := b.st.Mode == "forum"
	b.mu.Unlock()
	header, lines, kb := "🟠 Ждёт тебя", 20, mainKeyboard(s.ID, pane)
	if status == "completed" {
		header, lines, kb = "✅ Готово", 10, doneKeyboard(s.ID, pane)
	}
	if !forum {
		// Budget: header ≤ ~200 chars + screen ≤ tgMaxScreen stays under Telegram's 4096.
		header += ": <b>" + html.EscapeString(truncRunes(sessionName(s.Session), tgMaxHeader)) + "</b>"
	}
	text := header + "\n" + screenPre(screen, lines)
	b.retrying(ctx, func() error { return b.sendToSession(ctx, gen, s, pane, text, kb) })
}

// retrying runs f now and, while it fails transiently, again after 5 s, 10 s,
// 20 s … (about ten minutes in all).
func (b *tgBot) retrying(ctx context.Context, f func() error) {
	var attempt func(n int, delay time.Duration)
	attempt = func(n int, delay time.Duration) {
		err := f()
		if !transient(err) || ctx.Err() != nil {
			return
		}
		if n >= tgRetries {
			log.Printf("%v (gave up after %d tries)", err, n+1)
			return
		}
		b.after(delay, func() { attempt(n+1, min(delay*2, 2*time.Minute)) })
	}
	attempt(0, 5*time.Second)
}

// sendToSession posts into the session's topic (forum) or the chat (private),
// remembering which session (and pane) a private-chat message belongs to.
func (b *tgBot) sendToSession(ctx context.Context, gen uint64, s *sessionRef, pane, text string, kb any) error {
	for attempt := 0; ; attempt++ {
		b.mu.Lock()
		chat, forum, cur := b.st.ChatID, b.st.Mode == "forum", b.gen
		b.mu.Unlock()
		if cur != gen {
			return errStaleGen
		}
		p := map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML", "reply_markup": kb,
			"disable_notification": b.silentFor(ctx, s.ID)}
		if forum {
			thread, err := b.ensureTopic(ctx, gen, s)
			if err != nil {
				return err
			}
			p["message_thread_id"] = thread
		}
		var out tgMessage
		err := b.out.call(ctx, chat, "sendMessage", p, &out)
		if err != nil && forum && attempt == 0 && topicGone(err) {
			b.forgetTopic(gen, s.ID) // deleted by hand: make a new one
			continue
		}
		if err != nil {
			log.Printf("%v", err)
			return err
		}
		if !forum {
			b.mu.Lock()
			if b.gen == gen {
				b.st.addMsgRef(out.MessageID, s.ID, pane)
				b.saveLocked()
			}
			b.mu.Unlock()
		}
		return nil
	}
}

func topicGone(err error) bool {
	var te *tgError
	if !errors.As(err, &te) {
		return false
	}
	d := strings.ToLower(te.Desc)
	return strings.Contains(d, "thread not found") || strings.Contains(d, "topic_id_invalid") || strings.Contains(d, "topic_deleted")
}

func (b *tgBot) forgetTopic(gen uint64, sid string) {
	b.mu.Lock()
	if b.gen == gen {
		delete(b.st.Topics, sid)
		b.saveLocked()
	}
	b.mu.Unlock()
}

/* ---------- forum topics ---------- */

func (b *tgBot) ensureTopic(ctx context.Context, gen uint64, s *sessionRef) (int64, error) {
	b.topicMu.Lock()
	defer b.topicMu.Unlock()
	return b.ensureTopicLocked(ctx, gen, *s)
}

// ensureTopicLocked returns the session's topic, creating it if needed; topicMu held.
// An archived (or archiving) topic is never reused: the session gets a new one.
// A topic created for an earlier pairing is not recorded.
func (b *tgBot) ensureTopicLocked(ctx context.Context, gen uint64, s sessionRef) (int64, error) {
	b.mu.Lock()
	ref := b.st.Topics[s.ID]
	chat, cur := b.st.ChatID, b.gen
	b.mu.Unlock()
	if cur != gen {
		return 0, errStaleGen
	}
	if ref != nil && !ref.Closing {
		return ref.Thread, nil
	}
	if ref != nil {
		return 0, errors.New("telegram: previous topic still being archived")
	}
	title := topicName(s)
	style := b.workspaceStyle(s.Workspace)
	params := b.topicCreateParams(chat, title, style)
	icon := ""
	if params["icon_custom_emoji_id"] != nil {
		icon = style.Icon
	}
	var t tgTopic
	if err := b.out.call(ctx, chat, "createForumTopic", params, &t); err != nil {
		log.Printf("%v", err)
		return 0, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gen != gen {
		return 0, errStaleGen
	}
	if b.st.Topics == nil {
		b.st.Topics = map[string]*tgTopicRef{}
	}
	b.st.Topics[s.ID] = &tgTopicRef{Thread: t.ThreadID, Title: title, Workspace: s.Workspace, Name: sessionName(s.Session), Icon: icon,
		Provisional: provisionalName(sessionName(s.Session), s.Cwd)}
	b.lastRename[s.ID] = b.now()
	b.saveLocked()
	return t.ThreadID, nil
}

func (b *tgBot) scheduleSync() {
	b.mu.Lock()
	if b.syncScheduled || !b.st.paired() || b.st.Mode != "forum" {
		b.mu.Unlock()
		return
	}
	b.syncScheduled = true
	b.mu.Unlock()
	b.after(syncDebounce, func() {
		b.mu.Lock()
		b.syncScheduled = false
		b.mu.Unlock()
		b.syncTopics(b.ctx)
	})
}

// syncTopics makes the forum match agterm: a topic per session, titled with
// status, workspace and name, so a status change, a rename or a move to another
// workspace all retitle it; topics of closed sessions are archived, resuming
// any archive step that failed before. Renames of one topic are at least
// renameGap apart; work that has to wait schedules another pass. A pass started
// for an earlier pairing stops at the first step after the pairing changes.
func (b *tgBot) syncTopics(ctx context.Context) {
	b.topicMu.Lock()
	defer b.topicMu.Unlock()
	b.mu.Lock()
	ok := b.st.paired() && b.st.Mode == "forum"
	chat, gen := b.st.ChatID, b.gen
	b.mu.Unlock()
	if !ok {
		return
	}
	stale := func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.gen != gen
	}
	b.refreshIcons(ctx)
	b.ensureLauncherLocked(ctx, gen)
	b.migrateTitlesLocked(ctx, gen)
	b.deleteArchivedLocked(ctx, gen)
	all, err := b.sessions(ctx)
	if err != nil {
		return
	}
	b.markProvisionalLocked(gen, all)
	seen := map[string]bool{}
	for _, s := range all {
		seen[s.ID] = true
	}
	pending, retry := false, false

	// First finish archives: sessions that closed, or vanished while the tree
	// is not empty (an empty tree is more likely agterm restarting).
	b.mu.Lock()
	var gone []string
	for sid, ref := range b.st.Topics {
		if ref.Closing || (!seen[sid] && len(all) > 0) {
			gone = append(gone, sid)
		}
	}
	b.mu.Unlock()
	sort.Strings(gone)
	for _, sid := range gone {
		if stale() {
			return
		}
		if err := b.archiveStepLocked(ctx, gen, sid); err != nil {
			retry = true
		}
	}

	for _, s := range all {
		if stale() {
			return
		}
		if _, err := b.ensureTopicLocked(ctx, gen, s); err != nil {
			retry = true
			continue // not an admin, not a forum, rate limited, still archiving: next pass
		}
		if b.settleNameLocked(ctx, gen, chat, s) {
			pending = true
		}
		b.mu.Lock()
		ref := b.st.Topics[s.ID]
		var thread int64
		var have, want, name string
		if ref != nil {
			// Only a move to another workspace retitles; the name stays the one
			// the topic got at creation (or its first real name, see above).
			thread, have, name = ref.Thread, ref.Title, ref.Name
			want = topicTitle("", s.Workspace, name, "")
		}
		wait := b.renameGap - b.now().Sub(b.lastRename[s.ID])
		b.mu.Unlock()
		if ref == nil || have == want {
			continue
		}
		if wait > 0 {
			pending = true
			continue
		}
		// One edit for the new name and the new workspace's icon (the color
		// cannot change after creation).
		newIcon := b.workspaceStyle(s.Workspace).Icon
		iconID := b.iconID(newIcon)
		err := b.renameTopic(ctx, chat, thread, want, iconID)
		if err != nil && topicGone(err) {
			b.forgetTopic(gen, s.ID)
			pending = true
			continue
		}
		if err != nil && !notModified(err) {
			log.Printf("%v", err)
			retry = retry || transient(err)
			continue
		}
		b.mu.Lock()
		if r := b.st.Topics[s.ID]; r != nil && b.gen == gen {
			r.Title, r.Workspace = want, s.Workspace
			if iconID != "" {
				r.Icon = newIcon
			}
			b.lastRename[s.ID] = b.now()
			b.saveLocked()
		}
		b.mu.Unlock()
	}
	switch {
	case pending:
		b.after(b.renameGap, b.scheduleSync)
	case retry:
		b.after(tgSyncRetry, b.scheduleSync)
	}
}

// provisionalName: an auto name agterm gives a fresh session (the folder's
// name, also what the launcher passes as --name), a program's name, or none.
// A topic named so gets its real name once Claude sets one.
func provisionalName(name, cwd string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "", "сессия", "claude", "codex", "zsh", "-zsh", "bash", "-bash", "fish", "sh":
		return true
	}
	return cwd != "" && n == strings.ToLower(filepath.Base(cwd))
}

const nameSettle = 10 * time.Second // a real name must hold this long

// settleNameLocked gives a provisional topic its session's real name, once,
// after the name has held for nameSettle (titles flicker while Claude
// starts). Returns true while a rename is waiting. topicMu held.
func (b *tgBot) settleNameLocked(ctx context.Context, gen uint64, chat int64, s sessionRef) bool {
	name := sessionName(s.Session)
	b.mu.Lock()
	ref := b.st.Topics[s.ID]
	if ref == nil || !ref.Provisional || ref.Closing {
		delete(b.nameCand, s.ID)
		b.mu.Unlock()
		return false
	}
	if provisionalName(name, s.Cwd) {
		delete(b.nameCand, s.ID)
		b.mu.Unlock()
		return false
	}
	c, ok := b.nameCand[s.ID]
	if !ok || c.name != name {
		b.nameCand[s.ID] = nameCand{name, b.now()}
		b.mu.Unlock()
		return true
	}
	if b.now().Sub(c.since) < nameSettle {
		b.mu.Unlock()
		return true
	}
	thread, hasIcon := ref.Thread, ref.Icon != ""
	b.mu.Unlock()
	title := topicTitle("", s.Workspace, name, "")
	icon, emoji := "", b.workspaceStyle(s.Workspace).Icon
	if !hasIcon {
		icon = b.iconID(emoji)
	}
	err := b.renameTopic(ctx, chat, thread, title, icon)
	if err != nil && !notModified(err) {
		if topicGone(err) {
			b.forgetTopic(gen, s.ID)
		} else {
			log.Printf("%v", err)
		}
		return true
	}
	b.mu.Lock()
	if r := b.st.Topics[s.ID]; r != nil && b.gen == gen {
		r.Name, r.Title, r.Workspace, r.Provisional = name, title, s.Workspace, false
		if icon != "" {
			r.Icon = emoji
		}
		b.lastRename[s.ID] = b.now()
		delete(b.nameCand, s.ID)
		b.saveLocked()
	}
	b.mu.Unlock()
	return false
}

type nameCand struct {
	name  string
	since time.Time
}

// markProvisionalLocked: once per state file, topics made before this rule
// whose name is an auto name are marked provisional. topicMu held.
func (b *tgBot) markProvisionalLocked(gen uint64, all []sessionRef) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.NamesV3 || b.gen != gen {
		return
	}
	cwd := map[string]string{}
	for _, s := range all {
		cwd[s.ID] = s.Cwd
	}
	for sid, r := range b.st.Topics {
		if !r.Closing && provisionalName(r.Name, cwd[sid]) {
			r.Provisional = true
		}
	}
	b.st.NamesV3 = true
	b.saveLocked()
}

// renameTopic retitles a topic and remembers it, so the "topic edited"
// service message the rename leaves in the topic can be deleted if Telegram
// tells the bot about it.
// An empty name or icon is left as it is.
func (b *tgBot) renameTopic(ctx context.Context, chat, thread int64, name, icon string) error {
	p := map[string]any{"chat_id": chat, "message_thread_id": thread}
	if name != "" {
		p["name"] = name
	}
	if icon != "" {
		p["icon_custom_emoji_id"] = icon
	}
	err := b.out.call(ctx, chat, "editForumTopic", p, nil)
	if err == nil {
		b.mu.Lock()
		b.renamed[thread] = b.now()
		b.mu.Unlock()
	}
	return err
}

// migrateTitlesLocked brings topics made by older versions up to date, once
// per state file: titles lose the status emoji (TitlesV2) and live topics get
// their workspace icon (IconsV1) — one edit per topic for both. Topics are
// never recreated. The icon part waits until the allowed icons are known.
// topicMu held.
func (b *tgBot) migrateTitlesLocked(ctx context.Context, gen uint64) {
	b.mu.Lock()
	titles, icons := !b.st.TitlesV2, !b.st.IconsV1 && len(b.st.Icons) > 0
	if (!titles && !icons) || b.gen != gen {
		b.mu.Unlock()
		return
	}
	chat, launcher := b.st.ChatID, b.st.LauncherThread
	type job struct {
		sid, title, ws string
		thread         int64
		needIcon       bool
	}
	var jobs []job
	for sid, r := range b.st.Topics {
		if r.Closing {
			continue
		}
		j := job{sid: sid, thread: r.Thread, ws: r.Workspace, needIcon: icons && r.Icon == ""}
		if titles && r.Title != refTitle(r) {
			j.title = refTitle(r)
		}
		jobs = append(jobs, j)
	}
	b.mu.Unlock()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].sid < jobs[j].sid })
	for _, j := range jobs {
		icon, emoji := "", b.workspaceStyle(j.ws).Icon
		if j.needIcon {
			icon = b.iconID(emoji)
		}
		if j.title == "" && icon == "" {
			continue
		}
		err := b.renameTopic(ctx, chat, j.thread, j.title, icon)
		if err != nil && !notModified(err) && !topicGone(err) {
			log.Printf("%v (topic migration: next sync)", err)
			return
		}
		b.mu.Lock()
		if r := b.st.Topics[j.sid]; r != nil && b.gen == gen {
			if j.title != "" {
				r.Title = j.title
			}
			if icon != "" {
				r.Icon = emoji
			}
			b.saveLocked()
		}
		b.mu.Unlock()
	}
	b.mu.Lock()
	launcherDone := b.st.LauncherIcon
	b.mu.Unlock()
	if icons && launcher != 0 && !launcherDone {
		if icon := b.iconID(launcherStyle.Icon); icon != "" {
			if err := b.renameTopic(ctx, chat, launcher, "", icon); err != nil && !notModified(err) && !topicGone(err) {
				log.Printf("%v (topic migration: next sync)", err)
				return
			}
		}
	}
	b.mu.Lock()
	if b.gen == gen {
		b.st.TitlesV2 = true
		if icons {
			b.st.IconsV1 = true
			if launcher != 0 && b.st.LauncherThread == launcher && b.iconAllowedLocked(launcherStyle.Icon) {
				b.st.LauncherIcon = true
			}
		}
		b.saveLocked()
	}
	b.mu.Unlock()
}

// dropRenameNotice deletes the "topic edited" service message a bot rename
// left in a topic. Telegram may not send a bot its own service messages; when
// it does, this keeps the topic clean.
func (b *tgBot) dropRenameNotice(ctx context.Context, m *tgMessage) bool {
	if len(m.ForumTopicEdited) == 0 || string(m.ForumTopicEdited) == "null" || m.From == nil || m.From.ID != b.botID {
		return false
	}
	b.mu.Lock()
	at, ok := b.renamed[m.MessageThreadID]
	mine := ok && b.now().Sub(at) < time.Minute && m.Chat.ID == b.st.ChatID && b.st.paired()
	b.mu.Unlock()
	if mine {
		b.out.call(ctx, m.Chat.ID, "deleteMessage", map[string]any{"chat_id": m.Chat.ID, "message_id": m.MessageID}, nil)
	}
	return true
}

func (b *tgBot) iconAllowedLocked(emoji string) bool { return b.st.Icons[normEmoji(emoji)] != "" }

func notModified(err error) bool {
	u := strings.ToUpper(fmt.Sprint(err))
	return err != nil && (strings.Contains(u, "NOT_MODIFIED") || strings.Contains(u, "NOT MODIFIED"))
}

// startArchive marks a closed session's topic for archiving (saved, so a
// restart resumes it) and runs the steps.
func (b *tgBot) startArchive(ctx context.Context, sid string) {
	b.topicMu.Lock()
	defer b.topicMu.Unlock()
	b.mu.Lock()
	ref, gen := b.st.Topics[sid], b.gen
	if ref == nil || b.st.Mode != "forum" {
		b.mu.Unlock()
		return
	}
	ref.Closing = true
	b.saveLocked()
	b.mu.Unlock()
	if err := b.archiveStepLocked(ctx, gen, sid); err != nil {
		b.after(tgSyncRetry, b.scheduleSync)
	}
}

// archiveStepLocked deletes a closed session's topic and everything kept
// about it (mapping, message refs; its open cards become stale). A transient
// failure keeps the "closing" mark, so the next sync tries again; a permanent
// one (the topic is already gone) just drops the state. topicMu held.
func (b *tgBot) archiveStepLocked(ctx context.Context, gen uint64, sid string) error {
	b.mu.Lock()
	ref := b.st.Topics[sid]
	chat, cur := b.st.ChatID, b.gen
	var thread int64
	if ref != nil {
		ref.Closing = true
		thread = ref.Thread
	}
	b.mu.Unlock()
	if ref == nil || cur != gen {
		return nil
	}
	err := b.out.call(ctx, chat, "deleteForumTopic", map[string]any{"chat_id": chat, "message_thread_id": thread}, nil)
	if err != nil && transient(err) {
		log.Printf("%v", err)
		return err
	}
	if err != nil {
		log.Printf("%v (topic state dropped)", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gen == gen {
		b.dropSessionLocked(sid)
		b.saveLocked()
	}
	return nil
}

// dropSessionLocked forgets a closed session: its topic, its message refs,
// and its open cards (a reply to one now gets "уже отменена"); b.mu held.
// Mirror followers drop by themselves (the session is no longer in the tree).
func (b *tgBot) dropSessionLocked(sid string) {
	delete(b.st.Topics, sid)
	refs := b.st.MsgRefs[:0]
	for _, r := range b.st.MsgRefs {
		if !strings.EqualFold(r.Session, sid) {
			refs = append(refs, r)
		}
	}
	b.st.MsgRefs = refs
	for _, c := range b.st.Cards {
		if strings.EqualFold(c.Session, sid) && c.open() {
			c.Status = "cancelled"
		}
	}
	if strings.EqualFold(b.st.Current, sid) {
		b.st.Current = ""
	}
}

// deleteArchivedLocked: once, delete the topics older versions archived
// ("📦 … (архив)"). Live topics and the launcher are not touched. topicMu held.
func (b *tgBot) deleteArchivedLocked(ctx context.Context, gen uint64) {
	b.mu.Lock()
	if b.st.ArchivedDeletedV1 || b.gen != gen {
		b.mu.Unlock()
		return
	}
	chat := b.st.ChatID
	list := append([]tgArchived(nil), b.st.Archived...)
	b.mu.Unlock()
	for _, a := range list {
		err := b.out.call(ctx, chat, "deleteForumTopic", map[string]any{"chat_id": chat, "message_thread_id": a.Thread}, nil)
		if err != nil && transient(err) {
			log.Printf("%v (archived topics: next sync)", err)
			return
		}
		b.mu.Lock()
		if b.gen == gen {
			kept := b.st.Archived[:0]
			for _, x := range b.st.Archived {
				if x.Thread != a.Thread {
					kept = append(kept, x)
				}
			}
			b.st.Archived = kept
			b.saveLocked()
		}
		b.mu.Unlock()
	}
	b.mu.Lock()
	if b.gen == gen {
		b.st.Archived, b.st.ArchivedDeletedV1 = nil, true
		b.saveLocked()
	}
	b.mu.Unlock()
}
