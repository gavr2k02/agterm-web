package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var limitsNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// fakeCswap writes a script standing in for cswap: it counts its runs and
// prints the given output with the given exit code.
func fakeCswap(t *testing.T, output string, exit int) (path, countFile string) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "out.json")
	if err := os.WriteFile(out, []byte(output), 0o644); err != nil {
		t.Fatal(err)
	}
	countFile = filepath.Join(dir, "count")
	script := "#!/bin/sh\n" +
		`[ "$1 $2" = "list --json" ] || { echo "bad args: $*" >&2; exit 2; }` + "\n" +
		`echo x >> "` + countFile + `"` + "\n" +
		`cat "` + out + `"` + "\n" +
		"exit " + string(rune('0'+exit)) + "\n"
	path = filepath.Join(dir, "cswap")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, countFile
}

func runs(t *testing.T, countFile string) int {
	b, err := os.ReadFile(countFile)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x")
}

const cswapFixture = `{
 "schemaVersion": 1, "activeAccountNumber": 3,
 "accounts": [
  {"number": 1, "email": "user@dead.example", "organizationName": "user@dead.example's Organization",
   "active": false, "usageStatus": "relogin_required", "usage": null,
   "lastGoodUsage": {"fiveHour": {"pct": 61.0, "resetsAt": "2026-09-15T15:50:00.506376+00:00"}},
   "lastGoodFetchedAt": "2026-09-15T13:34:16Z", "lastGoodAgeSeconds": 4746120.6},
  {"number": 2, "email": "ng@corp.example", "organizationName": "Corp Inc", "active": false, "usageStatus": "ok",
   "usage": {"fiveHour": {"pct": 72.0, "resetsAt": "2026-11-09T16:00:00.213341+00:00", "countdown": "4h"},
             "sevenDay": {"pct": 2.0, "resetsAt": "2026-11-15T07:00:00+00:00", "expectedPct": 17.2},
             "scoped": [{"pct": 0.0, "resetsAt": "2026-11-15T07:00:00+00:00", "name": "Fable"}]},
   "usageFetchedAt": "2026-11-09T11:56:17Z"},
  {"number": 3, "email": "user@personal.example", "organizationName": "user@personal.example's Organization",
   "active": true, "usageStatus": "ok",
   "usage": {"fiveHour": {"pct": 99.0, "resetsAt": "2026-11-09T11:00:00+00:00"}, "sevenDay": {"pct": 6.0, "resetsAt": "2026-11-15T23:00:00+00:00"}},
   "usageFetchedAt": "2026-11-09T11:56:17Z"},
  {"number": 4, "email": "x@alias.example", "alias": "work", "active": false, "disabled": true,
   "usageStatus": "unavailable", "usage": null,
   "lastGoodUsage": {"sevenDay": {"pct": 40.0, "resetsAt": "2026-11-12T00:00:00+00:00"}},
   "lastGoodFetchedAt": "2026-11-09T11:30:00Z", "lastGoodAgeSeconds": 1800},
  {"number": 5, "email": "me@gmail.com", "active": false, "usageStatus": "ok",
   "usage": {"fiveHour": {"pct": 30.0, "resetsAt": "2026-11-09T15:59:59.8+00:00"}},
   "usageFetchedAt": "2026-11-09T11:05:00Z", "usageAgeSeconds": 3300}
 ]
}`

func newLimits(t *testing.T, cswap, codexHome string) *LimitsSource {
	l := NewLimitsSource(cswap, codexHome)
	l.Timeout = 5 * time.Second
	l.Now = func() time.Time { return limitsNow }
	return l
}

