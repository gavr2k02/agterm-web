package main

// Mirroring Claude Code conversations into the sessions' topics.
//
// Which transcript belongs to which agterm session comes from the SessionStart
// hook (hooks/claude-session-start.sh), which saves its input as
// claude-sessions/<AGTERM_SESSION_ID>.<pane>.json; the newest file per session
// and pane wins (/clear starts a new transcript). For a Claude started before
// the hook existed there is a fallback: the newest transcript of its folder, if
// it is the only Claude running there.
//
// Transcripts are tailed from where mirroring began (no history dump); offsets
// are saved so a restart does not resend. What goes out: prompts typed at the
// Mac (prompts sent from Telegram are not echoed), `!` shell commands with
// their output, Claude's text (Markdown to Telegram HTML), and one "🔧" line
// per turn listing the tools, edited in place. Thinking, tool results,
// subagents, meta and hook entries are left out.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	mirrorEvery       = 1500 * time.Millisecond
	mirrorMapEvery    = 5 * time.Second
	mirrorReadMax     = 1 << 20          // bytes read from one transcript per tick
	mirrorHoldMax     = 10 * time.Minute // text held for a turn end that does not come goes out as intermediate
	mirrorEditGap     = 3 * time.Second
	mirrorToolItems   = 15
	mirrorEchoWindow  = 3 * time.Minute
	mirrorFallbackAge = 30 * time.Minute
	mirrorOpTries     = 20
	mirrorPromptMax   = 3500
	mirrorBashOutMax  = 1500
	mirrorBashFold    = 8             // output lines from which it goes into an expandable quote
	mirrorBgMax       = 2 * time.Hour // a background task with no end notice stops counting after this
	defaultLoudAfter  = 15 * time.Minute
)

/* ---------- mapping ---------- */

// claudeMapping is what the SessionStart hook saved for one agterm session pane.
type claudeMapping struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Source         string `json:"source"` // startup | resume | clear | compact | (fallback)
	session, pane  string
	mtime          time.Time
}

var reMapName = regexp.MustCompile(`^([0-9A-Fa-f-]{36})\.(left|right|scratch)\.json$`)

