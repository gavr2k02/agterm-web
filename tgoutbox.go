package main

// tgOutbox is the single way bot output reaches Telegram. Every chat-scoped
// method (messages, edits, topics, reactions) reserves a slot here first, so all
// senders together stay under Telegram's limits: about one message a second per
// chat and twenty a minute per group. Slots are handed out first come, first
// served across goroutines, which makes the outbox a FIFO queue per chat.
//
// Later output kinds (e.g. a mirrored conversation whose tool-call summary is
// edited in place each turn) are meant to go through the same call, so they
// share the budget with notifications instead of racing them.

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

type tgOutbox struct {
	api   *tgClient
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	perChat     time.Duration // gap between two sends to one chat
	groupWindow time.Duration
	groupMax    int // sends per groupWindow to one group (chat id < 0); 0 = no limit

	mu     sync.Mutex
	prio   map[int64]int         // priority calls in flight per chat; bulk traffic yields to them
	cool   map[int64]time.Time   // Telegram's retry_after per chat: nothing goes out before it
	sent   map[int64]time.Time   // when the last call to the chat actually went out
	next   map[int64]time.Time   // earliest next slot per chat
	recent map[int64][]time.Time // reserved slots of a group within the window, ascending
}

func newTGOutbox(api *tgClient) *tgOutbox {
	return &tgOutbox{
		api: api, now: time.Now, sleep: sleepCtx,
		perChat: time.Second, groupWindow: time.Minute, groupMax: 20,
		prio: map[int64]int{}, cool: map[int64]time.Time{}, sent: map[int64]time.Time{}, next: map[int64]time.Time{}, recent: map[int64][]time.Time{},
	}
}

// reserve books the next free slot for chat and returns how long to wait for it.
func (o *tgOutbox) reserve(chat int64) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	at := now
	if n := o.next[chat]; n.After(at) {
		at = n
	}
	if chat < 0 && o.groupMax > 0 {
		r := o.recent[chat]
		for len(r) > 0 && !r[0].After(at.Add(-o.groupWindow)) {
			r = r[1:]
		}
		if len(r) >= o.groupMax {
			if t := r[len(r)-o.groupMax].Add(o.groupWindow); t.After(at) {
				at = t
			}
			for len(r) > 0 && !r[0].After(at.Add(-o.groupWindow)) {
				r = r[1:]
			}
		}
		o.recent[chat] = append(r, at)
	}
	o.next[chat] = at.Add(o.perChat)
	return at.Sub(now)
}

// cooldown keeps chat quiet for d (Telegram's retry_after) for every sender.
func (o *tgOutbox) cooldown(chat int64, d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.now().Add(d)
	if t.After(o.cool[chat]) {
		o.cool[chat] = t
	}
	if t.After(o.next[chat]) {
		o.next[chat] = t
	}
}

// claim marks the chat as sent to now, or says how long to wait first.
func (o *tgOutbox) claim(chat int64) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	if last, ok := o.sent[chat]; ok {
		if w := last.Add(o.perChat).Sub(now); w > 0 {
			return w
		}
	}
	o.sent[chat] = now
	return 0
}

// coolWait: how long chat's cooldown still holds.
func (o *tgOutbox) coolWait(chat int64) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cool[chat].Sub(o.now())
}

// call is the priority lane: answers to the user, notifications. It waits for
// chat's slot, then calls the method. A 429 sets the chat's cooldown and the
// call queues again for a new slot (3 tries).
func (o *tgOutbox) call(ctx context.Context, chat int64, method string, params, out any) error {
	o.mu.Lock()
	o.prio[chat]++
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		if o.prio[chat]--; o.prio[chat] <= 0 {
			delete(o.prio, chat)
		}
		o.mu.Unlock()
	}()
	return o.do(ctx, chat, method, params, out)
}

// callBulk is the bulk lane (the mirrored conversation): it lets every
// priority call to the chat go first, then takes a slot like any other.
func (o *tgOutbox) callBulk(ctx context.Context, chat int64, method string, params, out any) error {
	for {
		o.mu.Lock()
		busy := o.prio[chat] > 0
		o.mu.Unlock()
		if !busy {
			break
		}
		if err := o.sleep(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
	return o.do(ctx, chat, method, params, out)
}

func (o *tgOutbox) do(ctx context.Context, chat int64, method string, params, out any) error {
	for attempt := 0; ; attempt++ {
		// Take a slot; if a 429 cooldown came up while waiting for it, wait the
		// cooldown out and take a new slot, so calls queued behind the cooldown
		// keep their spacing instead of all going at its end.
		for {
			if d := o.reserve(chat); d > 0 {
				if err := o.sleep(ctx, d); err != nil {
					return err
				}
			}
			d := o.coolWait(chat)
			if d <= 0 {
				break
			}
			if err := o.sleep(ctx, d); err != nil {
				return err
			}
		}
		// Slots are planned; sleeps overshoot. The gap is enforced against
		// the previous call that actually went out.
		for d := o.claim(chat); d > 0; d = o.claim(chat) {
			if err := o.sleep(ctx, d); err != nil {
				return err
			}
		}
		err := o.api.call(ctx, method, params, out)
		var te *tgError
		if attempt < 3 && errors.As(err, &te) && te.Code == http.StatusTooManyRequests && te.RetryAfter > 0 && te.RetryAfter <= 5*time.Minute {
			o.cooldown(chat, te.RetryAfter)
			continue
		}
		return err
	}
}
