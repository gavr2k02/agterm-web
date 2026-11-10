package main

// "🚀 Новая сессия": a fixed forum topic whose pinned message starts agterm
// sessions in three taps (folder → program → workspace), edited in place.
// Also `/new <folder> [claude|claude-continue|codex|shell] [workspace]` anywhere.
//
// A session is started the way a person would: `session new --cwd <dir>` opens
// the user's normal interactive login shell there, then the program's name is
// typed into it. So zsh functions and aliases apply (the user's `claude` is a
// function wrapping `cswap run` in some folders), and the shell stays when the
// program exits. `--command "zsh -lic …"` would need shell quoting, behaves
// differently in agterm's Live sessions mode, and closes the session when the
// program ends. Program strings are fixed constants; the only user input that
// reaches agterm is the folder, validated and passed as one --cwd argument, and
// a workspace name passed as one --workspace-name argument (exec, no shell).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	launcherTitle      = "🚀 Новая сессия"
	maxRecentDirs      = 12
	maxDirButtons      = 10
	maxWSButtons       = 20
	launchWait         = 15 * time.Second // for the new session to show up in the tree
	launchPoll         = 300 * time.Millisecond
	maxWorkspaceLen    = 64
	launcherCheckEvery = 10 * time.Minute
)

type launchProgram struct{ key, label, cmd string }

// The only things ever typed into a new session.
var launchPrograms = []launchProgram{
	{"claude", "Claude", "claude"},
	{"claude-continue", "Claude — продолжить", "claude --continue"},
	{"codex", "Codex", "codex"},
	{"shell", "Терминал", ""},
}

func programByKey(k string) *launchProgram {
	for i := range launchPrograms {
		if launchPrograms[i].key == k {
			return &launchPrograms[i]
		}
	}
	return nil
}

type tgRecentDir struct {
	Path string    `json:"path"`
	Uses int       `json:"uses"`
	Last time.Time `json:"last"`
}

type wsOption struct{ ID, Window, Name string }

// launchDraft is the launcher message's state; one user, so one draft.
type launchDraft struct {
	step  int      // 1 folder, 2 program, 3 workspace
	dirs  []string // folder buttons shown in step 1
	dir   string
	prog  string
	wss   []wsOption // workspace buttons shown in step 3
	ws    int        // chosen index into wss; -1 = a new one named newWS
	newWS string
	await string // "path" or "ws": the next text in the launcher topic answers it
}

type launchReq struct {
	dir, prog         string
	wsID, window, new string
	chat, thread      int64 // where to report
}

/* ---------- folders ---------- */

// resolveDir accepts "~/x", "x" (under home) or an absolute path; it must be an
// existing directory inside the home folder (home given in canonical form).
// It returns the canonical path, symlinks resolved: that is what gets launched,
// and it is checked again right before launching.
func resolveDir(raw, home string) (string, error) {
	p := strings.TrimSpace(raw)
	switch {
	case p == "":
		return "", errors.New("пустой путь")
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	case !filepath.IsAbs(p):
		p = filepath.Join(home, p)
	}
	p = filepath.Clean(p)
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", errors.New("нет такой папки")
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(realHome, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("только папки внутри домашней")
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return "", errors.New("это не папка")
	}
	return real, nil
}

var loginShells = map[string]bool{"zsh": true, "bash": true, "fish": true}

// shellState: ready when the pane's foreground is the login shell itself
// (agterm reports foregroundShell and no foreground, or an argv of just the
// shell); busy when anything else runs; neither while agterm cannot tell yet.
func shellState(s Session) (ready, busy bool) {
	base := func(p string) string { return strings.TrimPrefix(filepath.Base(p), "-") }
	switch len(s.Foreground) {
	case 0:
		if s.ForegroundShell == "" {
			return false, false
		}
		return loginShells[base(s.ForegroundShell)], !loginShells[base(s.ForegroundShell)]
	case 1:
		if loginShells[base(s.Foreground[0])] {
			return true, false
		}
	}
	return false, true
}