// readClaudeMappings: newest mapping per session and pane.
func readClaudeMappings(dir string) map[string]claudeMapping {
	out := map[string]claudeMapping{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range ents {
		m := reMapName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var cm claudeMapping
		if json.Unmarshal(raw, &cm) != nil || cm.TranscriptPath == "" || !filepath.IsAbs(cm.TranscriptPath) {
			continue
		}
		cm.session, cm.pane, cm.mtime = strings.ToUpper(m[1]), m[2], info.ModTime()
		key := cm.session + "." + cm.pane
		if old, ok := out[key]; !ok || cm.mtime.After(old.mtime) {
			out[key] = cm
		}
	}
	return out
}

func runsClaude(s Session) bool {
	for i, a := range s.Foreground {
		b := filepath.Base(a)
		if b == "claude" || (i > 0 && strings.Contains(a, "claude-code")) || (i == 0 && b == "claude.exe") {
			return true
		}
	}
	return false
}

var reNotAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// projectDir: Claude Code keeps a folder's transcripts in
// ~/.claude/projects/<cwd with every non-alphanumeric character as "-">.
func projectDir(projects, cwd string) string {
	return filepath.Join(projects, reNotAlnum.ReplaceAllString(cwd, "-"))
}

/* ---------- transcript entries ---------- */

type mirrorEvent struct {
	kind  string // prompt | bash | bashOut | text | tool | turnEnd | bgStart | bgEnd
	id    string // bgStart: tool_use id; bgEnd / tool (TaskStop): tool_use or task id
	task  string // bgStart: background task id
	durMs int64  // turnEnd: how long the turn took
	text  string // prompt / text / bash command / output
	err   string // bash stderr
	tool  string // tool line item
}

type transcriptEntry struct {
	Type                      string          `json:"type"`
	Subtype                   string          `json:"subtype"`
	DurationMs                int64           `json:"durationMs"`
	IsSidechain               bool            `json:"isSidechain"`
	IsMeta                    bool            `json:"isMeta"`
	IsCompactSummary          bool            `json:"isCompactSummary"`
	IsVisibleInTranscriptOnly bool            `json:"isVisibleInTranscriptOnly"`
	TurnOrigin                string          `json:"turnOrigin"`
	ToolUseResult             json.RawMessage `json:"toolUseResult"`
	Attachment                *struct {
		Type        string `json:"type"`
		Prompt      string `json:"prompt"`
		CommandMode string `json:"commandMode"`
		TaskID      string `json:"taskId"`
		Status      string `json:"status"`
	} `json:"attachment"`
	Operation string          `json:"operation"` // queue-operation
	Content   json.RawMessage `json:"content"`   // queue-operation
	Origin    *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

// bgLaunch: the task id when a tool result says the tool went on in the
// background (Bash/Agent with run_in_background, async agents, workflows).
func bgLaunch(raw json.RawMessage) string {
	var r struct {
		BackgroundTaskID string `json:"backgroundTaskId"`
		Status           string `json:"status"`
		IsAsync          bool   `json:"isAsync"`
		AgentID          string `json:"agentId"`
		TaskID           string `json:"taskId"`
		TimeoutMs        *int64 `json:"timeoutMs"`  // Monitor
		Persistent       bool   `json:"persistent"` // Monitor for the whole session: not "work"
	}
	if len(raw) == 0 || json.Unmarshal(raw, &r) != nil {
		return ""
	}
	if r.BackgroundTaskID != "" {
		return r.BackgroundTaskID
	}
	if r.TaskID != "" && r.TimeoutMs != nil && !r.Persistent {
		return r.TaskID // a Monitor watch: Claude will act on its events
	}
	if r.Status == "async_launched" || r.IsAsync {
		for _, id := range []string{r.AgentID, r.TaskID} {
			if id != "" {
				return id
			}
		}
		return "?"
	}
	return ""
}

// innerTag: the trimmed text of the first <tag>…</tag>, nested or not.
func innerTag(s, tag string) string {
	i := strings.Index(s, "<"+tag+">")
	if i < 0 {
		return ""
	}
	s = s[i+len(tag)+2:]
	j := strings.Index(s, "</"+tag+">")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(s[:j])
}

func runningStatus(s string) bool {
	switch strings.TrimSpace(s) {
	case "running", "in_progress", "progress", "pending":
		return true
	}
	return false
}

var reHandBack = regexp.MustCompile(`^\s*<agent-message from="([A-Za-z0-9_-]+)"[^>]*>\s*\[Subagent hand-back\]`)

// taskNotice: a background task reported back — a task notification with a
// final status, or a subagent's final report — so it no longer counts.
func taskNotice(s string) *mirrorEvent {
	if m := reHandBack.FindStringSubmatch(s); m != nil {
		return &mirrorEvent{kind: "bgEnd", id: m[1], task: m[1]}
	}
	// Without a status it is an event of a running task (Monitor), not its
	// end — unless it is the watch expiring.
	expired := strings.HasPrefix(innerTag(s, "event"), "[Monitor expired")
	if st := innerTag(s, "status"); !strings.Contains(s, "<task-notification>") || (!expired && (st == "" || runningStatus(st))) {
		return nil
	}
	task := innerTag(s, "task-id")
	id := innerTag(s, "tool-use-id")
	if id == "" {
		id = task
	}
	if id == "" {
		return nil
	}
	return &mirrorEvent{kind: "bgEnd", id: id, task: task}
}

// contentText: a user message's text, whether a string or text blocks.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

var (
	reTagged  = regexp.MustCompile(`(?s)<([a-z-]+)>(.*?)</([a-z-]+)>`)
	reLeadTag = regexp.MustCompile(`^\s*<([a-z-]+)>`)
	skipLeads = map[string]bool{"system-reminder": true, "task-notification": true, "local-command-stdout": true, "local-command-stderr": true, "local-command-caveat": true, "command-message": true, "user-prompt-submit-hook": true}
	bashLeads = map[string]bool{"bash-stdout": true, "bash-stderr": true}
)

func tagged(s, tag string) string {
	for _, m := range reTagged.FindAllStringSubmatch(s, -1) {
		if m[1] == tag && m[3] == tag {
			return m[2]
		}
	}
	return ""
}

// parseEntry turns one transcript line into what the mirror shows; nil for
// everything it leaves out.
func parseEntry(line []byte) *mirrorEvent {
	var e transcriptEntry
	if json.Unmarshal(line, &e) != nil || e.IsSidechain {
		return nil
	}
	switch e.Type {
	case "attachment": // a notice that came while Claude was busy
		a := e.Attachment
		switch {
		case a == nil:
		case a.Type == "queued_command":
			return taskNotice(a.Prompt)
		case a.Type == "task_status" && a.TaskID != "" && !runningStatus(a.Status):
			return &mirrorEvent{kind: "bgEnd", id: a.TaskID, task: a.TaskID}
		}
		return nil
	case "queue-operation": // notices are queued the moment a task ends
		if e.Operation == "enqueue" {
			return taskNotice(contentText(e.Content))
		}
		return nil
	case "system":
		if e.Subtype == "turn_duration" {
			return &mirrorEvent{kind: "turnEnd", durMs: e.DurationMs}
		}
		return nil
	case "user":
		if e.IsMeta || e.IsCompactSummary || e.IsVisibleInTranscriptOnly || e.Message == nil {
			return nil
		}
		if e.TurnOrigin == "task_notification" || e.TurnOrigin == "peer" || (e.Origin != nil && (e.Origin.Kind == "task-notification" || e.Origin.Kind == "peer")) {
			return taskNotice(contentText(e.Message.Content))
		}
		if (e.TurnOrigin != "" && e.TurnOrigin != "human") || (e.Origin != nil && e.Origin.Kind != "" && e.Origin.Kind != "human") {
			return nil // other agents, schedules
		}
		var s string
		if json.Unmarshal(e.Message.Content, &s) != nil {
			var blocks []contentBlock
			if json.Unmarshal(e.Message.Content, &blocks) != nil {
				return nil
			}
			var parts []string
			for _, b := range blocks {
				switch b.Type {
				case "tool_result":
					if t := bgLaunch(e.ToolUseResult); t != "" && b.ToolUseID != "" {
						return &mirrorEvent{kind: "bgStart", id: b.ToolUseID, task: t}
					}
					return nil
				case "text":
					parts = append(parts, b.Text)
				case "image":
					parts = append(parts, "[картинка]")
				}
			}
			s = strings.Join(parts, "\n")
		}
		lead := ""
		if m := reLeadTag.FindStringSubmatch(s); m != nil {
			lead = m[1]
		}
		switch {
		case lead == "bash-input":
			return &mirrorEvent{kind: "bash", text: tagged(s, "bash-input")}
		case bashLeads[lead]:
			return &mirrorEvent{kind: "bashOut", text: tagged(s, "bash-stdout"), err: tagged(s, "bash-stderr")}
		case tagged(s, "command-name") != "": // a slash command (often led by <command-message>)
			cmd := strings.TrimSpace(tagged(s, "command-name") + " " + strings.TrimSpace(tagged(s, "command-args")))
			return &mirrorEvent{kind: "prompt", text: cmd}
		case skipLeads[lead]:
			return nil
		}
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return &mirrorEvent{kind: "prompt", text: s}
	case "assistant":
		if e.Message == nil {
			return nil
		}
		var blocks []contentBlock
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			return nil
		}
		for _, b := range blocks { // one block per entry in practice
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					return &mirrorEvent{kind: "text", text: b.Text}
				}
			case "tool_use":
				ev := &mirrorEvent{kind: "tool", tool: toolItem(b.Name, b.Input)}
				if b.Name == "TaskStop" || b.Name == "KillShell" || b.Name == "KillBash" {
					var in struct {
						TaskID  string `json:"task_id"`
						ShellID string `json:"shell_id"`
					}
					json.Unmarshal(b.Input, &in)
					ev.id = in.TaskID + in.ShellID
				}
				return ev
			}
		}
	}
	return nil
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return truncRunes(s, n)
}

