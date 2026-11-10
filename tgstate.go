package main

// Telegram bot state and one-time pairing codes, both 0600 files next to the token.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	pairTTL      = 10 * time.Minute
	pairAttempts = 5
	maxMsgRefs   = 500
)

type tgMsgRef struct {
	ID      int64  `json:"id"`
	Session string `json:"session"`
	Pane    string `json:"pane,omitempty"`
}

// tgState is what survives a restart. The pairing code lives in its own file
// (tgPair) so the CLI writing it and the service saving state never clobber
// each other.
type tgState struct {
	ChatID            int64                        `json:"chatId,omitempty"`
	UserID            int64                        `json:"userId,omitempty"`
	Mode              string                       `json:"mode,omitempty"` // forum | private
	ChatTitle         string                       `json:"chatTitle,omitempty"`
	PairedAt          time.Time                    `json:"pairedAt,omitzero"`
	TitlesV2          bool                         `json:"titlesV2,omitempty"`          // topic titles without the status emoji
	NamesV3           bool                         `json:"namesV3,omitempty"`           // existing topics checked for provisional names
	ArchivedDeletedV1 bool                         `json:"archivedDeletedV1,omitempty"` // topics archived by older versions deleted
	IconsV1           bool                         `json:"iconsV1,omitempty"`           // existing topics got their workspace icon
	LauncherIcon      bool                         `json:"launcherIcon,omitempty"`      // the launcher topic has its icon
	Icons             map[string]string            `json:"icons,omitempty"`             // allowed topic icons: emoji (no U+FE0F) -> custom emoji id
	IconsAt           time.Time                    `json:"iconsAt,omitzero"`
	Topics            map[string]*tgTopicRef       `json:"topics,omitempty"`   // live session id -> its forum topic
	Archived          []tgArchived                 `json:"archived,omitempty"` // topics of closed sessions, never reused
	MutedUntil        time.Time                    `json:"mutedUntil,omitzero"`
	Current           string                       `json:"current,omitempty"`        // private mode: session plain text goes to
	MsgRefs           []tgMsgRef                   `json:"msgRefs,omitempty"`        // private mode: bot message -> session, newest last
	LauncherThread    int64                        `json:"launcherThread,omitempty"` // "🚀 Новая сессия" topic
	LauncherMsg       int64                        `json:"launcherMsg,omitempty"`    // its pinned message
	LauncherPinned    bool                         `json:"launcherPinned,omitempty"` // pin confirmed
	Recent            []tgRecentDir                `json:"recentDirs,omitempty"`     // launched folders
	Follow            map[string]*transcriptMirror `json:"follow,omitempty"`         // followed transcripts with their unsent buffers
	MirrorQ           []*mirrorOp                  `json:"mirrorQ,omitempty"`        // mirror messages not sent yet
	MirrorIDs         map[string]int64             `json:"mirrorIds,omitempty"`      // tool lines still being edited
	Mirror            map[string]int64             `json:"mirror,omitempty"`         // transcript path -> bytes already mirrored
	Cards             map[int64]*tgCard            `json:"cards,omitempty"`          // confirmation cards and their tombstones
	Offset            int64                        `json:"offset,omitempty"`         // next getUpdates offset
}

type tgTopicRef struct {
	Thread      int64  `json:"thread"`
	Title       string `json:"title"`                 // the name the topic has now
	Workspace   string `json:"workspace"`             // what the title was built from,
	Name        string `json:"sessionName"`           // kept for the archive title
	Icon        string `json:"icon,omitempty"`        // icon emoji the topic has, "" = none set
	Provisional bool   `json:"provisional,omitempty"` // named after an auto name (folder, program): renamed once a real name settles

	// Archiving progress, so a step that failed is retried, never skipped.
	Closing  bool `json:"closing,omitempty"`  // the session closed; the topic is being archived
	Retitled bool `json:"retitled,omitempty"` // "📦 … (архив)" title set
}

type tgArchived struct {
	Session string    `json:"session"`
	Thread  int64     `json:"thread"`
	At      time.Time `json:"at"`
}

const maxArchived = 1000

func (s *tgState) archive(sid string, thread int64, at time.Time) {
	delete(s.Topics, sid)
	s.Archived = append(s.Archived, tgArchived{sid, thread, at})
	if n := len(s.Archived) - maxArchived; n > 0 {
		s.Archived = append([]tgArchived(nil), s.Archived[n:]...)
	}
}

func (s *tgState) archivedThread(thread int64) bool {
	for _, a := range s.Archived {
		if a.Thread == thread {
			return true
		}
	}
	return false
}