func TestLimitsClaude(t *testing.T) {
	p, _ := fakeCswap(t, cswapFixture, 0)
	res := newLimits(t, p, filepath.Join(t.TempDir(), "no-codex")).Collect(context.Background())
	if res.ClaudeError != "" || len(res.Claude) != 5 {
		t.Fatalf("got %+v", res)
	}
	// The active account comes first, the rest keep cswap's order.
	personal, dead, corp, aliased, old := res.Claude[0], res.Claude[1], res.Claude[2], res.Claude[3], res.Claude[4]

	// Dead refresh token: an error state, and no months-old last-good numbers.
	if dead.Number != 1 || dead.Status != "relogin_required" || dead.Error != "нужен вход (cswap add)" || dead.FiveHour != nil || dead.SevenDay != nil || dead.Stale {
		t.Errorf("dead account: %+v", dead)
	}
	if dead.Label != "dead" {
		t.Errorf("dead label %q", dead.Label)
	}

	if corp.Label != "Corp" || corp.Error != "" || corp.FiveHour == nil || corp.FiveHour.Pct != 72 || corp.FiveHour.Label != "5 часов" ||
		corp.FiveHour.ResetsAt != "2026-11-09T16:00:00Z" || corp.SevenDay.Pct != 2 || corp.SevenDay.Label != "Неделя" ||
		len(corp.Scoped) != 1 || corp.Scoped[0].Label != "Неделя (Fable)" || corp.Stale || corp.AgeSec == nil || *corp.AgeSec != 223 {
		t.Errorf("corp: %+v", corp)
	}

	// A window whose reset already passed has restarted.
	if !personal.Active || personal.Label != "personal" || !personal.FiveHour.Expired || personal.FiveHour.Pct != 0 {
		t.Errorf("personal: %+v %+v", personal, personal.FiveHour)
	}

	// Recent last-good data is shown, marked stale, next to the error.
	if aliased.Label != "work" || !aliased.Disabled || !aliased.Stale || aliased.SevenDay == nil || aliased.SevenDay.Pct != 40 || aliased.Error == "" {
		t.Errorf("aliased: %+v", aliased)
	}

	// "ok" with an hour-old measurement (cswap in 429 backoff) is stale, not current;
	// a public mail domain is no name, so the masked email stays.
	if !old.Stale || old.Error != "" || old.AsOf != "2026-11-09T11:05:00Z" || *old.AgeSec != 3300 || old.Label != "m***@gmail.com" ||
		old.FiveHour.ResetsAt != "2026-11-09T15:59:59Z" {
		t.Errorf("old measurement: %+v", old)
	}

	b, _ := json.Marshal(res)
	for _, leak := range []string{"user@", "ng@corp", "x@alias", "me@gmail"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("full email %q leaked: %s", leak, b)
		}
	}
	if !strings.Contains(string(b), `"codex":null`) {
		t.Errorf("missing codex dir must give codex:null: %s", b)
	}
}

func TestLimitsClaudeErrors(t *testing.T) {
	envelope, _ := fakeCswap(t, `{"schemaVersion":1,"error":{"type":"ClaudeSwitchError","message":"no accounts for someone@x.example"}}`, 1)
	res := newLimits(t, envelope, t.TempDir()).Collect(context.Background())
	if !strings.Contains(res.ClaudeError, "so***@x.example") || strings.Contains(res.ClaudeError, "someone@") || len(res.Claude) != 0 {
		t.Errorf("envelope: %q", res.ClaudeError)
	}

	res = newLimits(t, filepath.Join(t.TempDir(), "missing-cswap"), t.TempDir()).Collect(context.Background())
	if res.ClaudeError == "" || res.Claude == nil {
		t.Errorf("missing binary: %+v", res)
	}
	if b, _ := json.Marshal(res); !strings.Contains(string(b), `"claude":[]`) {
		t.Errorf("claude must be an empty list, not null: %s", b)
	}

	dir := t.TempDir()
	slow := filepath.Join(dir, "cswap")
	os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755)
	l := newLimits(t, slow, t.TempDir())
	l.Timeout = 200 * time.Millisecond
	start := time.Now()
	res = l.Collect(context.Background())
	if res.ClaudeError == "" || time.Since(start) > 3*time.Second {
		t.Errorf("timeout: %q after %v", res.ClaudeError, time.Since(start))
	}
}