// toolItem: "Read auth.go", "Bash: go test ./…", "🤖 агент: review".
func toolItem(name string, raw json.RawMessage) string {
	var in map[string]any
	json.Unmarshal(raw, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch name {
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
		if p := str("file_path"); p != "" {
			return name + " " + filepath.Base(p)
		}
		if p := str("notebook_path"); p != "" {
			return name + " " + filepath.Base(p)
		}
	case "Bash":
		return "Bash: " + clip(str("command"), 40)
	case "Grep", "Glob":
		return name + " " + clip(str("pattern"), 30)
	case "WebFetch":
		if u, err := url.Parse(str("url")); err == nil && u.Host != "" {
			return "WebFetch " + u.Host
		}
	case "WebSearch":
		return "WebSearch " + clip(str("query"), 30)
	case "Agent", "Task":
		return "🤖 агент: " + clip(str("description"), 50)
	}
	if strings.HasPrefix(name, "mcp__") {
		parts := strings.Split(name, "__")
		return parts[len(parts)-1]
	}
	return name
}

/* ---------- per-transcript state ---------- */

// transcriptMirror follows one transcript for one owner (session + pane). It
// is saved with the state, together with the read offset, so what was read
// but not yet sent survives a restart.
type transcriptMirror struct {
	Path    string `json:"path"`
	Session string `json:"session"` // owner; buffered work always goes to it
	Pane    string `json:"pane"`
	Off     int64  `json:"off"` // bytes read and turned into buffers or queued messages
	Ino     uint64 `json:"ino,omitempty"`

	Text    []string  `json:"text,omitempty"` // assistant text waiting to be sent together
	TextAt  time.Time `json:"textAt,omitzero"`
	BashCmd string    `json:"bash,omitempty"`
	Turn    int       `json:"turn,omitempty"`
	Tools   []string  `json:"tools,omitempty"`
	ToolN   int       `json:"toolN,omitempty"`
	ToolKey string    `json:"toolKey,omitempty"` // queue key of this turn's "🔧" message
	Dirty   bool      `json:"dirty,omitempty"`
	EditAt  time.Time `json:"editAt,omitzero"`
	// Background tasks still running: tool_use id → launch.
	Bg map[string]bgTask `json:"bg,omitempty"`

	skipping bool // inside a line longer than mirrorReadMax
}

type bgTask struct {
	Task string    `json:"task,omitempty"`
	At   time.Time `json:"at"`
}

// bgEnd forgets the background task with this tool_use id or task id.
func (tm *transcriptMirror) bgEnd(id string) {
	if id == "" {
		return
	}
	for k, t := range tm.Bg {
		if k == id || t.Task == id {
			delete(tm.Bg, k)
		}
	}
}

// bgRunning: background tasks still running (stale ones are dropped).
func (tm *transcriptMirror) bgRunning(now time.Time) int {
	for k, t := range tm.Bg {
		if now.Sub(t.At) >= mirrorBgMax {
			delete(tm.Bg, k)
		}
	}
	return len(tm.Bg)
}

// mirrorOp is one queued Telegram call; the queue is saved with the state.
type mirrorOp struct {
	Key     string `json:"key,omitempty"` // edit: the message it edits; send: where its id is kept
	Edit    bool   `json:"edit,omitempty"`
	Final   bool   `json:"final,omitempty"` // the last edit of a tool line: its id can go
	Session string `json:"session"`
	Text    string `json:"text"`
	Tries   int    `json:"tries,omitempty"`
	Loud    bool   `json:"loud,omitempty"` // may ring (sound policy decides); everything else is silent
}

const (
	mirrorQMax      = 100       // queued messages before reading pauses
	mirrorQBytesMax = 512 << 10 // queued text before reading pauses
)

type sentText struct {
	norm string
	at   time.Time
}

func normText(s string) string { return strings.Join(strings.Fields(s), " ") }

func echoKey(sid, pane string) string { return strings.ToUpper(sid) + "." + paneOr(pane) }

// noteSent remembers text the bot typed into a session pane, so its prompt is
// not echoed back, and that the last prompt there came from Telegram.
func (b *tgBot) noteSent(sid, pane, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastSrc[strings.ToUpper(sid)] = "tg"
	key, now := echoKey(sid, pane), b.now()
	keep := b.sentTexts[key][:0]
	for _, t := range b.sentTexts[key] {
		if now.Sub(t.at) < mirrorEchoWindow {
			keep = append(keep, t)
		}
	}
	b.sentTexts[key] = append(keep, sentText{normText(text), now})
}

// fromTelegram: the prompt in this pane's transcript is one the bot typed into
// the same pane in the last few minutes (each match used once).
func (b *tgBot) fromTelegram(sid, pane, text string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	key, n, now := echoKey(sid, pane), normText(text), b.now()
	list := b.sentTexts[key]
	for i, t := range list {
		if now.Sub(t.at) < mirrorEchoWindow && (t.norm == n || "!"+n == t.norm) {
			b.sentTexts[key] = append(list[:i:i], list[i+1:]...)
			return true
		}
	}
	return false
}

/* ---------- sound ---------- */

type tgSettings struct {
	LoudAfterIdleMinutes *float64 `json:"loudAfterIdleMinutes"`
}

func (b *tgBot) loudAfter() time.Duration {
	raw, err := os.ReadFile(b.settingsPath)
	if err != nil {
		return defaultLoudAfter
	}
	var s tgSettings
	if json.Unmarshal(raw, &s) != nil || s.LoudAfterIdleMinutes == nil || *s.LoudAfterIdleMinutes < 0 {
		return defaultLoudAfter
	}
	return time.Duration(*s.LoudAfterIdleMinutes * float64(time.Minute))
}

// silentFor decides disable_notification for a new message about a session,
// at the moment it is sent: quiet while the user is at the Mac and the last
// prompt there was typed at the Mac; loud when the last prompt came from
// Telegram or the Mac has been idle for loudAfter. "🔕 Тишина" makes
// everything quiet.
func (b *tgBot) silentFor(ctx context.Context, sid string) bool {
	b.mu.Lock()
	muted := b.now().Before(b.st.MutedUntil)
	src := b.lastSrc[strings.ToUpper(sid)]
	b.mu.Unlock()
	if muted {
		return true
	}
	if src == "tg" {
		return false
	}
	idle, at := b.srv.ctl.Idle()
	if at.IsZero() || time.Since(at) > 10*time.Second {
		b.srv.ctl.Tree(ctx) // refreshes the idle time
		idle, at = b.srv.ctl.Idle()
	}
	if at.IsZero() {
		return false // cannot tell: better heard than missed
	}
	return idle < b.loudAfter()
}

/* ---------- the loop ---------- */

func (b *tgBot) mirrorLoop(ctx context.Context) {
	t := time.NewTicker(mirrorEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.safely("mirror", func() { b.mirrorTick(ctx) })
		}
	}
}

