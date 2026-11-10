package main

// Usage limits of the Claude subscription accounts (through cswap) and of Codex
// (from the rate_limits events Codex writes into its session rollouts).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	limitsTTL     = 60 * time.Second // a good result is served this long
	limitsErrTTL  = 20 * time.Second // a failed cswap call is retried sooner
	cswapTimeout  = 25 * time.Second
	staleLastGood = 3 * time.Hour
	// cswap serves its stored measurement for up to ~3 min and lets an idle
	// alternate account drift to ~10 min; older than this is worth flagging
	// (a 429 backoff can keep serving an hour-old "ok" measurement).
	staleUsage = 15 * time.Minute

	codexMaxAge     = 31 * 24 * time.Hour // session folders older than this are not looked at
	codexMaxOpen    = 40                  // rollout files opened per collection at most
	codexChunk      = 256 << 10           // backward read step
	codexMaxScan    = 32 << 20            // bytes read from the end of one file at most
	codexMaxLineLen = 1 << 20             // longer lines (tool output) are skipped unparsed
)

// Window is one usage window: percent used and when it resets.
type Window struct {
	Label    string  `json:"label"` // "5 часов", "Неделя", "Неделя (Opus)"
	Pct      float64 `json:"pct"`
	ResetsAt string  `json:"resetsAt,omitempty"` // RFC 3339
	// Expired: the reset time has passed since the data was measured, so the
	// window restarted and Pct is reported as 0.
	Expired bool `json:"expired,omitempty"`
}

type NamedWindow struct {
	Name string `json:"name"`
	Window
}

type ClaudeAccount struct {
	Number   int           `json:"number"`
	Label    string        `json:"label"`
	Active   bool          `json:"active"`
	Disabled bool          `json:"disabled,omitempty"`
	Status   string        `json:"status"` // cswap usageStatus: ok, relogin_required, token_expired, ...
	FiveHour *Window       `json:"fiveHour"`
	SevenDay *Window       `json:"sevenDay"`
	Scoped   []NamedWindow `json:"scoped,omitempty"` // per-model weekly limits
	AsOf     string        `json:"asOf,omitempty"`   // when the numbers were measured
	AgeSec   *int64        `json:"ageSeconds,omitempty"`
	Stale    bool          `json:"stale,omitempty"` // the numbers are old (last-good or long-unrefreshed)
	Error    string        `json:"error,omitempty"`
}

type CodexWindow struct {
	Name          string  `json:"name"`  // primary, secondary
	Label         string  `json:"label"` // "5 часов", "Неделя"
	Pct           float64 `json:"pct"`
	ResetsAt      string  `json:"resetsAt,omitempty"`
	WindowMinutes int     `json:"windowMinutes"`
	Expired       bool    `json:"expired,omitempty"`
}

type CodexLimits struct {
	Plan    string        `json:"plan,omitempty"`
	Windows []CodexWindow `json:"windows"`
	AsOf    string        `json:"asOf"` // time of the Codex turn the numbers come from
}

type Limits struct {
	Claude      []ClaudeAccount `json:"claude"`
	ClaudeError string          `json:"claudeError,omitempty"`
	Codex       *CodexLimits    `json:"codex"`
	Bot         *BotLine        `json:"bot,omitempty"` // Telegram bot health, when the bot is set up
	FetchedAt   string          `json:"fetchedAt"`
}

// LimitsSource collects limits and caches the result, so a page polling every
// minute (or several open pages) never runs cswap more than once per TTL.
type LimitsSource struct {
	CswapPath string
	CodexHome string
	// The Telegram bot's token file and the health file the service writes;
	// empty BotTokenFile leaves the bot line out.
	BotTokenFile, BotHealthFile string
	Timeout                     time.Duration
	Now                         func() time.Time

	mu      sync.Mutex
	cached  *Limits
	expires time.Time
}

func NewLimitsSource(cswapPath, codexHome string) *LimitsSource {
	return &LimitsSource{CswapPath: cswapPath, CodexHome: codexHome, Timeout: cswapTimeout, Now: time.Now}
}

// Get returns the cached result while it is fresh; otherwise it collects anew.
// Concurrent callers wait for the one collection instead of starting their own.
func (l *LimitsSource) Get(ctx context.Context) *Limits {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	if l.cached != nil && now.Before(l.expires) {
		return l.cached
	}
	res := l.Collect(ctx)
	l.cached = res
	ttl := limitsTTL
	if res.ClaudeError != "" {
		ttl = limitsErrTTL
	}
	l.expires = l.Now().Add(ttl)
	return res
}