func writeRollout(t *testing.T, home, day, name string, mod time.Time, lines ...string) {
	t.Helper()
	d := filepath.Join(home, "sessions", day)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func tokenCount(ts string, primaryPct float64, primaryReset int64, secondary string) string {
	return `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"token_count","info":{},"rate_limits":{"limit_id":"codex","primary":{"used_percent":` +
		fmtFloat(primaryPct) + `,"window_minutes":10080,"resets_at":` +
		fmtInt(primaryReset) + `},"secondary":` + secondary + `,"plan_type":"prolite"}}}`
}

func fmtFloat(f float64) string { b, _ := json.Marshal(f); return string(b) }
func fmtInt(i int64) string     { b, _ := json.Marshal(i); return string(b) }

func TestCodexLimits(t *testing.T) {
	home := t.TempDir()
	reset := limitsNow.Add(48 * time.Hour).Unix()
	// An older session with both windows, and the session that ran last with
	// secondary null: the newest event wins.
	writeRollout(t, home, "2026/11/08", "rollout-a.jsonl", limitsNow.Add(-20*time.Hour),
		tokenCount("2026-11-08T16:00:00.000Z", 50, reset, `{"used_percent":20,"window_minutes":300,"resets_at":1}`))
	writeRollout(t, home, "2026/11/09", "rollout-b.jsonl", limitsNow.Add(-5*time.Minute),
		`{"timestamp":"2026-11-09T11:50:00.000Z","type":"session_meta","payload":{"type":"x"}}`,
		tokenCount("2026-11-09T11:54:00.000Z", 9, reset, "null"),
		tokenCount("2026-11-09T11:55:00.000Z", 10, reset, "null"),
		`{"timestamp":"2026-11-09T11:55:30.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":null}}`,
		`not json "rate_limits"`)
	// A newer file without any rate limits must not hide the older one.
	writeRollout(t, home, "2026/11/09", "rollout-c.jsonl", limitsNow.Add(-time.Minute),
		`{"timestamp":"2026-11-09T11:59:00.000Z","type":"response_item","payload":{"type":"message"}}`)
	// Other files in the tree are ignored.
	writeRollout(t, home, "2026/11/09", "notes.txt", limitsNow, tokenCount("2026-11-09T11:59:59.000Z", 99, reset, "null"))

	cx := codexLimits(home, limitsNow)
	if cx == nil {
		t.Fatal("no codex limits")
	}
	if cx.Plan != "prolite" || cx.AsOf != "2026-11-09T11:55:00Z" || len(cx.Windows) != 1 {
		t.Fatalf("got %+v", cx)
	}
	w := cx.Windows[0]
	if w.Name != "primary" || w.Label != "Неделя" || w.Pct != 10 || w.WindowMinutes != 10080 || w.ResetsAt != time.Unix(reset, 0).UTC().Format(time.RFC3339) || w.Expired {
		t.Errorf("window %+v", w)
	}

	// Only the older session left: both windows, the 5h one long reset.
	os.Remove(filepath.Join(home, "sessions", "2026/11/09", "rollout-b.jsonl"))
	cx = codexLimits(home, limitsNow)
	if cx == nil || len(cx.Windows) != 2 || cx.Windows[1].Label != "5 часов" || !cx.Windows[1].Expired || cx.Windows[1].Pct != 0 || cx.Windows[0].Pct != 50 {
		t.Errorf("two windows: %+v", cx)
	}

	if codexLimits(filepath.Join(t.TempDir(), "absent"), limitsNow) != nil {
		t.Error("missing codex home must give nil")
	}
}

func TestCodexReadsOnlyTheTail(t *testing.T) {
	home := t.TempDir()
	reset := limitsNow.Add(time.Hour).Unix()
	pad := `{"timestamp":"2026-11-09T11:00:00.000Z","type":"response_item","payload":{"text":"` + strings.Repeat("y", codexMaxLineLen) + `"}}`
	writeRollout(t, home, "2026/11/09", "rollout-big.jsonl", limitsNow,
		tokenCount("2026-11-09T10:00:00.000Z", 5, reset, "null"), pad, tokenCount("2026-11-09T11:58:00.000Z", 33, reset, "null"))
	cx := codexLimits(home, limitsNow)
	if cx == nil || cx.Windows[0].Pct != 33 {
		t.Errorf("got %+v", cx)
	}
}

func TestLimitsCache(t *testing.T) {
	p, count := fakeCswap(t, cswapFixture, 0)
	l := newLimits(t, p, t.TempDir())
	now := limitsNow
	l.Now = func() time.Time { return now }

	a := l.Get(context.Background())
	b := l.Get(context.Background())
	if runs(t, count) != 1 || a != b {
		t.Fatalf("second Get within TTL ran cswap again: %d runs", runs(t, count))
	}
	now = now.Add(limitsTTL - time.Second)
	l.Get(context.Background())
	if runs(t, count) != 1 {
		t.Fatalf("ran before TTL: %d", runs(t, count))
	}
	now = now.Add(2 * time.Second)
	l.Get(context.Background())
	if runs(t, count) != 2 {
		t.Fatalf("not refreshed after TTL: %d", runs(t, count))
	}

	// Concurrent callers share one run.
	now = now.Add(limitsTTL + time.Second)
	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func() { l.Get(context.Background()); done <- struct{}{} }()
	}
	for i := 0; i < 5; i++ {
		<-done
	}
	if runs(t, count) != 3 {
		t.Errorf("concurrent Gets: %d runs, want 3", runs(t, count))
	}

	// A failure is cached too, but for a shorter time.
	bad, badCount := fakeCswap(t, "boom", 1)
	l2 := newLimits(t, bad, t.TempDir())
	now2 := limitsNow
	l2.Now = func() time.Time { return now2 }
	if r := l2.Get(context.Background()); r.ClaudeError == "" {
		t.Fatal("expected an error")
	}
	l2.Get(context.Background())
	if runs(t, badCount) != 1 {
		t.Errorf("failure not cached: %d", runs(t, badCount))
	}
	now2 = now2.Add(limitsErrTTL + time.Second)
	l2.Get(context.Background())
	if runs(t, badCount) != 2 {
		t.Errorf("failure cached past its TTL: %d", runs(t, badCount))
	}
}