// loadMirror restores followers, queue and message ids saved with the state
// (and turns the offsets of the first mirror version into followers).
func (b *tgBot) loadMirror() {
	for p, tm := range b.st.Follow {
		cp := *tm
		cp.Path = p
		b.mirrors[p] = &cp
	}
	for p, off := range b.st.Mirror {
		if b.mirrors[p] == nil {
			b.mirrors[p] = &transcriptMirror{Path: p, Off: off}
		}
	}
	b.st.Mirror = nil
	for _, op := range b.st.MirrorQ {
		cp := *op
		b.mirrorQ = append(b.mirrorQ, &cp)
	}
	for k, v := range b.st.MirrorIDs {
		b.mirrorIDs[k] = v
	}
}

// persistMirror saves followers, queue and ids in one state write; mirrorMu held.
func (b *tgBot) persistMirror(gen uint64) {
	follow := make(map[string]*transcriptMirror, len(b.mirrors))
	for p, tm := range b.mirrors {
		cp := *tm
		cp.Text = append([]string(nil), tm.Text...)
		cp.Tools = append([]string(nil), tm.Tools...)
		follow[p] = &cp
	}
	q := make([]*mirrorOp, len(b.mirrorQ))
	for i, op := range b.mirrorQ {
		cp := *op
		q[i] = &cp
	}
	ids := make(map[string]int64, len(b.mirrorIDs))
	for k, v := range b.mirrorIDs {
		ids[k] = v
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gen != gen {
		return
	}
	b.st.Follow, b.st.MirrorQ, b.st.MirrorIDs = follow, q, ids
	b.saveLocked()
}

// mirrorTick: refresh which transcripts to follow, read what they gained,
// turn it into messages, send what is due.
func (b *tgBot) mirrorTick(ctx context.Context) {
	b.mirrorMu.Lock()
	defer b.mirrorMu.Unlock()
	b.mu.Lock()
	paired, gen := b.st.paired(), b.gen
	b.mu.Unlock()
	if !paired {
		return
	}
	if gen != b.mirrorGen { // re-paired: start over, nothing queued for the old chat
		b.mirrors, b.mirrorQ, b.mirrorIDs, b.mirrorGen = map[string]*transcriptMirror{}, nil, map[string]int64{}, gen
	}
	if b.now().Sub(b.mirrorMapped) >= mirrorMapEvery {
		b.refreshMirrors(ctx)
	}
	paths := make([]string, 0, len(b.mirrors))
	for p := range b.mirrors {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if !b.mirrorBacklogged() { // a long outage pauses reading instead of piling up
		for _, p := range paths {
			b.readTranscript(b.mirrors[p])
		}
	}
	now := b.now()
	for _, p := range paths {
		tm := b.mirrors[p]
		if len(tm.Text) > 0 && now.Sub(tm.TextAt) >= mirrorHoldMax {
			b.flushText(ctx, tm)
		}
		if tm.Dirty && tm.ToolKey != "" && now.Sub(tm.EditAt) >= mirrorEditGap {
			b.queueToolEdit(tm, false)
		}
	}
	b.persistMirror(gen)
	b.drainMirror(ctx, gen)
}

func (b *tgBot) mirrorBacklogged() bool {
	n := 0
	for _, op := range b.mirrorQ {
		n += len(op.Text)
	}
	return len(b.mirrorQ) >= mirrorQMax || n >= mirrorQBytesMax
}

// transcriptCwd: the working directory the transcript itself records (its
// entries carry "cwd"), "" if none is found near its end or start.
func transcriptCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	look := func(off, n int64) string {
		buf := make([]byte, n)
		k, _ := f.ReadAt(buf, off)
		cwd := ""
		for _, line := range bytes.Split(buf[:k], []byte{'\n'}) {
			var e struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &e) == nil && e.Cwd != "" {
				cwd = e.Cwd
			}
		}
		return cwd
	}
	const span = 256 << 10
	if c := look(max(0, st.Size()-span), min(span, st.Size())); c != "" {
		return c
	}
	return look(0, min(span, st.Size()))
}