// Collect gathers limits without the cache.
func (l *LimitsSource) Collect(ctx context.Context) *Limits {
	now := l.Now()
	res := &Limits{Claude: []ClaudeAccount{}, FetchedAt: now.UTC().Format(time.RFC3339)}
	// A request that went away must not leave a half-run cswap behind, but the
	// result is shared through the cache, so it gets its own deadline.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.Timeout)
	defer cancel()
	accts, err := l.claude(cctx, now)
	if err != nil {
		res.ClaudeError = err.Error()
	} else {
		res.Claude = accts
	}
	res.Codex = codexLimits(l.CodexHome, now)
	if l.BotTokenFile != "" {
		res.Bot = botLine(l.BotTokenFile, l.BotHealthFile, now)
	}
	return res
}

/* ---------- Claude (cswap) ---------- */

type cswapWindow struct {
	Pct      float64 `json:"pct"`
	ResetsAt string  `json:"resetsAt"`
	Name     string  `json:"name"`
}

type cswapUsage struct {
	FiveHour *cswapWindow  `json:"fiveHour"`
	SevenDay *cswapWindow  `json:"sevenDay"`
	Scoped   []cswapWindow `json:"scoped"`
}

type cswapAccount struct {
	Number             int         `json:"number"`
	Email              string      `json:"email"`
	Alias              string      `json:"alias"`
	OrganizationName   string      `json:"organizationName"`
	Active             bool        `json:"active"`
	Disabled           bool        `json:"disabled"`
	UsageStatus        string      `json:"usageStatus"`
	Usage              *cswapUsage `json:"usage"`
	UsageFetchedAt     string      `json:"usageFetchedAt"`
	UsageAgeSeconds    *float64    `json:"usageAgeSeconds"`
	LastGoodUsage      *cswapUsage `json:"lastGoodUsage"`
	LastGoodFetchedAt  string      `json:"lastGoodFetchedAt"`
	LastGoodAgeSeconds *float64    `json:"lastGoodAgeSeconds"`
}

