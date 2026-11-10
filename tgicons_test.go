package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// iconStickers answers getForumTopicIconStickers like Telegram (some emoji
// without U+FE0F); every other call falls through.
func iconStickers(h *botHarness) {
	h.tg.hook = func(m string, p map[string]any) (int, string, bool) {
		if m == "getForumTopicIconStickers" {
			return 200, `{"ok":true,"result":[
				{"emoji":"💼","custom_emoji_id":"id-brief"},{"emoji":"🎬","custom_emoji_id":"id-clap"},
				{"emoji":"⚽","custom_emoji_id":"id-ball"},{"emoji":"💡","custom_emoji_id":"id-bulb"},
				{"emoji":"⚡","custom_emoji_id":"id-bolt"},{"emoji":"📁","custom_emoji_id":"id-folder"},
				{"emoji":"💬","custom_emoji_id":"id-chat"}]}`, true
		}
		return 0, "", false
	}
}

func TestWorkspaceStyle(t *testing.T) {
	h := newBotHarness(t)
	for ws, want := range map[string]topicStyle{
		"DXP": {"💼", "blue"}, "dxp": {"💼", "blue"}, "EM": {"🎬", "green"},
		"Своя лига": {"⚽️", "violet"}, "GLOBAL": {"💡", "yellow"},
	} {
		if got := h.bot.workspaceStyle(ws); got != want {
			t.Errorf("%s: %+v", ws, got)
		}
	}
	// Unknown: 💬 and a spare color that stays the same for the name.
	a, b := h.bot.workspaceStyle("Research"), h.bot.workspaceStyle("research")
	if a.Icon != "💬" || a != b || (a.Color != "pink" && a.Color != "red") {
		t.Errorf("unknown: %+v %+v", a, b)
	}
	// Settings override, case-insensitive; a bad color keeps the default one.
	os.WriteFile(h.bot.settingsPath, []byte(`{"workspaces":{"dxp":{"icon":"🔥","color":"pink"},"EM":{"color":"nope"},"Research":{"icon":"💡"}}}`), 0o600)
	if got := h.bot.workspaceStyle("DXP"); got != (topicStyle{"🔥", "pink"}) {
		t.Errorf("override: %+v", got)
	}
	if got := h.bot.workspaceStyle("EM"); got != (topicStyle{"🎬", "green"}) {
		t.Errorf("bad color: %+v", got)
	}
	if got := h.bot.workspaceStyle("Research"); got.Icon != "💡" || got.Color != a.Color {
		t.Errorf("partial override: %+v", got)
	}
}

func TestTopicIcons(t *testing.T) {
	h := newBotHarness(t)
	iconStickers(h)
	h.pair(forumChat, true)
	h.flush()
	calls := h.tg.take()
	created := map[string]tgCall{}
	for _, c := range only(calls, "createForumTopic") {
		created[str(c.Params["name"])] = c
	}
	l, s := created[launcherTitle], created["EM · Fix bug"]
	if num(l.Params["icon_color"]) != 16478047 || str(l.Params["icon_custom_emoji_id"]) != "id-bolt" {
		t.Errorf("launcher: %+v", l.Params)
	}
	if num(s.Params["icon_color"]) != 9367192 || str(s.Params["icon_custom_emoji_id"]) != "id-clap" || h.bot.st.Topics[sidA].Icon != "🎬" {
		t.Errorf("session topic: %+v", s.Params)
	}
	if !h.bot.st.IconsV1 || !h.bot.st.LauncherIcon {
		t.Error("fresh topics need no migration")
	}
	if e := only(calls, "editForumTopic"); len(e) != 0 {
		t.Errorf("fresh topics edited: %+v", e)
	}

	// Moved to DXP: one edit with the new name and icon.
	h.ag.setTreeWS(t, "DXP", fakeSession{sidA, "◐ Fix bug", "blocked"}, fakeSession{sidB, "shell", "idle"})
	h.clock = h.clock.Add(h.bot.renameGap + 1)
	h.bot.syncTopics(context.Background())
	var moveA []tgCall
	for _, c := range only(h.tg.take(), "editForumTopic") {
		if num(c.Params["message_thread_id"]) == h.bot.st.Topics[sidA].Thread {
			moveA = append(moveA, c)
		}
	}
	if len(moveA) != 1 || str(moveA[0].Params["name"]) != "DXP · Fix bug" || str(moveA[0].Params["icon_custom_emoji_id"]) != "id-brief" || h.bot.st.Topics[sidA].Icon != "💼" {
		t.Fatalf("move: %+v", moveA)
	}
	// Close: deleted, not renamed.
	thread := h.bot.st.Topics[sidA].Thread
	h.bot.onEvent(agtermEvent{Kind: "session.closed", Session: sidA})
	calls = h.tg.take()
	if d := only(calls, "deleteForumTopic"); len(d) != 1 || num(d[0].Params["message_thread_id"]) != thread || len(only(calls, "editForumTopic")) != 0 {
		t.Fatalf("close: %+v", calls)
	}
}