// fallbackTranscript: for a Claude the hook does not know about, the one
// recent transcript of its project folder whose recorded cwd is exactly the
// session's ("" and ambiguous=true when more than one fits). The folder name
// alone is not enough: ~/a-b and ~/a_b share it.
func fallbackTranscript(projects, cwd string, now time.Time) (path string, ambiguous bool) {
	dir := projectDir(projects, cwd)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var found []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) > mirrorFallbackAge {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if transcriptCwd(p) == cwd {
			found = append(found, p)
		}
	}
	if len(found) == 1 {
		return found[0], false
	}
	return "", len(found) > 1
}

func fileIno(st os.FileInfo) uint64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Ino)
	}
	return 0
}

// refreshMirrors decides which transcripts to follow and for whom. A
// transcript claimed by two sessions is followed for neither. When a
// transcript changes owner, or stops being followed (/clear), what was
// buffered for the old owner is queued for the old owner first. A newly
// followed transcript starts at its end, except a brand-new conversation
// (hook source startup or clear), which starts at its beginning.
func (b *tgBot) refreshMirrors(ctx context.Context) {
	all, err := b.sessions(ctx)
	if err != nil {
		return
	}
	b.mirrorMapped = b.now()
	live := map[string]sessionRef{}
	for _, s := range all {
		live[s.ID] = s
	}
	claims := map[string][]claudeMapping{}
	hasMap := map[string]bool{}
	for _, m := range readClaudeMappings(b.claudeDir) {
		if _, ok := live[m.session]; ok {
			claims[m.TranscriptPath] = append(claims[m.TranscriptPath], m)
			hasMap[m.session] = true
		}
	}
	want := map[string]claudeMapping{}
	var unmirrored []string
	for p, ms := range claims {
		owners := map[string]bool{}
		for _, m := range ms {
			owners[m.session] = true
		}
		if len(owners) > 1 {
			var names []string
			for sid := range owners {
				names = append(names, sessionName(live[sid].Session))
			}
			sort.Strings(names)
			unmirrored = append(unmirrored, strings.Join(names, " и ")+" — один разговор")
			continue
		}
		want[p] = ms[0]
	}
	// Fallback for Claudes started before the hook.
	byCwd := map[string][]sessionRef{}
	for _, s := range all {
		if runsClaude(s.Session) && !hasMap[s.ID] && s.Cwd != "" {
			byCwd[s.Cwd] = append(byCwd[s.Cwd], s)
		}
	}
	for cwd, ss := range byCwd {
		if len(ss) > 1 {
			for _, s := range ss {
				unmirrored = append(unmirrored, sessionName(s.Session))
			}
			continue
		}
		// File times are wall-clock, so the age is judged against time.Now.
		p, ambiguous := fallbackTranscript(b.projectsDir, cwd, time.Now())
		if ambiguous {
			unmirrored = append(unmirrored, sessionName(ss[0].Session))
		}
		if _, taken := claims[p]; p != "" && !taken {
			want[p] = claudeMapping{TranscriptPath: p, Source: "fallback", session: ss[0].ID, pane: "left"}
		}
	}
	sort.Strings(unmirrored)

	// Retire followers that are no longer wanted or changed owner.
	carry := map[string]*transcriptMirror{}
	for p, tm := range b.mirrors {
		m, ok := want[p]
		if ok && (tm.Session == "" || (tm.Session == m.session && tm.Pane == m.pane)) {
			tm.Session, tm.Pane = m.session, m.pane // adopt (first version had no owner)
			continue
		}
		b.endTurn(ctx, tm) // buffered work goes to its own owner
		delete(b.mirrors, p)
		if ok {
			carry[p] = tm
		}
	}
	for p, m := range want {
		if b.mirrors[p] != nil {
			continue
		}
		tm := &transcriptMirror{Path: p, Session: m.session, Pane: m.pane}
		if old := carry[p]; old != nil { // new owner continues where the old one stopped
			tm.Off, tm.Ino = old.Off, old.Ino
		} else if st, err := os.Stat(p); err == nil {
			tm.Ino = fileIno(st)
			if m.Source != "startup" && m.Source != "clear" {
				tm.Off = st.Size()
			}
		}
		b.mirrors[p] = tm
	}
	mirrored := map[string]bool{}
	for _, tm := range b.mirrors {
		mirrored[tm.Session] = true
	}
	b.mu.Lock()
	b.mirrored, b.unmirrored = mirrored, unmirrored
	b.mu.Unlock()
}