func (s *tgState) paired() bool { return s.ChatID != 0 && s.UserID != 0 }

func loadTGState(path string) (*tgState, error) {
	st := &tgState{}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return st, nil
}

// writeFile0600 replaces a file atomically, readable by the owner only.
func writeFile0600(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *tgState) save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFile0600(path, b)
}

func (s *tgState) addMsgRef(id int64, session, pane string) {
	s.MsgRefs = append(s.MsgRefs, tgMsgRef{id, session, pane})
	if n := len(s.MsgRefs) - maxMsgRefs; n > 0 {
		s.MsgRefs = append([]tgMsgRef(nil), s.MsgRefs[n:]...)
	}
}

func (s *tgState) msgRef(id int64) *tgMsgRef {
	for i := len(s.MsgRefs) - 1; i >= 0; i-- {
		if s.MsgRefs[i].ID == id {
			return &s.MsgRefs[i]
		}
	}
	return nil
}

func (s *tgState) sessionOfThread(thread int64) string {
	for sid, t := range s.Topics {
		if t.Thread == thread && !t.Closing {
			return sid
		}
	}
	return ""
}

/* ---------- pairing ---------- */

type tgPair struct {
	Salt     string    `json:"salt"`
	Hash     string    `json:"hash"` // sha256(salt + code)
	Expires  time.Time `json:"expires"`
	Attempts int       `json:"attempts"` // wrong codes so far
}

func pairHash(salt, code string) string {
	h := sha256.Sum256([]byte(salt + code))
	return hex.EncodeToString(h[:])
}

// withPairLock runs fn holding an exclusive lock shared by the CLI
// (-telegram-pair) and the service, so checking a code and using it up is one
// step even across processes. No lock, no pairing: it fails closed.
func withPairLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("pairing lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("pairing lock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// newPairCode writes a fresh one-time code (replacing any earlier one) and returns it.
func newPairCode(path string, now time.Time) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	code := fmt.Sprintf("%06d", n.Int64())
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	p := tgPair{Salt: hex.EncodeToString(salt), Expires: now.Add(pairTTL)}
	p.Hash = pairHash(p.Salt, code)
	b, _ := json.Marshal(p)
	err = withPairLock(path, func() error { return writeFile0600(path, b) })
	if err != nil {
		return "", err
	}
	return code, nil
}

var (
	errNoPairCode  = errors.New("no pairing code")
	errPairExpired = errors.New("pairing code expired")
	errPairWrong   = errors.New("wrong pairing code")
	errPairBurned  = errors.New("too many wrong codes, pairing code burned")
)

func readPair(path string) (*tgPair, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoPairCode
	}
	if err != nil {
		return nil, err
	}
	var p tgPair
	if json.Unmarshal(b, &p) != nil || p.Hash == "" {
		return nil, errNoPairCode
	}
	return &p, nil
}

// pairPending: a code is waiting to be used (not expired, not burned).
func pairPending(path string, now time.Time) bool {
	p, err := readPair(path)
	return err == nil && now.Before(p.Expires) && p.Attempts < pairAttempts
}

// checkPairCode verifies a code against the pending one. A right code is used
// up, and only counts once it is gone from disk: if it cannot be removed the
// pairing fails, so nobody can replay it. A wrong one counts (the count must be
// written, or it fails too), and the fifth burns the code.
func checkPairCode(path, code string, now time.Time) error {
	return withPairLock(path, func() error {
		p, err := readPair(path)
		if err != nil {
			if errors.Is(err, errNoPairCode) {
				os.Remove(path) // unreadable junk
			}
			return err
		}
		if p.Attempts >= pairAttempts {
			os.Remove(path)
			return errPairBurned
		}
		if !now.Before(p.Expires) {
			os.Remove(path)
			return errPairExpired
		}
		if subtle.ConstantTimeCompare([]byte(pairHash(p.Salt, strings.TrimSpace(code))), []byte(p.Hash)) == 1 {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("cannot use up the pairing code: %w", err)
			}
			return nil
		}
		p.Attempts++
		if p.Attempts >= pairAttempts {
			if err := os.Remove(path); err != nil {
				b, _ := json.Marshal(p) // at least record the burn
				writeFile0600(path, b)
			}
			return errPairBurned
		}
		b, _ := json.Marshal(p)
		if err := writeFile0600(path, b); err != nil {
			return fmt.Errorf("cannot record a wrong pairing code: %w", err)
		}
		return errPairWrong
	})
}
