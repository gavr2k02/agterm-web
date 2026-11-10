package main

import (
	"bufio"
	"context"
	"log"
	"os/exec"
	"sync"
	"time"
)

// eventKinds are the agterm events the panel reacts to.
const eventKinds = "status,notify,session.created,session.closed,tree.changed,pane.split,pane.scratch"

// hub runs one `agtermctl events` process while at least one browser is listening
// and fans its lines out to all of them. A slow listener drops lines instead of
// blocking the others; the browser refetches the tree on any event anyway.
type hub struct {
	ctlPath string
	start   func(ctx context.Context, out chan<- string) // runs until ctx ends

	mu     sync.Mutex
	subs   map[chan string]struct{}
	cancel context.CancelFunc
}

func newHub(ctlPath string) *hub {
	h := &hub{ctlPath: ctlPath, subs: map[chan string]struct{}{}}
	h.start = h.runEvents
	return h
}

func (h *hub) subscribe() (<-chan string, func()) {
	ch := make(chan string, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	if h.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		out := make(chan string, 64)
		go h.start(ctx, out)
		go h.fanout(ctx, out)
	}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			if len(h.subs) == 0 && h.cancel != nil {
				h.cancel()
				h.cancel = nil
			}
			h.mu.Unlock()
		})
	}
}

func (h *hub) fanout(ctx context.Context, in <-chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case line := <-in:
			h.mu.Lock()
			for ch := range h.subs {
				select {
				case ch <- line:
				default:
				}
			}
			h.mu.Unlock()
		}
	}
}

// runEvents keeps `agtermctl events --json` running until ctx ends, restarting it
// after a pause when it exits (agterm restarted, cursor lost, ...). Each restart is
// announced as a synthetic "resync" event so the browser refetches everything.
func (h *hub) runEvents(ctx context.Context, out chan<- string) {
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, h.ctlPath, "events", "--json", "--kind", eventKinds)
		stdout, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			log.Printf("events: %v", err)
		} else {
			send(ctx, out, `{"kind":"resync"}`)
			sc := bufio.NewScanner(stdout)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				send(ctx, out, sc.Text())
			}
			if err := cmd.Wait(); err != nil && ctx.Err() == nil {
				log.Printf("events exited: %v", err)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

func send(ctx context.Context, out chan<- string, line string) {
	select {
	case out <- line:
	case <-ctx.Done():
	}
}