// readTranscript reads what the transcript gained since its offset (complete
// lines only, at most mirrorReadMax bytes) and processes it. A file replaced
// (another inode) or truncated is a new generation and is read from its start.
func (b *tgBot) readTranscript(tm *transcriptMirror) {
	f, err := os.Open(tm.Path)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	// Offsets always sit right after a newline; if that byte is not one any
	// more, the file was rewritten in place (even to a larger size).
	rewritten := false
	if tm.Off > 0 && tm.Off <= st.Size() {
		one := make([]byte, 1)
		if _, err := f.ReadAt(one, tm.Off-1); err == nil && one[0] != '\n' {
			rewritten = true
		}
	}
	if ino := fileIno(st); ino != 0 && tm.Ino != 0 && ino != tm.Ino || st.Size() < tm.Off || rewritten {
		tm.Off, tm.skipping = 0, false
	}
	tm.Ino = fileIno(st)
	if st.Size() == tm.Off {
		return
	}
	buf := make([]byte, min(int64(mirrorReadMax), st.Size()-tm.Off))
	n, err := f.ReadAt(buf, tm.Off)
	if err != nil && err != io.EOF {
		return
	}
	buf = buf[:n]
	last := bytes.LastIndexByte(buf, '\n')
	if last < 0 {
		if len(buf) == mirrorReadMax { // one huge line: skip it
			tm.skipping = true
			tm.Off += int64(len(buf))
		}
		return
	}
	// Lines are consumed one by one, so reading can stop in the middle when
	// the queue is full (a long outage) and go on later from there.
	for pos := 0; pos <= last; {
		end := pos + bytes.IndexByte(buf[pos:], '\n')
		line := buf[pos:end]
		pos = end + 1
		if tm.skipping {
			tm.skipping = false
		} else if ev := parseEntry(line); ev != nil {
			b.mirrorEvent(tm, ev)
		}
		tm.Off += int64(len(line)) + 1
		if b.mirrorBacklogged() {
			return
		}
	}
}

func (b *tgBot) mirrorEvent(tm *transcriptMirror, ev *mirrorEvent) {
	ctx := b.ctx
	switch ev.kind {
	case "prompt":
		b.endTurn(ctx, tm) // a turn that did not end (Esc): no ✅
		if b.fromTelegram(tm.Session, tm.Pane, ev.text) {
			return // typed from Telegram: it is already in the chat
		}
		b.mu.Lock()
		b.lastSrc[tm.Session] = "mac"
		b.mu.Unlock()
		b.queueSend(ctx, tm, "👤 "+escapedHead(strings.TrimSpace(ev.text), mirrorPromptMax))
	case "bash":
		b.flushText(ctx, tm)
		if b.fromTelegram(tm.Session, tm.Pane, "!"+ev.text) || b.fromTelegram(tm.Session, tm.Pane, ev.text) {
			tm.BashCmd = "\x00tg"
			return
		}
		b.mu.Lock()
		b.lastSrc[tm.Session] = "mac"
		b.mu.Unlock()
		tm.BashCmd = ev.text
	case "bashOut":
		cmd := tm.BashCmd
		tm.BashCmd = ""
		out := strings.TrimRight(ev.text, "\n")
		if e := strings.TrimRight(ev.err, "\n"); e != "" {
			out = strings.TrimSpace(out + "\n" + e)
		}
		msg := "💻"
		if cmd != "" && cmd != "\x00tg" {
			msg += " <code>" + escapedHead(clip(cmd, 200), 1000) + "</code>"
		}
		if out != "" && strings.Count(out, "\n") >= mirrorBashFold {
			// Long output folds away (no <pre> allowed inside a quote).
			msg += "\n<blockquote expandable>" + escapedTail(out, mirrorBashOutMax) + "</blockquote>"
		} else if out != "" {
			msg += "\n<pre>" + escapedTail(out, mirrorBashOutMax) + "</pre>"
		}
		b.queueSend(ctx, tm, msg)
	case "text":
		tm.Text = append(tm.Text, ev.text)
		tm.TextAt = b.now()
	case "bgStart":
		if tm.Bg == nil {
			tm.Bg = map[string]bgTask{}
		}
		tm.Bg[ev.id] = bgTask{Task: ev.task, At: b.now()}
	case "bgEnd":
		tm.bgEnd(ev.id)
		tm.bgEnd(ev.task)
	case "tool":
		tm.bgEnd(ev.id) // TaskStop
		b.flushText(ctx, tm)
		tm.Tools = append(tm.Tools, ev.tool)
		if len(tm.Tools) > mirrorToolItems { // only the last ones are shown
			tm.Tools = tm.Tools[len(tm.Tools)-mirrorToolItems:]
		}
		tm.ToolN++
		if tm.ToolKey == "" {
			tm.Turn++
			tm.ToolKey = fmt.Sprintf("tool|%s|%d", tm.Path, tm.Turn)
			tm.EditAt, tm.Dirty = b.now(), false
			b.queueSendKey(ctx, tm, tm.ToolKey, toolLine(tm, false))
			return
		}
		tm.Dirty = true
	case "turnEnd":
		b.finishTurn(ctx, tm, ev.durMs)
	}
}