func TestLimitsHandler(t *testing.T) {
	s, _ := newTestServer(t)
	p, _ := fakeCswap(t, cswapFixture, 0)
	s.limits = newLimits(t, p, t.TempDir())
	h := s.routes()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/limits", nil))
	if w.Code != 401 {
		t.Errorf("unauthenticated: %d", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, authed(httptest.NewRequest("GET", "/api/limits", nil)))
	if w.Code != 200 {
		t.Fatalf("authed: %d %s", w.Code, w.Body.String())
	}
	var got Limits
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Claude) != 5 || got.FetchedAt == "" {
		t.Errorf("body: %v %s", err, w.Body.String())
	}
}

func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{
		"user@example.com": "us***@example.com",
		"ab@x.io":            "a***@x.io",
		"a@x.io":             "a***@x.io",
		"no-at":              "***",
	} {
		if got := maskEmail(in); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithPath(t *testing.T) {
	env := withPath([]string{"HOME=/h", "PATH=/usr/bin:/bin"})
	var path string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			if path != "" {
				t.Fatal("PATH twice")
			}
			path = v
		}
	}
	if !strings.HasPrefix(path, "/usr/bin:/bin:") || !strings.Contains(path, "/opt/homebrew/bin") || strings.Count(path, "/usr/bin:") != 1 {
		t.Errorf("PATH=%q", path)
	}
}

// Finding 3: a big tool output written after the last token_count must not hide it.
func TestCodexEventBeforeHugeLines(t *testing.T) {
	home := t.TempDir()
	reset := limitsNow.Add(time.Hour).Unix()
	huge := `{"type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 3<<20) + `"}}`
	writeRollout(t, home, "2026/11/09", "rollout-a.jsonl", limitsNow,
		tokenCount("2026-11-09T11:58:00.000Z", 95, reset, "null"), huge, huge,
		`{"timestamp":"2026-11-09T11:59:30.000Z","payload":{"rate_limits":`) // partial last line
	cx := codexLimits(home, limitsNow)
	if cx == nil || cx.Windows[0].Pct != 95 || cx.AsOf != "2026-11-09T11:58:00Z" {
		t.Fatalf("got %+v", cx)
	}
}