func tildePath(p, home string) string {
	if p == home {
		return "~"
	}
	if r, ok := strings.CutPrefix(p, home+"/"); ok {
		return "~/" + r
	}
	return p
}

func (b *tgBot) favorites() []string {
	raw, err := os.ReadFile(b.launcherFile)
	if err != nil {
		return nil
	}
	var cfg struct {
		Favorites []string `json:"favorites"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		log.Printf("telegram: %s: bad JSON", b.launcherFile)
		return nil
	}
	return cfg.Favorites
}

// dirOptions: favorites, then recently launched folders (most used first),
// then folders of open sessions (most sessions first) — existing, deduplicated.
func (b *tgBot) dirOptions(all []sessionRef) []string {
	var cands []string
	cands = append(cands, b.favorites()...)
	b.mu.Lock()
	recent := append([]tgRecentDir(nil), b.st.Recent...)
	b.mu.Unlock()
	sort.SliceStable(recent, func(i, j int) bool {
		if recent[i].Uses != recent[j].Uses {
			return recent[i].Uses > recent[j].Uses
		}
		return recent[i].Last.After(recent[j].Last)
	})
	for _, r := range recent {
		cands = append(cands, r.Path)
	}
	count := map[string]int{}
	var cwds []string
	for _, s := range all {
		if s.Cwd != "" && s.RemoteHost == "" {
			if count[s.Cwd] == 0 {
				cwds = append(cwds, s.Cwd)
			}
			count[s.Cwd]++
		}
	}
	sort.SliceStable(cwds, func(i, j int) bool { return count[cwds[i]] > count[cwds[j]] })
	cands = append(cands, cwds...)

	seen := map[string]bool{}
	var out []string
	for _, c := range cands {
		p, err := resolveDir(c, b.home)
		if err != nil || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) == maxDirButtons {
			break
		}
	}
	return out
}

func (b *tgBot) rememberDir(dir string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for i := range b.st.Recent {
		if b.st.Recent[i].Path == dir {
			b.st.Recent[i].Uses++
			b.st.Recent[i].Last = now
			b.saveLocked()
			return
		}
	}
	b.st.Recent = append(b.st.Recent, tgRecentDir{dir, 1, now})
	if len(b.st.Recent) > maxRecentDirs {
		// Drop the least used, oldest first.
		sort.SliceStable(b.st.Recent, func(i, j int) bool {
			if b.st.Recent[i].Uses != b.st.Recent[j].Uses {
				return b.st.Recent[i].Uses > b.st.Recent[j].Uses
			}
			return b.st.Recent[i].Last.After(b.st.Recent[j].Last)
		})
		b.st.Recent = b.st.Recent[:maxRecentDirs]
	}
	b.saveLocked()
}

/* ---------- workspaces ---------- */

func (b *tgBot) workspaceOptions(ctx context.Context) ([]wsOption, error) {
	tree, err := b.srv.ctl.Tree(ctx)
	if err != nil {
		return nil, err
	}
	var out []wsOption
	for _, ws := range tree {
		out = append(out, wsOption{ws.ID, ws.Window, ws.Name})
		if len(out) == maxWSButtons {
			break
		}
	}
	return out, nil
}

// guessWorkspace: the workspace holding most sessions in dir, else the first.
func guessWorkspace(wss []wsOption, all []sessionRef, dir string) int {
	count := map[string]int{}
	for _, s := range all {
		if s.Cwd == dir {
			count[s.Workspace]++
		}
	}
	best, n := 0, 0
	for i, w := range wss {
		if count[w.Name] > n {
			best, n = i, count[w.Name]
		}
	}
	return best
}

func validWorkspaceName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" || utf8.RuneCountInString(n) > maxWorkspaceLen {
		return "", fmt.Errorf("имя воркспейса — от 1 до %d символов", maxWorkspaceLen)
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return "", errors.New("в имени воркспейса управляющие символы")
		}
	}
	return n, nil
}

/* ---------- the launcher message ---------- */

func (b *tgBot) renderLauncher(d *launchDraft) (string, tgInlineKeyboard) {
	var lines []string
	lines = append(lines, "🚀 <b>Новая сессия</b>")
	if d.dir != "" && d.step > 1 {
		lines = append(lines, "Папка: <code>"+html.EscapeString(tildePath(d.dir, b.home))+"</code>")
	}
	if p := programByKey(d.prog); p != nil && d.step > 2 {
		lines = append(lines, "Программа: "+html.EscapeString(p.label))
	}
	var rows [][]tgButton
	switch d.step {
	case 1:
		if d.await == "path" {
			lines = append(lines, "", "Пришли путь к папке сообщением в эту тему, например <code>~/work-projects/app</code>.")
		} else {
			lines = append(lines, "", "Шаг 1 из 3: папка")
		}
		for i, dir := range d.dirs {
			rows = append(rows, []tgButton{{Text: "📂 " + truncRunes(tildePath(dir, b.home), 60), Data: "L|d|" + strconv.Itoa(i)}})
		}
		rows = append(rows, []tgButton{{Text: "📁 Другая…", Data: "L|d|o"}})
	case 2:
		lines = append(lines, "", "Шаг 2 из 3: что запустить")
		rows = append(rows,
			[]tgButton{{Text: "Claude", Data: "L|p|claude"}, {Text: "Claude — продолжить", Data: "L|p|claude-continue"}},
			[]tgButton{{Text: "Codex", Data: "L|p|codex"}, {Text: "Терминал", Data: "L|p|shell"}},
			[]tgButton{{Text: "⬅️ Сначала", Data: "L|r"}})
	case 3:
		if d.await == "ws" {
			lines = append(lines, "", "Пришли имя нового воркспейса сообщением в эту тему.")
		} else {
			lines = append(lines, "", "Шаг 3 из 3: воркспейс")
		}
		for i, w := range d.wss {
			label := w.Name
			if i == d.ws {
				label = "● " + label
			}
			rows = append(rows, []tgButton{{Text: truncRunes(label, 60), Data: "L|w|" + strconv.Itoa(i)}})
		}
		newLabel := "➕ Создать новый…"
		if d.ws == -1 && d.newWS != "" {
			newLabel = "● ➕ " + truncRunes(d.newWS, 50)
		}
		rows = append(rows, []tgButton{{Text: newLabel, Data: "L|w|n"}})
		rows = append(rows, []tgButton{{Text: "▶️ Запустить", Data: "L|go"}, {Text: "⬅️ Сначала", Data: "L|r"}})
	}
	return strings.Join(lines, "\n"), tgInlineKeyboard{rows}
}

// freshDraft starts at step 1 with current folder options.
func (b *tgBot) freshDraft(ctx context.Context) *launchDraft {
	all, _ := b.sessions(ctx)
	return &launchDraft{step: 1, dirs: b.dirOptions(all), ws: -1}
}

// ensureLauncherLocked keeps the launcher topic and its pinned message alive:
// it creates them when missing, checks now and then (a no-op edit) that the
// message still exists, recreating both if it was deleted, and pins until a pin
// is confirmed — and again on each check, which repairs a manual unpin. The
// launcher topic is never archived. topicMu held.
func (b *tgBot) ensureLauncherLocked(ctx context.Context, gen uint64) {
	b.mu.Lock()
	chat, cur := b.st.ChatID, b.gen
	thread, msgID, pinned := b.st.LauncherThread, b.st.LauncherMsg, b.st.LauncherPinned
	due := b.now().Sub(b.launcherCheck) >= launcherCheckEvery
	d := b.draft
	b.mu.Unlock()
	if cur != gen {
		return
	}
	if thread != 0 && msgID != 0 {
		if pinned && !due {
			return
		}
		if d == nil {
			d = b.freshDraft(ctx)
			b.setDraft(d)
		}
		// The no-op edit sends the controls of the draft as it is; if the user
		// moved on while it was in flight, send the newer ones again, so a slow
		// check never leaves older buttons behind.
		var err error
		for try := 0; try < 3; try++ {
			b.mu.Lock()
			d, ver := b.draft, b.draftVer
			b.mu.Unlock()
			_, kb := b.renderLauncher(d)
			err = b.out.call(ctx, chat, "editMessageReplyMarkup", map[string]any{"chat_id": chat, "message_id": msgID, "reply_markup": kb}, nil)
			b.mu.Lock()
			moved := b.draftVer != ver
			b.mu.Unlock()
			if !moved || (err != nil && !notModified(err)) {
				break
			}
		}
		switch {
		case err == nil || notModified(err):
			b.pinLauncher(ctx, gen, chat, msgID)
			return
		case launcherGone(err):
			log.Printf("telegram: launcher message is gone, recreating it")
			b.mu.Lock()
			if b.gen == gen {
				b.st.LauncherThread, b.st.LauncherMsg, b.st.LauncherPinned = 0, 0, false
				b.saveLocked()
			}
			b.mu.Unlock()
		default:
			log.Printf("%v", err) // transient: the next sync tries again
			return
		}
	}
	var t tgTopic
	if err := b.out.call(ctx, chat, "createForumTopic", b.topicCreateParams(chat, launcherTitle, launcherStyle), &t); err != nil {
		log.Printf("%v", err)
		return
	}
	d = b.freshDraft(ctx)
	text, kb := b.renderLauncher(d)
	var msg tgMessage
	if err := b.out.call(ctx, chat, "sendMessage", map[string]any{
		"chat_id": chat, "message_thread_id": t.ThreadID, "text": text, "parse_mode": "HTML", "reply_markup": kb,
	}, &msg); err != nil {
		log.Printf("%v", err)
		return
	}
	hasIcon := b.iconID(launcherStyle.Icon) != "" // takes b.mu itself
	b.mu.Lock()
	if b.gen == gen {
		b.st.LauncherThread, b.st.LauncherMsg, b.st.LauncherPinned = t.ThreadID, msg.MessageID, false
		b.st.LauncherIcon = hasIcon
		b.draft = d
		b.draftVer++
		b.saveLocked()
	}
	b.mu.Unlock()
	b.pinLauncher(ctx, gen, chat, msg.MessageID)
}

// pinLauncher pins the launcher message; the pin counts only when confirmed,
// so a failure is retried on the next sync.
func (b *tgBot) pinLauncher(ctx context.Context, gen uint64, chat, msgID int64) {
	err := b.out.call(ctx, chat, "pinChatMessage", map[string]any{"chat_id": chat, "message_id": msgID, "disable_notification": true}, nil)
	if err != nil {
		log.Printf("%v (will retry)", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gen == gen && b.st.LauncherMsg == msgID {
		b.st.LauncherPinned = err == nil
		if err == nil {
			b.launcherCheck = b.now()
		}
		b.saveLocked()
	}
}

func launcherGone(err error) bool {
	u := strings.ToLower(fmt.Sprint(err))
	return topicGone(err) || strings.Contains(u, "message to edit not found") || strings.Contains(u, "message_id_invalid") ||
		strings.Contains(u, "message can't be edited")
}

// showLauncher edits the launcher message to the draft. If the message or its
// topic is gone, both are forgotten and the next sync recreates them.
func (b *tgBot) showLauncher(ctx context.Context, chat, msgID int64, d *launchDraft) {
	text, kb := b.renderLauncher(d)
	err := b.out.call(ctx, chat, "editMessageText", map[string]any{
		"chat_id": chat, "message_id": msgID, "text": text, "parse_mode": "HTML", "reply_markup": kb,
	}, nil)
	if err == nil || strings.Contains(err.Error(), "not modified") {
		return
	}
	log.Printf("%v", err)
	if launcherGone(err) {
		b.mu.Lock()
		if msgID == b.st.LauncherMsg {
			b.st.LauncherThread, b.st.LauncherMsg, b.st.LauncherPinned = 0, 0, false
			b.saveLocked()
		}
		b.mu.Unlock()
		b.scheduleSync()
	}
}

func (b *tgBot) launcherCallback(ctx context.Context, c *tgCallback, parts []string) string {
	chat, msgID := c.Message.Chat.ID, c.Message.MessageID
	b.mu.Lock()
	d := b.draft
	b.mu.Unlock()
	if d == nil && len(parts) > 0 && parts[0] == "go" {
		return "Уже запускаю"
	}
	if d == nil || len(parts) == 0 || parts[0] == "r" {
		d = b.freshDraft(ctx)
		b.setDraft(d)
		b.showLauncher(ctx, chat, msgID, d)
		if len(parts) > 0 && parts[0] == "r" {
			return ""
		}
		return "Меню обновлено, выбери ещё раз"
	}
	// Drafts are never changed once published (renders and the periodic
	// check read them without a lock): work on a copy, publish it at the end.
	orig := d
	cp := *d
	d = &cp
	arg := ""
	if len(parts) > 1 {
		arg = parts[1]
	}
	toast := ""
	switch parts[0] {
	case "d":
		if arg == "o" {
			d.await = "path"
			break
		}
		i, err := strconv.Atoi(arg)
		if err != nil || i < 0 || i >= len(d.dirs) {
			return "Устаревшая кнопка"
		}
		d.dir, d.step, d.await = d.dirs[i], 2, ""
	case "p":
		if d.dir == "" || programByKey(arg) == nil {
			return "Устаревшая кнопка"
		}
		d.prog = arg
		if err := b.loadWorkspaces(ctx, d); err != nil {
			return errText(err)
		}
		d.step = 3
	case "w":
		if d.step != 3 {
			return "Устаревшая кнопка"
		}
		if arg == "n" {
			d.await = "ws"
			break
		}
		i, err := strconv.Atoi(arg)
		if err != nil || i < 0 || i >= len(d.wss) {
			return "Устаревшая кнопка"
		}
		d.ws, d.newWS, d.await = i, "", ""
	case "go":
		if d.step != 3 || d.dir == "" || programByKey(d.prog) == nil || (d.ws < 0 && d.newWS == "") {
			return "Выбери папку, программу и воркспейс"
		}
		req := launchReq{dir: d.dir, prog: d.prog, chat: chat, thread: c.Message.MessageThreadID}
		if d.ws >= 0 {
			req.wsID, req.window = d.wss[d.ws].ID, d.wss[d.ws].Window
		} else {
			req.new = d.newWS
		}
		b.mu.Lock()
		gen := b.gen
		if b.draft != orig { // a second tap on the same menu
			b.mu.Unlock()
			return "Уже запускаю"
		}
		b.draft = nil // used up: one tap, one session
		b.mu.Unlock()
		b.after(0, func() {
			b.launch(b.ctx, gen, req)
			if !b.genIs(gen) {
				return
			}
			nd := b.freshDraft(b.ctx)
			b.setDraft(nd)
			b.showLauncher(b.ctx, chat, msgID, nd)
		})
		return "Запускаю…"
	default:
		return "Устаревшая кнопка"
	}
	b.setDraft(d)
	b.showLauncher(ctx, chat, msgID, d)
	return toast
}

func (b *tgBot) setDraft(d *launchDraft) {
	b.mu.Lock()
	b.draft = d
	b.draftVer++
	b.mu.Unlock()
}

func (b *tgBot) loadWorkspaces(ctx context.Context, d *launchDraft) error {
	wss, err := b.workspaceOptions(ctx)
	if err != nil {
		return err
	}
	all, _ := b.sessions(ctx)
	d.wss, d.ws = wss, guessWorkspace(wss, all, d.dir)
	if len(wss) == 0 {
		d.ws = -1
	}
	return nil
}

// launcherText handles text in the launcher topic: answers to "📁 Другая…" and
// "➕ Создать новый…"; anything else gets a hint. Never typed into a session.
func (b *tgBot) launcherText(ctx context.Context, m *tgMessage) {
	b.mu.Lock()
	d, msgID := b.draft, b.st.LauncherMsg
	b.mu.Unlock()
	if d != nil {
		cp := *d // copy on write, as in launcherCallback
		d = &cp
	}
	switch {
	case d != nil && d.await == "path":
		dir, err := resolveDir(m.Text, b.home)
		if err != nil {
			b.reply(ctx, m, "Не подходит: "+err.Error())
			return
		}
		d.dir, d.step, d.await = dir, 2, ""
	case d != nil && d.await == "ws":
		name, err := validWorkspaceName(m.Text)
		if err != nil {
			b.reply(ctx, m, "Не подходит: "+err.Error())
			return
		}
		d.newWS, d.ws, d.await = name, -1, ""
	default:
		b.reply(ctx, m, "Здесь запускаются новые сессии: кнопки в закреплённом сообщении или /new <папка> [claude|claude-continue|codex|shell] [воркспейс].")
		return
	}
	b.setDraft(d)
	b.showLauncher(ctx, m.Chat.ID, msgID, d)
}

// handleNew: /new <folder> [program] [workspace name…]
func (b *tgBot) handleNew(ctx context.Context, m *tgMessage, arg string) {
	f := strings.Fields(arg)
	if len(f) == 0 {
		b.reply(ctx, m, "Формат: /new <папка> [claude|claude-continue|codex|shell] [воркспейс]")
		return
	}
	dir, err := resolveDir(f[0], b.home)
	if err != nil {
		b.reply(ctx, m, "Папка не подходит: "+err.Error())
		return
	}
	prog, rest := "claude", f[1:]
	if len(rest) > 0 && programByKey(strings.ToLower(rest[0])) != nil {
		prog, rest = strings.ToLower(rest[0]), rest[1:]
	}
	wss, err := b.workspaceOptions(ctx)
	if err != nil {
		b.reply(ctx, m, "Не удалось получить воркспейсы: "+errText(err))
		return
	}
	req := launchReq{dir: dir, prog: prog, chat: m.Chat.ID}
	if m.IsTopicMessage {
		req.thread = m.MessageThreadID
	}
	if len(rest) > 0 {
		name, err := validWorkspaceName(strings.Join(rest, " "))
		if err != nil {
			b.reply(ctx, m, err.Error())
			return
		}
		req.new = name
		for _, w := range wss {
			if strings.EqualFold(w.Name, name) {
				req.wsID, req.window, req.new = w.ID, w.Window, ""
				break
			}
		}
	} else if len(wss) > 0 {
		all, _ := b.sessions(ctx)
		w := wss[guessWorkspace(wss, all, dir)]
		req.wsID, req.window = w.ID, w.Window
	} else {
		req.new = "agterm"
	}
	b.mu.Lock()
	gen := b.gen
	b.mu.Unlock()
	b.after(0, func() { b.launch(b.ctx, gen, req) })
}

/* ---------- launching ---------- */

// launch creates the session, types the program into its shell and reports
// "✅ Запущено: name → topic link" where it was asked. Everything is dropped
// when the pairing changed since the request (checked again before each step
// that touches agterm); the folder is validated again right before creation;
// the program goes in only into a fresh login shell, under the pane's write
// lock, and is never retyped.
func (b *tgBot) launch(ctx context.Context, gen uint64, r launchReq) {
	b.launchMu.Lock()
	defer b.launchMu.Unlock()
	// A re-pairing cancels this context, so waits and agterm calls stop too.
	ctx, cancel := b.pairingContext(ctx, gen)
	defer cancel()
	if !b.genIs(gen) {
		return
	}
	report := func(text string) {
		if !b.genIs(gen) {
			return
		}
		p := map[string]any{"chat_id": r.chat, "text": text, "parse_mode": "HTML"}
		if r.thread != 0 {
			p["message_thread_id"] = r.thread
		}
		b.send(ctx, p)
	}
	prog := programByKey(r.prog)
	if prog == nil {
		return
	}
	if dir, err := resolveDir(r.dir, b.home); err != nil || dir != r.dir {
		report("❌ Папка изменилась или пропала, не запускаю: <code>" + html.EscapeString(tildePath(r.dir, b.home)) + "</code>")
		return
	}
	args := []string{"session", "new", "--cwd", r.dir, "--no-select", "--name", filepath.Base(r.dir)}
	if r.new != "" {
		args = append(args, "--workspace-name", r.new, "--create-workspace")
	} else {
		args = append(args, "--workspace", r.wsID)
		if r.window != "" {
			args = append(args, "--window", r.window)
		}
	}
	if !b.genIs(gen) {
		return
	}
	id, err := b.srv.ctl.NewSession(ctx, args...)
	if err != nil {
		report("❌ Не запустилось: " + html.EscapeString(errText(err)))
		return
	}
	b.rememberDir(r.dir)

	// Wait for the session to show up and, if a program is to be started, for
	// its shell to be ready.
	failed := "⚠️ Не удалось запустить " + html.EscapeString(prog.label) + ", сессия открыта."
	var s *sessionRef
	for deadline := b.now().Add(launchWait); ; {
		s, _ = b.findSession(ctx, id)
		if s != nil {
			ready, busy := shellState(s.Session)
			if prog.cmd == "" || ready {
				break
			}
			if busy {
				report("⚠️ Сессия занята, программа не запущена.")
				return
			}
		}
		if !b.now().Before(deadline) {
			if s == nil {
				report("⚠️ Сессия создана, но не появилась в agterm за 15 с.")
			} else {
				report(failed)
			}
			return
		}
		if b.pollSleep(ctx, launchPoll) != nil {
			return
		}
	}
	if prog.cmd != "" {
		if !b.genIs(gen) {
			return
		}
		// Hold the pane's write lock from the last readiness check through the
		// typing, so a submit from the web or the bot cannot get in between.
		mu := b.srv.paneLock(s.Window, s.ID, "left")
		mu.Lock()
		if !b.genIs(gen) || ctx.Err() != nil { // re-paired while waiting for the pane
			mu.Unlock()
			return
		}
		fresh, err := b.findSession(ctx, s.ID)
		ready := false
		if err == nil && fresh != nil {
			ready, _ = shellState(fresh.Session)
		}
		// Input from the web or the bot that got in before this lock may not
		// show in the foreground yet (a command still starting, a half line).
		if b.srv.wasWritten(s.ID) {
			ready = false
		}
		if !b.genIs(gen) || ctx.Err() != nil {
			mu.Unlock()
			return
		}
		if !ready {
			mu.Unlock()
			report("⚠️ Сессия занята, программа не запущена.")
			return
		}
		chunks, _ := typeChunks("submit", prog.cmd)
		err = b.srv.typeLocked(ctx, s.Window, s.ID, "left", chunks)
		mu.Unlock()
		if err != nil {
			// Part of it may have gone in: never type it again.
			report(failed)
			return
		}
	}
	name := html.EscapeString(sessionName(s.Session))
	b.mu.Lock()
	forum, chat := b.st.Mode == "forum", b.st.ChatID
	if !forum && b.gen == gen {
		b.st.Current = s.ID
		b.saveLocked()
	}
	b.mu.Unlock()
	if !forum {
		report("✅ Запущено: <b>" + name + "</b>. Пиши сюда — текст уйдёт в неё.")
		return
	}
	thread, err := b.ensureTopic(ctx, gen, s)
	if err != nil {
		report("✅ Запущено: <b>" + name + "</b> (тему создать не удалось)")
		return
	}
	report(fmt.Sprintf(`✅ Запущено: <a href="%s">%s</a>`, topicURL(chat, thread), name))
}