// finishTurn: the turn ended. The tool line gets its total first, then the
// last text goes out as the topic's last message (so the topic list reads as
// status), headed "🏁 Полностью готово · <time> · N действий" when nothing is
// left running — the only mirror message that may ring — or, silently,
// "✅ Ответ готов · в фоне ещё N" while background tasks will wake Claude again.
func (b *tgBot) finishTurn(ctx context.Context, tm *transcriptMirror, durMs int64) {
	n := tm.ToolN
	if tm.ToolKey != "" {
		b.queueToolEdit(tm, true)
	}
	bg := tm.bgRunning(b.now())
	head := "🏁 Полностью готово · " + fmtDur(durMs)
	if bg > 0 {
		head = fmt.Sprintf("✅ Ответ готов · в фоне ещё %d · %s", bg, fmtDur(durMs))
	}
	if n > 0 {
		head += fmt.Sprintf(" · %d %s", n, plural(n, "действие", "действия", "действий"))
	}
	loud := bg == 0
	prefix := b.mirrorPrefix(tm)
	if len(tm.Text) == 0 {
		b.queueOp(tm, "", prefix+head, loud)
	} else {
		msgs := mdToMessages(strings.Join(tm.Text, "\n\n"), prefix)
		tm.Text = nil
		for i, m := range msgs {
			if i < len(msgs)-1 {
				b.queueOp(tm, "", m, false)
				continue
			}
			b.queueOp(tm, "", prefix+head+"\n"+strings.TrimPrefix(m, prefix), loud)
		}
	}
	tm.Tools, tm.ToolN, tm.ToolKey, tm.Dirty = nil, 0, "", false
}

// fmtDur: "14 с", "2 мин 14 с", "1 ч 5 мин".
func fmtDur(ms int64) string {
	sec := (ms + 500) / 1000
	switch {
	case sec < 60:
		return fmt.Sprintf("%d с", sec)
	case sec < 3600:
		if sec%60 == 0 {
			return fmt.Sprintf("%d мин", sec/60)
		}
		return fmt.Sprintf("%d мин %d с", sec/60, sec%60)
	}
	return fmt.Sprintf("%d ч %d мин", sec/3600, sec%3600/60)
}

// escapedHead: the start of s, escaped, at most n characters, never cut
// inside an entity.
func escapedHead(s string, n int) string {
	parts := escapedChunks(s, n-1)
	if len(parts) > 1 {
		return parts[0] + "…"
	}
	return parts[0]
}

// escapedTail: the end of s, escaped, at most n characters.
func escapedTail(s string, n int) string {
	parts := escapedChunks(s, n-1)
	if len(parts) > 1 {
		return "…" + parts[len(parts)-1]
	}
	return parts[0]
}

// endTurn closes a turn that did not end normally (a new prompt, Esc, the
// follower retired): held text goes out as intermediate, the tool line gets
// its total, no ✅.
func (b *tgBot) endTurn(ctx context.Context, tm *transcriptMirror) {
	b.flushText(ctx, tm)
	if tm.ToolKey != "" {
		b.queueToolEdit(tm, true)
	}
	tm.Tools, tm.ToolN, tm.ToolKey, tm.Dirty = nil, 0, "", false
}

func toolLine(tm *transcriptMirror, final bool) string {
	items := tm.Tools
	esc := make([]string, len(items))
	for i, it := range items {
		esc[i] = html.EscapeString(stripControl(it))
	}
	line := strings.Join(esc, " · ")
	if final {
		return fmt.Sprintf("🔧 %d %s: %s", tm.ToolN, plural(tm.ToolN, "действие", "действия", "действий"), line)
	}
	if more := tm.ToolN - len(items); more > 0 {
		return fmt.Sprintf("⏳ Работает · %s (+%d)", line, more)
	}
	return "⏳ Работает · " + line
}

func (b *tgBot) flushText(ctx context.Context, tm *transcriptMirror) {
	if len(tm.Text) == 0 {
		return
	}
	// Text followed by a tool, a new prompt or nothing for long is
	// intermediate: "🤖", silent.
	md := strings.Join(tm.Text, "\n\n")
	tm.Text = nil
	prefix := b.mirrorPrefix(tm)
	for i, msg := range mdToMessages(md, prefix) {
		if i == 0 {
			msg = prefix + "🤖 " + strings.TrimPrefix(msg, prefix)
		}
		b.queueRaw(tm, "", msg)
	}
}

