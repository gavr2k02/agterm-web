package main

// Plain-language rendering of the limits for the macOS menu bar (SwiftBar).
// web/app.js renders the same wording for the panel.

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	warnPct      = 70
	dangerPct    = 90
	codexOldData = 10 * time.Minute // older Codex numbers get a "данные от" note
	colorWarn    = "#ff8a3d"
	colorDanger  = "#ff5a5a"
	// SwiftBar may trim plain leading spaces, so indents are no-break spaces.
	headIndent = "\u00a0\u00a0"
	indent     = "\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0"
)

var (
	ruWeekdays = [...]string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}
	ruMonths   = [...]string{"янв", "фев", "мар", "апр", "мая", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"}
)

// resetText says when a window resets, as local wall time:
// "сегодня 18:40", "завтра 09:00", "пт 10:00" (within six days), "15 ноя".
// now's location is the one used.
func resetText(resetsAt string, now time.Time) string {
	t, err := time.Parse(time.RFC3339Nano, resetsAt)
	if err != nil {
		return ""
	}
	if !t.After(now) {
		return "сброшен"
	}
	// Expiry uses the exact time; the clock shows the nearest minute
	// (the API reports 15:59:59.8 for a 16:00 reset).
	t = t.Round(time.Minute).In(now.Location())
	day := func(x time.Time) time.Time { y, m, d := x.Date(); return time.Date(y, m, d, 0, 0, 0, 0, x.Location()) }
	days := int(math.Round(day(t).Sub(day(now)).Hours() / 24))
	hm := t.Format("15:04")
	switch {
	case days == 0:
		return "сброс сегодня " + hm
	case days == 1:
		return "сброс завтра " + hm
	case days <= 6:
		return "сброс " + ruWeekdays[t.Weekday()] + " " + hm
	}
	return fmt.Sprintf("сброс %d %s", t.Day(), ruMonths[t.Month()-1])
}

func windowPct(w Window, now time.Time) int {
	if w.Expired {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil && !t.After(now) {
		return 0
	}
	return int(math.Round(w.Pct))
}

func levelColor(pct int) string {
	switch {
	case pct >= dangerPct:
		return colorDanger
	case pct >= warnPct:
		return colorWarn
	}
	return ""
}

// accountWindows: the windows worth a line, in display order.
func accountWindows(a ClaudeAccount) []Window {
	var ws []Window
	if a.FiveHour != nil {
		ws = append(ws, *a.FiveHour)
	}
	if a.SevenDay != nil {
		ws = append(ws, *a.SevenDay)
	}
	for _, s := range a.Scoped {
		if s.Pct >= 1 {
			ws = append(ws, s.Window)
		}
	}
	return ws
}

func pctText(pct int, stale bool) string {
	if stale {
		return fmt.Sprintf("~%d%%", pct)
	}
	return fmt.Sprintf("%d%%", pct)
}

// menuTitle: "Claude 12% · Codex 40%" — the active Claude account's fullest
// window and Codex's fullest window; "—" where there is no data. Returns the
// color of the worse side ("" when below the warning level).
func menuTitle(l *Limits, now time.Time) (string, string) {
	worst := -1
	claude := "—"
	for _, a := range l.Claude {
		if !a.Active {
			continue
		}
		max := -1
		for _, w := range accountWindows(a) {
			max = maxInt(max, windowPct(w, now))
		}
		if max >= 0 {
			claude = pctText(max, a.Stale)
			worst = maxInt(worst, max)
		}
	}
	codex := "—"
	if l.Codex != nil && len(l.Codex.Windows) > 0 {
		max := -1
		for _, w := range l.Codex.Windows {
			max = maxInt(max, windowPct(Window{Pct: w.Pct, ResetsAt: w.ResetsAt, Expired: w.Expired}, now))
		}
		codex = pctText(max, false)
		worst = maxInt(worst, max)
	}
	return "Claude " + claude + " · Codex " + codex, levelColor(worst)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// swiftbarLine escapes text for SwiftBar, where "|" starts the parameters.
func swiftbarLine(b *strings.Builder, text string, params ...string) {
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(text, "|", "¦"), "\n", " "))
	var ps []string
	for i := 0; i+1 < len(params); i += 2 {
		if params[i+1] != "" {
			ps = append(ps, params[i]+"="+params[i+1])
		}
	}
	if len(ps) > 0 {
		b.WriteString(" | " + strings.Join(ps, " "))
	}
	b.WriteByte('\n')
}