func TestTopicIconMigration(t *testing.T) {
	h := newBotHarness(t)
	iconStickers(h)
	h.pair(forumChat, true)
	h.bot.st.Topics = map[string]*tgTopicRef{
		sidA: {Thread: 11, Title: "🟠 EM · Fix bug", Workspace: "EM", Name: "Fix bug"},
		sidB: {Thread: 12, Title: "EM · shell", Workspace: "EM", Name: "shell"},
		sidC: {Thread: 13, Title: "EM · done", Workspace: "EM", Name: "done", Icon: "🎬"}, // already has one
	}
	h.bot.st.TitlesV2, h.bot.st.IconsV1 = false, false
	h.bot.st.LauncherThread, h.bot.st.LauncherMsg, h.bot.st.LauncherPinned, h.bot.st.LauncherIcon = 99, 100, true, false
	h.bot.launcherCheck = h.clock
	h.ag.setTree(t, fakeSession{sidA, "x", "idle"}, fakeSession{sidB, "shell", "idle"}, fakeSession{sidC, "done", "idle"})
	h.tg.take()
	h.bot.syncTopics(context.Background())
	got := map[int64]string{}
	edits := only(h.tg.take(), "editForumTopic")
	if len(edits) != 3 {
		t.Errorf("%d edits, want one per topic needing it", len(edits))
	}
	for _, c := range edits {
		got[num(c.Params["message_thread_id"])] = fmt.Sprintf("%s|%s", str(c.Params["name"]), str(c.Params["icon_custom_emoji_id"]))
	}
	want := map[int64]string{11: "EM · Fix bug|id-clap", 12: "|id-clap", 99: "|id-bolt"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("migration edits %v, want %v", got, want)
	}
	if !h.bot.st.IconsV1 || !h.bot.st.TitlesV2 || !h.bot.st.LauncherIcon || h.bot.st.Topics[sidB].Icon != "🎬" {
		t.Errorf("flags: %+v", h.bot.st)
	}
	if c := only(h.tg.take(), "createForumTopic"); len(c) != 0 {
		t.Error("topics recreated")
	}
	h.bot.syncTopics(context.Background())
	if e := only(h.tg.take(), "editForumTopic"); len(e) != 0 {
		t.Errorf("migrated twice: %+v", e)
	}
}

func TestTopicIconNotAllowed(t *testing.T) {
	h := newBotHarness(t)
	iconStickers(h)
	os.WriteFile(h.bot.settingsPath, []byte(`{"workspaces":{"EM":{"icon":"🦄","color":"pink"}}}`), 0o600)
	h.pair(forumChat, true)
	h.flush()
	for _, c := range only(h.tg.take(), "createForumTopic") {
		if strings.HasPrefix(str(c.Params["name"]), "EM ·") {
			if _, has := c.Params["icon_custom_emoji_id"]; has || num(c.Params["icon_color"]) != 16749490 {
				t.Errorf("not-allowed icon: %+v", c.Params)
			}
		}
	}
	if !h.bot.iconWarned["🦄"] {
		t.Error("not logged")
	}
}