// Lines that span chunk boundaries are stitched back together.
func TestCodexChunkBoundaries(t *testing.T) {
	reset := limitsNow.Add(time.Hour).Unix()
	for _, padLen := range []int{0, 1, codexChunk - 300, codexChunk - 1, codexChunk, codexChunk + 7, 2*codexChunk - 150} {
		home := t.TempDir()
		pad := `{"type":"response_item","payload":{"t":"` + strings.Repeat("z", padLen) + `"}}`
		writeRollout(t, home, "2026/11/09", "rollout-a.jsonl", limitsNow,
			tokenCount("2026-11-09T11:00:00.000Z", 1, reset, "null"),
			tokenCount("2026-11-09T11:58:00.000Z", 42, reset, "null"), pad, pad)
		if cx := codexLimits(home, limitsNow); cx == nil || cx.Windows[0].Pct != 42 {
			t.Errorf("pad %d: %+v", padLen, cx)
		}
	}
}

// Finding 4: candidates are picked by mtime across the folders of the last month.
func TestCodexLongRunningSessionAndEmptyNewFiles(t *testing.T) {
	home := t.TempDir()
	reset := limitsNow.Add(time.Hour).Unix()
	// Started Nov 1, still written to now; eight newer day folders exist.
	writeRollout(t, home, "2026/11/01", "rollout-long.jsonl", limitsNow.Add(-2*time.Minute),
		tokenCount("2026-11-09T11:58:00.000Z", 77, reset, "null"))
	for d := 2; d <= 9; d++ {
		os.MkdirAll(filepath.Join(home, "sessions", fmt.Sprintf("2026/11/%02d", d)), 0o755)
	}
	// Many sessions just opened, no turn yet.
	for i := 0; i < 12; i++ {
		writeRollout(t, home, "2026/11/09", fmt.Sprintf("rollout-new-%d.jsonl", i), limitsNow,
			`{"timestamp":"2026-11-09T11:59:00.000Z","type":"session_meta","payload":{}}`)
	}
	// Older than a month: not looked at even with a fresh mtime.
	writeRollout(t, home, "2026/09/01", "rollout-ancient.jsonl", limitsNow,
		tokenCount("2026-11-09T11:59:30.000Z", 5, reset, "null"))
	cx := codexLimits(home, limitsNow)
	if cx == nil || cx.Windows[0].Pct != 77 {
		t.Fatalf("got %+v", cx)
	}
}

func TestAccountLabel(t *testing.T) {
	for _, c := range []struct {
		a    cswapAccount
		want string
	}{
		{cswapAccount{Email: "user@acmemedia.ai", OrganizationName: "user@acmemedia.ai's Organization"}, "acmemedia"},
		{cswapAccount{Email: "n@mail.initech.com"}, "initech"},
		{cswapAccount{Email: "n@shop.example.co.uk"}, "example"},
		{cswapAccount{Email: "ng@x.com", OrganizationName: "Globex Audio Inc"}, "Globex Audio"},
		{cswapAccount{Email: "ng@x.com", OrganizationName: "Acme, Inc."}, "Acme"},
		{cswapAccount{Email: "ng@x.com", OrganizationName: "Inc"}, "Inc"},
		{cswapAccount{Email: "user@gmail.com", Alias: "home"}, "home"},
		{cswapAccount{Email: "user@gmail.com"}, "us***@gmail.com"},
		{cswapAccount{Email: "user@localhost"}, "us***@localhost"},
		{cswapAccount{Number: 7}, "Аккаунт 7"},
	} {
		if got := accountLabel(c.a); got != c.want {
			t.Errorf("accountLabel(%+v) = %q, want %q", c.a, got, c.want)
		}
	}
	accts := []ClaudeAccount{{Label: "corp"}, {Label: "corp"}, {Label: "solo"}}
	dedupeLabels(accts, []cswapAccount{{Email: "anna@corp.io"}, {Email: "boris@corp.io"}, {Email: "c@solo.io"}})
	if accts[0].Label != "corp (an***)" || accts[1].Label != "corp (bo***)" || accts[2].Label != "solo" {
		t.Errorf("dedupe: %+v", accts)
	}
}

