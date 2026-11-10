package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Ctl runs agtermctl. Every call is a separate process with its own timeout;
// arguments go through exec directly, never through a shell.
type Ctl struct {
	Path    string
	Timeout time.Duration

	idleMu sync.Mutex
	idle   time.Duration // user input idle time agterm reported with the last tree
	idleAt time.Time     // when; zero = never
}

// Idle returns how long the user has not touched the Mac (keyboard, mouse) as
// of the last Tree call, and when that was read.
func (c *Ctl) Idle() (time.Duration, time.Time) {
	c.idleMu.Lock()
	defer c.idleMu.Unlock()
	return c.idle, c.idleAt
}

// CtlError is an error reported by agtermctl itself (bad target, app not running, ...).
type CtlError struct{ Msg string }

func (e *CtlError) Error() string { return e.Msg }

func (c *Ctl) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Path, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), nil
	}
	if ctx.Err() != nil {
		return nil, &CtlError{Msg: "agtermctl timed out"}
	}
	// With --json agtermctl prints {"ok":false,"error":"..."} on stdout.
	var env struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(out.Bytes(), &env) == nil && env.Error != "" {
		return nil, &CtlError{Msg: env.Error}
	}
	msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(errb.String()), "error:"))
	if msg == "" {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			// agtermctl missing or not executable.
			return nil, fmt.Errorf("run agtermctl: %w", err)
		}
		msg = err.Error()
	}
	return nil, &CtlError{Msg: msg}
}

// runJSON runs a --json command and decodes its "result" field into v.
func (c *Ctl) runJSON(ctx context.Context, v any, args ...string) error {
	out, err := c.run(ctx, nil, append(args, "--json")...)
	if err != nil {
		return err
	}
	var env struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return fmt.Errorf("decode agtermctl output: %w", err)
	}
	if !env.OK {
		return &CtlError{Msg: env.Error}
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(env.Result, v)
}

type rawWindow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Open bool   `json:"open"`
}

type rawSession struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Title           string   `json:"title"`
	Cwd             string   `json:"cwd"`
	Status          string   `json:"status"`
	StatusBlink     bool     `json:"statusBlink"`
	StatusChangedAt float64  `json:"statusChangedAt"`
	Foreground      []string `json:"foreground"`
	ForegroundShell string   `json:"foregroundShell"`
	HasSplit        bool     `json:"hasSplit"`
	SplitAxis       string   `json:"splitAxis"`
	Unseen          int      `json:"unseen"`
	Realized        bool     `json:"realized"`
	Active          bool     `json:"active"`
	Scratch         bool     `json:"scratch"`
	RemoteHost      string   `json:"remoteHost"`
}

type rawWorkspace struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Sessions []rawSession `json:"sessions"`
}

// Session is what the browser gets: only the fields the panel shows.
type Session struct {
	ID              string   `json:"id"`
	Window          string   `json:"window"`
	Name            string   `json:"name"`
	Cwd             string   `json:"cwd"`
	Status          string   `json:"status"`
	StatusChangedAt float64  `json:"statusChangedAt,omitempty"`
	Program         string   `json:"program,omitempty"`
	HasSplit        bool     `json:"hasSplit,omitempty"`
	SplitAxis       string   `json:"splitAxis,omitempty"`
	HasScratch      bool     `json:"hasScratch,omitempty"`
	Unseen          int      `json:"unseen,omitempty"`
	Realized        bool     `json:"realized"`
	RemoteHost      string   `json:"remoteHost,omitempty"`
	Foreground      []string `json:"-"`
	ForegroundShell string   `json:"-"`
}

type Workspace struct {
	ID         string    `json:"id"`
	Window     string    `json:"window"`
	WindowName string    `json:"windowName"`
	Name       string    `json:"name"`
	Sessions   []Session `json:"sessions"`
}

func programName(fg []string) string {
	if len(fg) == 0 {
		return ""
	}
	p := fg[0]
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return p
}

func normStatus(s string) string {
	switch s {
	case "active", "blocked", "completed":
		return s
	}
	return "idle"
}

// Tree returns every workspace of every open window.
func (c *Ctl) Tree(ctx context.Context) ([]Workspace, error) {
	var wl struct {
		Windows []rawWindow `json:"windows"`
	}
	if err := c.runJSON(ctx, &wl, "window", "list"); err != nil {
		return nil, err
	}
	out := []Workspace{}
	for _, w := range wl.Windows {
		if !w.Open {
			continue
		}
		var t struct {
			Tree struct {
				IdleMs     *int64         `json:"idleMs"`
				Workspaces []rawWorkspace `json:"workspaces"`
			} `json:"tree"`
		}
		if err := c.runJSON(ctx, &t, "tree", "--window", w.ID); err != nil {
			return nil, err
		}
		if t.Tree.IdleMs != nil {
			c.idleMu.Lock()
			c.idle, c.idleAt = time.Duration(*t.Tree.IdleMs)*time.Millisecond, time.Now()
			c.idleMu.Unlock()
		}
		for _, ws := range t.Tree.Workspaces {
			o := Workspace{ID: ws.ID, Window: w.ID, WindowName: w.Name, Name: ws.Name, Sessions: []Session{}}
			for _, s := range ws.Sessions {
				o.Sessions = append(o.Sessions, Session{
					ID:              s.ID,
					Window:          w.ID,
					Name:            s.Name,
					Cwd:             s.Cwd,
					Status:          normStatus(s.Status),
					StatusChangedAt: s.StatusChangedAt,
					Program:         programName(s.Foreground),
					HasSplit:        s.HasSplit,
					SplitAxis:       s.SplitAxis,
					HasScratch:      s.Scratch,
					Unseen:          s.Unseen,
					Realized:        s.Realized,
					RemoteHost:      s.RemoteHost,
					Foreground:      s.Foreground,
					ForegroundShell: s.ForegroundShell,
				})
			}
			out = append(out, o)
		}
	}
	return out, nil
}

// NewSession runs `session new` with the given arguments and returns the new id.
func (c *Ctl) NewSession(ctx context.Context, args ...string) (string, error) {
	var r struct {
		ID string `json:"id"`
	}
	if err := c.runJSON(ctx, &r, args...); err != nil {
		return "", err
	}
	if !idRe.MatchString(r.ID) {
		return "", fmt.Errorf("session new: unexpected id %q", r.ID)
	}
	return strings.ToUpper(r.ID), nil
}

// Text returns a pane's visible screen, or the last `lines` lines of its buffer when lines > 0.
func (c *Ctl) Text(ctx context.Context, window, session, pane string, lines int) (string, error) {
	args := []string{"session", "text", "--target", session, "--window", window}
	if pane != "" {
		args = append(args, "--pane", pane)
	}
	if lines > 0 {
		args = append(args, "--lines", fmt.Sprint(lines))
	}
	var r struct {
		Text string `json:"text"`
	}
	if err := c.runJSON(ctx, &r, args...); err != nil {
		return "", err
	}
	return r.Text, nil
}

// Type injects raw bytes as keystrokes. --stdin keeps text that starts with "-" from being read as an option.
func (c *Ctl) Type(ctx context.Context, window, session, pane, text string) error {
	args := []string{"session", "type", "--stdin", "--target", session, "--window", window}
	if pane != "" {
		args = append(args, "--pane", pane)
	}
	_, err := c.run(ctx, []byte(text), append(args, "--json")...)
	return err
}
