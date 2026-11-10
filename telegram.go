package main

// A minimal Telegram Bot API client: JSON over HTTPS, long polling.
//
// The bot token is part of every request URL, and Go's *url.Error quotes the
// URL, so no error from net/http leaves this file unredacted.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const defaultTelegramAPI = "https://api.telegram.org"

type tgClient struct {
	base  string
	token string
	http  *http.Client
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time

	hmu    sync.Mutex
	health tgHealth
}

func newTGClient(base, token string) *tgClient {
	return &tgClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 75 * time.Second}, // above the 50 s long poll
		sleep: sleepCtx,
		now:   time.Now,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// tgError is an error answer of the Bot API.
type tgError struct {
	Method     string
	Code       int
	Desc       string
	RetryAfter time.Duration
}

// errStaleGen: the result belongs to an earlier pairing and was dropped.
var errStaleGen = errors.New("telegram: pairing changed, result dropped")

func (e *tgError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Desc)
}

func (c *tgClient) redact(s string) string {
	if c.token == "" {
		return s
	}
	s = strings.ReplaceAll(s, c.token, "<token>")
	return strings.ReplaceAll(s, url.PathEscape(c.token), "<token>")
}

// call posts params as JSON to a Bot API method and decodes "result" into out.
// It does not retry: a 429 is retried by tgOutbox (which shares the cooldown
// with every other sender to that chat) or by the poll loop.
func (c *tgClient) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.once(ctx, method, body, out)
}

// transient: worth retrying later (network, Telegram 5xx, flood control).
func transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errStaleGen) {
		return false
	}
	var te *tgError
	if errors.As(err, &te) {
		return te.Code == http.StatusTooManyRequests || te.Code >= 500
	}
	return true
}

func (c *tgClient) once(ctx context.Context, method string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New(c.redact(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if !errors.Is(ctx.Err(), context.Canceled) { // shutting down is not a failure
			c.record(method, errKind(err, 0))
		}
		// *url.Error carries the URL with the token: keep only the redacted text.
		return fmt.Errorf("telegram %s: %s", method, c.redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		c.record(method, errKind(err, 0))
		return fmt.Errorf("telegram %s: %s", method, c.redact(err.Error()))
	}
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		c.record(method, errKind(nil, max(resp.StatusCode, 500))) // garbage from a proxy or an outage page
		return &tgError{Method: method, Code: resp.StatusCode, Desc: "bad response"}
	}
	if !env.OK {
		code := env.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		c.record(method, errKind(nil, code))
		return &tgError{Method: method, Code: code, Desc: c.redact(env.Description), RetryAfter: time.Duration(env.Parameters.RetryAfter) * time.Second}
	}
	c.record(method, "")
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("telegram %s: decode result: %v", method, err)
		}
	}
	return nil
}

/* ---------- types (only the fields the bot uses) ---------- */

type tgUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

// Telegram's stand-in senders: anonymous group admins, channel posts, service.
var syntheticUsers = map[int64]bool{1087968824: true, 136817688: true, 777000: true}

// human: a real person's own account, the only kind the bot may trust. An
// anonymous admin's message carries sender_chat and a shared placeholder user,
// so it could be any admin of the group.
func (u *tgUser) human() bool { return u != nil && u.ID > 0 && !u.IsBot && !syntheticUsers[u.ID] }

type tgChat struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"`
	Title   string `json:"title"`
	IsForum bool   `json:"is_forum"`
}

type tgMessage struct {
	MessageID       int64           `json:"message_id"`
	MessageThreadID int64           `json:"message_thread_id"`
	IsTopicMessage  bool            `json:"is_topic_message"`
	From            *tgUser         `json:"from"`
	SenderChat      json.RawMessage `json:"sender_chat"`
	Chat            tgChat          `json:"chat"`
	Text            string          `json:"text"`
	Caption         string          `json:"caption"`
	MediaGroupID    string          `json:"media_group_id"`
	ReplyTo         *tgMessage      `json:"reply_to_message"`
	ReplyMarkup     json.RawMessage `json:"reply_markup"`
	// Service message a topic rename leaves behind.
	ForumTopicEdited json.RawMessage `json:"forum_topic_edited"`

	// Present on media messages; the bot only takes text.
	Photo     json.RawMessage `json:"photo"`
	Document  json.RawMessage `json:"document"`
	Voice     json.RawMessage `json:"voice"`
	Audio     json.RawMessage `json:"audio"`
	Video     json.RawMessage `json:"video"`
	VideoNote json.RawMessage `json:"video_note"`
	Sticker   json.RawMessage `json:"sticker"`
	Animation json.RawMessage `json:"animation"`
}

func (m *tgMessage) fromHuman() bool {
	return m.From.human() && (len(m.SenderChat) == 0 || string(m.SenderChat) == "null")
}

func (m *tgMessage) hasMedia() bool {
	for _, r := range []json.RawMessage{m.Photo, m.Document, m.Voice, m.Audio, m.Video, m.VideoNote, m.Sticker, m.Animation} {
		if len(r) > 0 && string(r) != "null" {
			return true
		}
	}
	return false
}

type tgCallback struct {
	ID      string     `json:"id"`
	From    tgUser     `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

type tgUpdate struct {
	UpdateID int64       `json:"update_id"`
	Message  *tgMessage  `json:"message"`
	Callback *tgCallback `json:"callback_query"`
}

type tgButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data,omitempty"`
	URL  string `json:"url,omitempty"`
}

type tgInlineKeyboard struct {
	Rows [][]tgButton `json:"inline_keyboard"`
}

type tgTopic struct {
	ThreadID int64  `json:"message_thread_id"`
	Name     string `json:"name"`
}
