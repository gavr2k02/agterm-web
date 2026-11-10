package main

// Topic icons and colors per workspace. Telegram allows only its own set of
// topic icons (getForumTopicIconStickers: custom emoji ids) and six colors; the
// color can only be chosen when a topic is created, the icon can be changed.

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log"
	"os"
	"strings"
	"time"
)

const iconsRefresh = 24 * time.Hour

var topicColors = map[string]int{
	"blue": 7322096, "yellow": 16766590, "violet": 13338331, "green": 9367192, "pink": 16749490, "red": 16478047,
}

type topicStyle struct {
	Icon  string `json:"icon"`
	Color string `json:"color"`
}

var (
	defaultStyles = map[string]topicStyle{
		"DXP":       {"💼", "blue"},
		"EM":        {"🎬", "green"},
		"СВОЯ ЛИГА": {"⚽️", "violet"},
		"GLOBAL":    {"💡", "yellow"},
	}
	launcherStyle = topicStyle{"⚡️", "red"}
	archiveIcon   = "📁"
	// Colors no default workspace uses, for workspaces without a style.
	spareColors = []string{"pink", "red"}
)

func normEmoji(e string) string { return strings.ReplaceAll(e, "️", "") }

// workspaceStyle: the settings file's style for the workspace (name compared
// without case), else the default, else 💬 with a spare color chosen by the
// name's hash (so it stays the same).
func (b *tgBot) workspaceStyle(ws string) topicStyle {
	var cfg struct {
		Workspaces map[string]topicStyle `json:"workspaces"`
	}
	if raw, err := os.ReadFile(b.settingsPath); err == nil {
		json.Unmarshal(raw, &cfg)
	}
	pick := func(m map[string]topicStyle) (topicStyle, bool) {
		for k, v := range m {
			if strings.EqualFold(strings.TrimSpace(k), strings.TrimSpace(ws)) {
				return v, true
			}
		}
		return topicStyle{}, false
	}
	st, ok := pick(cfg.Workspaces)
	def, hasDef := pick(defaultStyles)
	if !hasDef {
		h := fnv.New32a()
		h.Write([]byte(strings.ToUpper(ws)))
		def = topicStyle{"💬", spareColors[int(h.Sum32())%len(spareColors)]}
	}
	if !ok {
		return def
	}
	if st.Icon == "" {
		st.Icon = def.Icon
	}
	if _, valid := topicColors[st.Color]; !valid {
		st.Color = def.Color
	}
	return st
}

// iconID: the custom emoji id Telegram allows for this emoji, "" if none
// (logged once per emoji).
func (b *tgBot) iconID(emoji string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id := b.st.Icons[normEmoji(emoji)]; id != "" {
		return id
	}
	if len(b.st.Icons) > 0 && !b.iconWarned[emoji] {
		b.iconWarned[emoji] = true
		log.Printf("telegram: %s is not an allowed topic icon, topics go without it", emoji)
	}
	return ""
}

// refreshIcons loads the allowed topic icons, at most once a day.
func (b *tgBot) refreshIcons(ctx context.Context) {
	b.mu.Lock()
	fresh := len(b.st.Icons) > 0 && b.now().Sub(b.st.IconsAt) < iconsRefresh
	b.mu.Unlock()
	if fresh {
		return
	}
	var stickers []struct {
		Emoji         string `json:"emoji"`
		CustomEmojiID string `json:"custom_emoji_id"`
	}
	if err := b.api.call(ctx, "getForumTopicIconStickers", map[string]any{}, &stickers); err != nil {
		log.Printf("%v", err)
		return
	}
	icons := map[string]string{}
	for _, s := range stickers {
		if s.CustomEmojiID != "" && s.Emoji != "" {
			icons[normEmoji(s.Emoji)] = s.CustomEmojiID
		}
	}
	b.mu.Lock()
	b.st.Icons, b.st.IconsAt = icons, b.now()
	b.saveLocked()
	b.mu.Unlock()
}

// topicCreateParams: a new topic with its workspace's color and icon.
func (b *tgBot) topicCreateParams(chat int64, name string, style topicStyle) map[string]any {
	p := map[string]any{"chat_id": chat, "name": name, "icon_color": topicColors[style.Color]}
	if id := b.iconID(style.Icon); id != "" {
		p["icon_custom_emoji_id"] = id
	}
	return p
}