// Wall-clock wording is checked in a fixed zone, so the test does not depend on the machine's.
func TestResetText(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	now := time.Date(2026, 11, 9, 15, 12, 0, 0, msk) // Monday
	for in, want := range map[string]string{
		"2026-11-09T15:40:00Z": "сброс сегодня 18:40",
		"2026-11-09T20:59:00Z": "сброс сегодня 23:59",
		"2026-11-09T21:00:00Z": "сброс завтра 00:00",
		"2026-11-10T06:00:00Z": "сброс завтра 09:00",
		"2026-11-13T07:00:00Z": "сброс пт 10:00",
		"2026-11-15T17:30:00Z": "сброс вс 20:30",
		"2026-11-15T23:00:00Z": "сброс 16 ноя",
		"2026-11-09T12:00:00Z": "сброшен",
		"garbage":              "",
	} {
		if got := resetText(in, now); got != want {
			t.Errorf("resetText(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestSwiftBarMenu(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	now := time.Date(2026, 11, 9, 15, 12, 0, 0, msk)
	l := &Limits{
		Claude: []ClaudeAccount{
			{Label: "initech", Active: true, Stale: true, AsOf: "2026-11-09T11:05:00Z",
				SevenDay: &Window{Label: "Неделя", Pct: 6, ResetsAt: "2026-11-15T17:30:00Z"},
				Scoped:   []NamedWindow{{Name: "Opus", Window: Window{Label: "Неделя (Opus)", Pct: 12, ResetsAt: "2026-11-15T17:30:00Z"}}, {Name: "Fable", Window: Window{Label: "Неделя (Fable)", Pct: 0}}}},
			{Label: "Globex Audio", FiveHour: &Window{Label: "5 часов", Pct: 95, ResetsAt: "2026-11-09T15:40:00Z"}},
			{Label: "acmemedia", Status: "relogin_required", Error: "нужен вход (cswap add)"},
		},
		Codex:     &CodexLimits{AsOf: "2026-11-09T11:50:00Z", Windows: []CodexWindow{{Label: "Неделя", Pct: 71, ResetsAt: "2026-11-11T05:20:00Z"}}},
		FetchedAt: "2026-11-09T12:12:00Z",
	}
	got := SwiftBarMenu(l, now)
	nb := "\u00a0"
	for _, want := range []string{
		// The title shows the active account (stale, so "~"), not the 95% of an inactive one.
		"Claude ~12% · Codex 71% | color=#ff8a3d\n---\n",
		nb + nb + "● initech — активный\n",
		"Неделя: использовано ~6% · сброс вс 20:30\n",
		"Неделя (Opus): использовано ~12% · сброс вс 20:30\n",
		"данные от 14:05 | size=11\n",
		"5 часов: использовано 95% · сброс сегодня 18:40 | color=#ff5a5a\n",
		"acmemedia — нужен вход (cswap add) | color=#ff5a5a\n",
		"Неделя: использовано 71% · сброс ср 08:20 | color=#ff8a3d\n",
		"данные Codex от 14:50 | size=11\n",
		"Обновлено в 15:12 | size=11\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("menu lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Fable") {
		t.Errorf("an empty scoped window is noise:\n%s", got)
	}

	// No data on either side; a "|" in a label cannot inject SwiftBar parameters.
	got = SwiftBarMenu(&Limits{ClaudeError: "cswap не найден", Claude: []ClaudeAccount{{Label: "a|b href=x"}}, FetchedAt: "2026-11-09T12:12:00Z"}, now)
	if !strings.HasPrefix(got, "Claude — · Codex —\n") || !strings.Contains(got, "a¦b href=x\n") || !strings.Contains(got, "cswap не найден | color=#ff5a5a") {
		t.Errorf("empty menu:\n%s", got)
	}
}

// Codex re-audit: equal masked local parts still need distinct names.
func TestDedupeAfterMasking(t *testing.T) {
	raw := []cswapAccount{{Number: 1, Email: "nina@corp.example"}, {Number: 2, Email: "nick@corp.example"}, {Number: 3, Email: "olga@corp.example"}}
	accts := []ClaudeAccount{{Number: 1, Label: accountLabel(raw[0])}, {Number: 2, Label: accountLabel(raw[1])}, {Number: 3, Label: accountLabel(raw[2])}}
	dedupeLabels(accts, raw)
	if accts[0].Label != "corp (ni***) #1" || accts[1].Label != "corp (ni***) #2" || accts[2].Label != "corp (ol***)" {
		t.Errorf("labels: %q %q %q", accts[0].Label, accts[1].Label, accts[2].Label)
	}
}

// Codex re-audit: expiry uses the exact reset time; only the shown clock is rounded.
func TestResetNotRoundedEarly(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 10, 0, time.UTC)
	w := window(&cswapWindow{Pct: 100, ResetsAt: "2026-11-09T12:00:20Z"}, labelFiveHour, now)
	if w.Pct != 100 || w.Expired {
		t.Errorf("Claude reset early: %+v", w)
	}
	h := t.TempDir()
	writeRollout(t, h, "2026/11/09", "rollout-a.jsonl", now, tokenCount("2026-11-09T12:00:00Z", 100, now.Add(10*time.Second).Unix(), "null"))
	if cx := codexLimits(h, now); cx == nil || cx.Windows[0].Pct != 100 || cx.Windows[0].Expired {
		t.Errorf("Codex reset early: %+v", cx)
	}
	if got := resetText("2026-11-09T15:59:59.8Z", time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)); got != "сброс сегодня 16:00" {
		t.Errorf("display rounding: %q", got)
	}
}

func TestResetTextZones(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err == nil { // DST ends that night
		if got := resetText("2026-11-02T06:00:00Z", time.Date(2026, 11, 1, 0, 30, 0, 0, ny)); got != "сброс завтра 01:00" {
			t.Errorf("DST: %q", got)
		}
	}
	now := time.Date(2026, 11, 9, 23, 45, 0, 0, time.FixedZone("MSK", 10800))
	if got := resetText("2026-11-10T00:00:00+03:00", now); got != "сброс завтра 00:00" {
		t.Errorf("midnight: %q", got)
	}
	menu := SwiftBarMenu(&Limits{Claude: []ClaudeAccount{{Label: "hello|href=https://x.example\n---\nworld", Active: true}}, FetchedAt: now.Format(time.RFC3339)}, now)
	if strings.Contains(menu, "|href=") || strings.Contains(menu, "\n---\nworld") {
		t.Errorf("label injected SwiftBar syntax:\n%s", menu)
	}
}

// Random sizes of the lines around the newest event (ported from the re-audit, fewer cases).
func TestBackwardScannerRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	reset := limitsNow.Add(time.Hour).Unix()
	for i := 0; i < 16; i++ {
		path := filepath.Join(t.TempDir(), "rollout-a.jsonl")
		var b strings.Builder
		b.WriteString(tokenCount("2026-11-09T11:00:00Z", 5, reset, "null") + "\n")
		b.WriteString(strings.Repeat("a", rng.Intn(3<<20)) + "\n")
		b.WriteString(tokenCount("2026-11-09T11:59:00Z", 42, reset, `{"used_percent":73,"window_minutes":300,"resets_at":1794268800}`) + "\n")
		b.WriteString(strings.Repeat("b", rng.Intn(3<<20)) + "\n")
		if i%2 == 0 {
			b.WriteString(`{"payload":{"rate_limits":`)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		if ev, _ := lastRateLimits(path); ev == nil || ev.Payload.RateLimits.Primary.UsedPercent != 42 || ev.Payload.RateLimits.Secondary.UsedPercent != 73 {
			t.Fatalf("case %d: %+v", i, ev)
		}
	}
}