func windowLine(b *strings.Builder, label string, pct int, stale bool, resetsAt string, now time.Time) {
	text := indent + label + ": использовано " + pctText(pct, stale)
	if r := resetText(resetsAt, now); r != "" {
		text += " · " + r
	}
	swiftbarLine(b, text, "color", levelColor(pct))
}

func localHM(rfc string, now time.Time) string {
	t, err := time.Parse(time.RFC3339Nano, rfc)
	if err != nil {
		return ""
	}
	t = t.In(now.Location())
	if y, m, d := t.Date(); y != now.Year() || m != now.Month() || d != now.Day() {
		return fmt.Sprintf("%d %s %s", d, ruMonths[m-1], t.Format("15:04"))
	}
	return t.Format("15:04")
}

// SwiftBarMenu renders the whole plugin output.
func SwiftBarMenu(l *Limits, now time.Time) string {
	var b strings.Builder
	title, color := menuTitle(l, now)
	swiftbarLine(&b, title, "color", color)
	b.WriteString("---\n")

	swiftbarLine(&b, "Claude", "size", "13")
	if l.ClaudeError != "" {
		swiftbarLine(&b, indent+l.ClaudeError, "color", colorDanger)
	}
	for _, a := range l.Claude {
		head := headIndent + a.Label
		if a.Active {
			head = headIndent + "● " + a.Label + " — активный"
		}
		if a.Disabled {
			head += " — выключен"
		}
		if a.Error != "" {
			swiftbarLine(&b, head+" — "+a.Error, "color", colorDanger)
		} else {
			swiftbarLine(&b, head)
		}
		for _, w := range accountWindows(a) {
			windowLine(&b, w.Label, windowPct(w, now), a.Stale, w.ResetsAt, now)
		}
		if a.Stale && a.AsOf != "" {
			swiftbarLine(&b, indent+"данные от "+localHM(a.AsOf, now), "size", "11")
		} else if a.Error == "" && len(accountWindows(a)) == 0 {
			swiftbarLine(&b, indent+"нет данных", "size", "11")
		}
	}

	swiftbarLine(&b, "Codex", "size", "13")
	if cx := l.Codex; cx == nil || len(cx.Windows) == 0 {
		swiftbarLine(&b, indent+"нет данных")
	} else {
		for _, w := range cx.Windows {
			windowLine(&b, w.Label, windowPct(Window{Pct: w.Pct, ResetsAt: w.ResetsAt, Expired: w.Expired}, now), false, w.ResetsAt, now)
		}
		if t, err := time.Parse(time.RFC3339Nano, cx.AsOf); err == nil && now.Sub(t) > codexOldData {
			swiftbarLine(&b, indent+"данные Codex от "+localHM(cx.AsOf, now), "size", "11")
		}
	}

	b.WriteString("---\n")
	if l.Bot != nil {
		color := ""
		if l.Bot.Bad {
			color = colorDanger
		}
		swiftbarLine(&b, l.Bot.Text, "color", color, "size", "12")
	}
	swiftbarLine(&b, "Обновлено в "+localHM(l.FetchedAt, now), "size", "11")
	swiftbarLine(&b, "Обновить", "refresh", "true")
	return b.String()
}

// LimitsText is the same menu as plain text (for Telegram): SwiftBar parameters
// and the refresh item dropped. Labels cannot hold "|" (swiftbarLine replaced
// it), so the last " | " of a line is always ours.
func LimitsText(l *Limits, now time.Time) string {
	var out []string
	for _, line := range strings.Split(strings.TrimRight(SwiftBarMenu(l, now), "\n"), "\n") {
		if i := strings.LastIndex(line, " | "); i >= 0 {
			if strings.HasSuffix(line, "refresh=true") {
				continue
			}
			line = line[:i]
		}
		if line == "---" {
			line = ""
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