func (b *tgBot) mirrorPrefix(tm *transcriptMirror) string {
	b.mu.Lock()
	forum := b.st.Mode == "forum"
	b.mu.Unlock()
	if forum {
		return ""
	}
	name := tm.Session
	if s, _ := b.findSession(b.ctx, tm.Session); s != nil {
		name = sessionName(s.Session)
	}
	return "<b>" + html.EscapeString(truncRunes(name, 100)) + "</b>\n"
}

func (b *tgBot) queueSend(ctx context.Context, tm *transcriptMirror, body string) {
	b.queueRaw(tm, "", b.mirrorPrefix(tm)+body)
}

func (b *tgBot) queueSendKey(ctx context.Context, tm *transcriptMirror, key, body string) {
	b.queueRaw(tm, key, b.mirrorPrefix(tm)+body)
}

func (b *tgBot) queueRaw(tm *transcriptMirror, key, text string) { b.queueOp(tm, key, text, false) }

func (b *tgBot) queueOp(tm *transcriptMirror, key, text string, loud bool) {
	if utf8.RuneCountInString(text) > 4096 { // the builders keep under it; never cut HTML here
		log.Printf("telegram: mirror message of %d characters dropped", utf8.RuneCountInString(text))
		return
	}
	b.mirrorQ = append(b.mirrorQ, &mirrorOp{Key: key, Session: tm.Session, Text: text, Loud: loud})
}

// queueToolEdit puts the turn's "🔧" message update in the queue, replacing an
// edit of the same message that has not gone out yet.
func (b *tgBot) queueToolEdit(tm *transcriptMirror, final bool) {
	text := b.mirrorPrefix(tm) + toolLine(tm, final)
	tm.Dirty, tm.EditAt = false, b.now()
	for _, op := range b.mirrorQ {
		if op.Edit && op.Key == tm.ToolKey {
			op.Text, op.Final = text, op.Final || final
			return
		}
	}
	b.mirrorQ = append(b.mirrorQ, &mirrorOp{Key: tm.ToolKey, Edit: true, Final: final, Session: tm.Session, Text: text})
}

// drainMirror sends the queue in order on the bulk lane, saving after each
// message. A transient failure (or a topic deleted by hand, which is made
// again) stops the drain until the next tick; an op that keeps failing or
// fails for good is dropped.
func (b *tgBot) drainMirror(ctx context.Context, gen uint64) {
	for len(b.mirrorQ) > 0 {
		op := b.mirrorQ[0]
		err := b.sendMirrorOp(ctx, gen, op)
		retry := err != nil && (transient(err) || topicGone(err))
		if retry && op.Tries < mirrorOpTries {
			op.Tries++
			b.persistMirror(gen)
			return
		}
		if err != nil && !notModified(err) {
			log.Printf("%v (mirror message dropped)", err)
		}
		if op.Edit && op.Final {
			delete(b.mirrorIDs, op.Key) // the line is finished; its id is no longer needed
		}
		b.mirrorQ = b.mirrorQ[1:]
		b.persistMirror(gen)
	}
}

func (b *tgBot) sendMirrorOp(ctx context.Context, gen uint64, op *mirrorOp) error {
	b.mu.Lock()
	chat, forum, cur := b.st.ChatID, b.st.Mode == "forum", b.gen
	b.mu.Unlock()
	if cur != gen {
		return errStaleGen
	}
	if op.Edit {
		id := b.mirrorIDs[op.Key]
		if id == 0 {
			return nil // its message never went out
		}
		return b.out.callBulk(ctx, chat, "editMessageText", map[string]any{
			"chat_id": chat, "message_id": id, "text": op.Text, "parse_mode": "HTML",
			"link_preview_options": map[string]bool{"is_disabled": true},
		}, nil)
	}
	p := map[string]any{"chat_id": chat, "text": op.Text, "parse_mode": "HTML",
		"disable_notification": !op.Loud || b.silentFor(ctx, op.Session), // decided now, not when queued
		"link_preview_options": map[string]bool{"is_disabled": true}}
	if forum {
		s, err := b.findSession(ctx, op.Session)
		if err != nil {
			return err
		}
		if s == nil {
			return nil // the session closed: nowhere to post
		}
		thread, err := b.ensureTopic(ctx, gen, s)
		if err != nil {
			return err
		}
		p["message_thread_id"] = thread
	}
	var out tgMessage
	err := b.out.callBulk(ctx, chat, "sendMessage", p, &out)
	if err != nil && forum && topicGone(err) {
		b.forgetTopic(gen, op.Session) // deleted by hand: the retry makes a new one
		return err
	}
	if err != nil {
		return err
	}
	if op.Key != "" {
		b.mirrorIDs[op.Key] = out.MessageID
	}
	if !forum {
		b.mu.Lock()
		if b.gen == gen {
			b.st.addMsgRef(out.MessageID, op.Session, "")
		}
		b.mu.Unlock()
	}
	return nil
}

func (b *tgBot) isMirrored(sid string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mirrored[strings.ToUpper(sid)]
}