type cswapList struct {
	Accounts []cswapAccount `json:"accounts"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
}

var statusText = map[string]string{
	"relogin_required":     "нужен вход (cswap add)",
	"token_expired":        "токен истёк, обновится при следующем запуске",
	"api_key":              "API-ключ, лимитов подписки нет",
	"keychain_unavailable": "Keychain недоступен",
	"foreign_credential":   "чужие учётные данные в слоте",
	"no_credentials":       "нет учётных данных",
	"unavailable":          "не удалось получить данные",
}

func (l *LimitsSource) claude(ctx context.Context, now time.Time) ([]ClaudeAccount, error) {
	// cswap keeps its own usage store and serves it while it is fresh (a few
	// minutes), so `list` does not call the usage endpoint on every run.
	cmd := exec.CommandContext(ctx, l.CswapPath, "list", "--json")
	cmd.Env = withPath(os.Environ())
	cmd.WaitDelay = 2 * time.Second // a killed cswap's children must not hold the pipes open
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, errors.New("cswap не ответил вовремя")
	}
	var list cswapList
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		if runErr != nil {
			var ee *exec.ExitError
			if !errors.As(runErr, &ee) {
				return nil, errors.New("cswap не найден")
			}
			return nil, fmt.Errorf("cswap: %s", firstLine(errb.String(), runErr.Error()))
		}
		return nil, errors.New("cswap: непонятный ответ")
	}
	if list.Error != nil {
		return nil, fmt.Errorf("cswap: %s", maskEmails(list.Error.Message))
	}
	if runErr != nil {
		return nil, fmt.Errorf("cswap: %s", firstLine(errb.String(), runErr.Error()))
	}

	out2 := make([]ClaudeAccount, 0, len(list.Accounts))
	for _, a := range list.Accounts {
		c := ClaudeAccount{
			Number:   a.Number,
			Label:    accountLabel(a),
			Active:   a.Active,
			Disabled: a.Disabled,
			Status:   a.UsageStatus,
		}
		switch {
		case a.Usage != nil:
			c.FiveHour = window(a.Usage.FiveHour, labelFiveHour, now)
			c.SevenDay = window(a.Usage.SevenDay, labelWeek, now)
			c.Scoped = scoped(a.Usage.Scoped, now)
			c.AsOf = a.UsageFetchedAt
			c.AgeSec = measurementAge(a.UsageFetchedAt, a.UsageAgeSeconds, now)
			// cswap can keep serving an old measurement as "ok" (429 backoff).
			c.Stale = c.AgeSec != nil && time.Duration(*c.AgeSec)*time.Second > staleUsage
			if c.Status == "" {
				c.Status = "ok"
			}
		default:
			c.Error = statusText[a.UsageStatus]
			if c.Error == "" {
				c.Error = "нет данных"
			}
			// Recent last-good numbers are still worth showing, marked as such.
			// A dead token's are not: they stopped updating long ago.
			if a.LastGoodUsage != nil && a.UsageStatus != "relogin_required" &&
				a.LastGoodAgeSeconds != nil && time.Duration(*a.LastGoodAgeSeconds*float64(time.Second)) < staleLastGood {
				c.FiveHour = window(a.LastGoodUsage.FiveHour, labelFiveHour, now)
				c.SevenDay = window(a.LastGoodUsage.SevenDay, labelWeek, now)
				c.Scoped = scoped(a.LastGoodUsage.Scoped, now)
				c.AsOf = a.LastGoodFetchedAt
				c.AgeSec = measurementAge(a.LastGoodFetchedAt, a.LastGoodAgeSeconds, now)
				c.Stale = true
			}
		}
		out2 = append(out2, c)
	}
	dedupeLabels(out2, list.Accounts)
	sort.SliceStable(out2, func(i, j int) bool { return out2[i].Active && !out2[j].Active })
	return out2, nil
}

// measurementAge prefers the fetch time (so it keeps counting while our cache
// serves the result) and falls back to the age cswap reported.
func measurementAge(fetchedAt string, ageSec *float64, now time.Time) *int64 {
	var age int64
	if t, err := time.Parse(time.RFC3339Nano, fetchedAt); err == nil {
		age = int64(now.Sub(t) / time.Second)
	} else if ageSec != nil {
		age = int64(*ageSec)
	} else {
		return nil
	}
	if age < 0 {
		age = 0
	}
	return &age
}

const (
	labelFiveHour = "5 часов"
	labelWeek     = "Неделя"
)

func window(w *cswapWindow, label string, now time.Time) *Window {
	if w == nil {
		return nil
	}
	o := &Window{Label: label, Pct: w.Pct}
	if t, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
		o.ResetsAt = t.UTC().Format(time.RFC3339)
		if !t.After(now) {
			o.Pct, o.Expired = 0, true
		}
	}
	return o
}

func scoped(ws []cswapWindow, now time.Time) []NamedWindow {
	var out []NamedWindow
	for i := range ws {
		label := labelWeek
		if n := strings.TrimSpace(ws[i].Name); n != "" {
			label += " (" + n + ")"
		}
		if w := window(&ws[i], label, now); w != nil {
			out = append(out, NamedWindow{Name: ws[i].Name, Window: *w})
		}
	}
	return out
}

// accountLabel is a short readable name that never exposes a full email: an
// alias, then a real organization name ("Globex Audio Inc" -> "Globex Audio"), then
// the email's domain without its TLD ("acmemedia"), and for public mail
// services the email with its local part masked.
func accountLabel(a cswapAccount) string {
	if s := strings.TrimSpace(a.Alias); s != "" {
		return s
	}
	if o := strings.TrimSpace(a.OrganizationName); o != "" && !strings.Contains(o, "@") && !strings.HasSuffix(o, "'s Organization") {
		return trimCompanySuffix(o)
	}
	if d := domainName(a.Email); d != "" {
		return d
	}
	if a.Email != "" {
		return maskEmail(a.Email)
	}
	return fmt.Sprintf("Аккаунт %d", a.Number)
}

var companySuffixes = []string{", Inc.", ", Inc", " Inc.", " Inc", " LLC", " Ltd.", " Ltd", " GmbH", " Corp.", " Corp", " Corporation"}

func trimCompanySuffix(o string) string {
	for _, suf := range companySuffixes {
		if t, ok := strings.CutSuffix(o, suf); ok && strings.TrimSpace(t) != "" {
			return strings.TrimSpace(t)
		}
	}
	return o
}

var publicMail = map[string]bool{
	"gmail": true, "googlemail": true, "yahoo": true, "outlook": true, "hotmail": true, "live": true,
	"icloud": true, "me": true, "mac": true, "mail": true, "yandex": true, "ya": true, "proton": true,
	"protonmail": true, "gmx": true, "aol": true, "fastmail": true, "rambler": true, "bk": true, "inbox": true, "list": true,
}

// domainName: "user@mail.acmemedia.ai" -> "acmemedia"; "" for public
// mail services, where the domain says nothing about the account.
func domainName(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return ""
	}
	parts := strings.Split(strings.ToLower(strings.Trim(email[at+1:], ".")), ".")
	if len(parts) < 2 {
		return ""
	}
	parts = parts[:len(parts)-1] // TLD
	if len(parts) > 1 {
		switch parts[len(parts)-1] { // second-level public suffix: example.co.uk
		case "co", "com", "org", "net", "ac", "gov", "edu":
			parts = parts[:len(parts)-1]
		}
	}
	name := parts[len(parts)-1]
	if name == "" || publicMail[name] {
		return ""
	}
	return name
}

// dedupeLabels adds the masked local part where two accounts share a label
// (say, two seats in one organization).
func dedupeLabels(accts []ClaudeAccount, raw []cswapAccount) {
	seen := map[string]int{}
	for _, a := range accts {
		seen[a.Label]++
	}
	for i := range accts {
		if seen[accts[i].Label] > 1 && raw[i].Email != "" {
			m := maskEmail(raw[i].Email)
			accts[i].Label += " (" + m[:strings.IndexByte(m, '@')] + ")"
		}
	}
	// Masked local parts can coincide too ("ni***"): the slot number settles it.
	seen = map[string]int{}
	for _, a := range accts {
		seen[a.Label]++
	}
	for i := range accts {
		if seen[accts[i].Label] > 1 {
			accts[i].Label += fmt.Sprintf(" #%d", accts[i].Number)
		}
	}
}

func maskEmail(e string) string {
	at := strings.LastIndex(e, "@")
	if at < 0 {
		return "***"
	}
	local, domain := e[:at], e[at+1:]
	keep := 2
	if len([]rune(local)) <= 2 {
		keep = 1
	}
	r := []rune(local)
	if len(r) < keep {
		keep = len(r)
	}
	return string(r[:keep]) + "***@" + domain
}

// maskEmails masks every email-looking word of a free-form message.
func maskEmails(s string) string {
	f := strings.Fields(s)
	for i, w := range f {
		if strings.Contains(w, "@") {
			f[i] = maskEmail(strings.Trim(w, "'\"()<>,.;:"))
		}
	}
	return strings.Join(f, " ")
}

func firstLine(s, def string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return maskEmails(s)
}

// withPath makes sure tools cswap shells out to are found when the server runs
// under launchd, whose PATH is minimal.
func withPath(env []string) []string {
	home, _ := os.UserHomeDir()
	extra := []string{filepath.Join(home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	out := make([]string, 0, len(env)+1)
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
			continue
		}
		out = append(out, kv)
	}
	parts := filepath.SplitList(path)
	for _, p := range extra {
		if !contains(parts, p) {
			parts = append(parts, p)
		}
	}
	return append(out, "PATH="+strings.Join(parts, string(os.PathListSeparator)))
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func defaultCswapPath() string {
	home, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(home, ".local", "bin", "cswap"), "/opt/homebrew/bin/cswap", "/usr/local/bin/cswap"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("cswap"); err == nil {
		return p
	}
	return "cswap"
}

func defaultCodexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

/* ---------- Codex ---------- */

type codexRateWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

type codexEvent struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type       string `json:"type"`
		RateLimits *struct {
			Primary   *codexRateWindow `json:"primary"`
			Secondary *codexRateWindow `json:"secondary"`
			PlanType  string           `json:"plan_type"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

type rolloutFile struct {
	path string
	mod  time.Time
}

// recentRollouts lists the rollout files of the session folders of the last
// month, newest modification first. A session keeps writing to the folder of
// the day it started, so folders are chosen by age, files by mtime.
func recentRollouts(codexHome string, now time.Time) []rolloutFile {
	root := filepath.Join(codexHome, "sessions")
	cutoff := now.Add(-codexMaxAge).Format("2006/01/02")
	var files []rolloutFile
	for _, y := range subdirsDesc(root) {
		for _, m := range subdirsDesc(filepath.Join(root, y)) {
			for _, d := range subdirsDesc(filepath.Join(root, y, m)) {
				if y+"/"+m+"/"+d < cutoff {
					break // the rest of this month is older; older months stop at their first day
				}
				dir := filepath.Join(root, y, m, d)
				ents, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				for _, e := range ents {
					n := e.Name()
					if e.IsDir() || !strings.HasPrefix(n, "rollout-") || !strings.HasSuffix(n, ".jsonl") {
						continue
					}
					if info, err := e.Info(); err == nil {
						files = append(files, rolloutFile{filepath.Join(dir, n), info.ModTime()})
					}
				}
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	return files
}

func subdirsDesc(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// lastRateLimits returns the last event with rate limits in a file. It reads
// backwards in chunks, at most codexMaxScan bytes, and skips over lines longer
// than codexMaxLineLen (big tool outputs) without keeping them in memory.
func lastRateLimits(path string) (*codexEvent, time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, time.Time{}
	}
	end := st.Size()
	var tail []byte   // start of a line whose beginning lies in an earlier chunk
	skipping := false // inside an over-long line: drop bytes until its start
	for read := int64(0); end > 0 && read < codexMaxScan; {
		n := min(int64(codexChunk), end)
		start := end - n
		buf := make([]byte, n, n+int64(len(tail)))
		if _, err := f.ReadAt(buf, start); err != nil {
			return nil, time.Time{}
		}
		read += n
		end = start
		nl := bytes.IndexByte(buf, '\n')
		if skipping {
			if nl < 0 {
				continue
			}
			// The over-long line starts after the last newline of this chunk.
			buf = buf[:bytes.LastIndexByte(buf, '\n')+1]
			skipping = false
			nl = bytes.IndexByte(buf, '\n')
		} else {
			buf = append(buf, tail...)
		}
		if nl < 0 && start > 0 {
			tail = buf
		} else {
			lines := bytes.Split(buf, []byte{'\n'})
			first := 0
			if start > 0 {
				first = 1 // lines[0] continues in the chunk before
				tail = lines[0]
			} else {
				tail = nil
			}
			for i := len(lines) - 1; i >= first; i-- {
				if ev, t := parseRateLimits(lines[i]); ev != nil {
					return ev, t
				}
			}
		}
		if len(tail) > codexMaxLineLen {
			tail, skipping = nil, true
		}
	}
	return nil, time.Time{}
}

func parseRateLimits(line []byte) (*codexEvent, time.Time) {
	if len(line) > codexMaxLineLen || !bytes.Contains(line, []byte(`"rate_limits"`)) {
		return nil, time.Time{}
	}
	var ev codexEvent
	if json.Unmarshal(line, &ev) != nil || ev.Payload.RateLimits == nil ||
		ev.Payload.RateLimits.Primary == nil && ev.Payload.RateLimits.Secondary == nil {
		return nil, time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, ev.Timestamp)
	if err != nil {
		return nil, time.Time{}
	}
	return &ev, t
}

// codexLimits reads the newest rate limits Codex has reported. They are as
// fresh as the last Codex turn; nil when there are none.
func codexLimits(codexHome string, now time.Time) *CodexLimits {
	var best *codexEvent
	var bestT time.Time
	for i, f := range recentRollouts(codexHome, now) {
		// Events in a file are never newer than the file itself. Files with no
		// usable event (just opened, no turn yet) are passed over.
		if best != nil && f.mod.Before(bestT) || i >= codexMaxOpen {
			break
		}
		if ev, t := lastRateLimits(f.path); ev != nil && (best == nil || t.After(bestT)) {
			best, bestT = ev, t
		}
	}
	if best == nil {
		return nil
	}
	rl := best.Payload.RateLimits
	out := &CodexLimits{Plan: rl.PlanType, Windows: []CodexWindow{}, AsOf: bestT.UTC().Format(time.RFC3339)}
	for _, w := range []struct {
		name string
		w    *codexRateWindow
	}{{"primary", rl.Primary}, {"secondary", rl.Secondary}} {
		if w.w == nil {
			continue
		}
		cw := CodexWindow{Name: w.name, Label: windowLabel(w.w.WindowMinutes), Pct: w.w.UsedPercent, WindowMinutes: w.w.WindowMinutes}
		if w.w.ResetsAt > 0 {
			t := time.Unix(w.w.ResetsAt, 0)
			cw.ResetsAt = t.UTC().Format(time.RFC3339)
			if !t.After(now) {
				cw.Pct, cw.Expired = 0, true
			}
		}
		out.Windows = append(out.Windows, cw)
	}
	return out
}

func windowLabel(min int) string {
	switch {
	case min == 300:
		return labelFiveHour
	case min == 10080:
		return labelWeek
	case min <= 0:
		return "Окно"
	case min%1440 == 0:
		return fmt.Sprintf("%d дн.", min/1440)
	case min%60 == 0:
		return fmt.Sprintf("%d ч", min/60)
	}
	return fmt.Sprintf("%d мин", min)
}
