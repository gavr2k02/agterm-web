// agterm-web: a small mobile web panel for agterm sessions.
//
// It listens on localhost only and is meant to be published to your own devices with
// `tailscale serve`. Every request also needs the access token (cookie set from a ?t= link).
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

const (
	cookieName  = "aw_token"
	maxTypeLen  = 16 << 10
	maxLines    = 2000
	enterDelay  = 200 * time.Millisecond
	pasteStart  = "\x1b[200~"
	pasteEnd    = "\x1b[201~"
	csrfHeader  = "X-Agterm-Web"
	defaultAddr = "127.0.0.1:7681"
)

// Only full UUIDs (as tree --json reports them): a prefix or another spelling of the
// same id would otherwise take a different per-pane write lock.
var idRe = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

type server struct {
	ctl    *Ctl
	limits *LimitsSource
	token  string
	hub    *hub
	sleep  func(time.Duration)
	// typeMu serializes keystroke writes per pane, so two submits never interleave
	// their text and their delayed Enter.
	typeMu sync.Map // "window/session/pane" -> *sync.Mutex
	// written marks sessions anything was ever typed into, so the launcher can
	// tell a brand-new session that someone else already used.
	written sync.Map // SESSION ID -> struct{}
}

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", defaultAddr, "listen address (keep it on localhost; publish with tailscale serve)")
	ctlPath := flag.String("agtermctl", defaultCtlPath(), "path to agtermctl")
	tokenFile := flag.String("token-file", filepath.Join(home, ".config", "agterm-web", "token"), "access token file (created on first run)")
	printToken := flag.Bool("print-token", false, "print the access token and exit")
	cswapPath := flag.String("cswap", defaultCswapPath(), "path to cswap (claude-swap), for Claude usage limits")
	codexHome := flag.String("codex-home", defaultCodexHome(), "Codex home folder (its sessions/ hold the rate limits)")
	limitsJSON := flag.Bool("limits-json", false, "print the usage limits as JSON and exit (for the menu bar plugin)")
	limitsMenu := flag.Bool("limits-swiftbar", false, "print the usage limits as a SwiftBar menu and exit")
	tgTokenFile := flag.String("telegram-token-file", filepath.Join(home, ".config", "agterm-web", "telegram-token"), "Telegram bot token file (0600); the bot runs iff it exists")
	tgAPI := flag.String("telegram-api", defaultTelegramAPI, "Telegram Bot API base URL")
	ffmpegPath := flag.String("ffmpeg", "", "ffmpeg for voice and video (default: /opt/homebrew/bin/ffmpeg or PATH)")
	whisperPath := flag.String("whisper-cli", "", "whisper.cpp CLI for speech to text (default: /opt/homebrew/bin/whisper-cli or PATH)")
	whisperModel := flag.String("whisper-model", "", "whisper model (default: ~/.local/share/whisper/ggml-large-v3-turbo.bin)")
	tgPair := flag.Bool("telegram-pair", false, "print a one-time code to pair the Telegram bot with a chat (/pair <code>) and exit")
	flag.Parse()

	if *tgPair {
		code, err := newPairCode(tgPairPath(*tgTokenFile), time.Now())
		if err != nil {
			log.Fatalf("telegram pairing: %v", err)
		}
		fmt.Printf("Код: %s (действует %d минут, один раз).\nОтправь боту в личку или в группу с темами: /pair %s\n", code, int(pairTTL/time.Minute), code)
		if _, ok, err := loadTGToken(*tgTokenFile); err != nil || !ok {
			fmt.Printf("Внимание: бот не запустится без токена в %s (chmod 600).\n", *tgTokenFile)
		}
		return
	}

	limits := NewLimitsSource(*cswapPath, *codexHome)
	limits.BotTokenFile, limits.BotHealthFile = *tgTokenFile, tgHealthPath(*tgTokenFile)
	if *limitsMenu {
		fmt.Print(SwiftBarMenu(limits.Collect(context.Background()), time.Now()))
		return
	}
	if *limitsJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(limits.Collect(context.Background())); err != nil {
			log.Fatal(err)
		}
		return
	}

	token, err := loadOrCreateToken(*tokenFile)
	if err != nil {
		log.Fatalf("token: %v", err)
	}
	if *printToken {
		fmt.Println(token)
		return
	}

	ctl := &Ctl{Path: *ctlPath, Timeout: 8 * time.Second}
	s := &server{ctl: ctl, limits: limits, token: token, hub: newHub(ctl.Path), sleep: time.Sleep}

	if tok, ok, err := loadTGToken(*tgTokenFile); err != nil {
		log.Printf("telegram: %v; bot disabled", err)
	} else if ok {
		bot, err := newTGBot(newTGClient(*tgAPI, tok), s, *tgTokenFile)
		if err != nil {
			log.Printf("telegram: %v; bot disabled", err)
		} else {
			if *ffmpegPath != "" {
				bot.tools.FFmpeg = *ffmpegPath
			}
			if *whisperPath != "" {
				bot.tools.Whisper = *whisperPath
			}
			if *whisperModel != "" {
				bot.tools.WhisperModel = *whisperModel
			}
			log.Printf("telegram bot enabled")
			go bot.run(context.Background())
		}
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("agterm-web listening on http://%s (agtermctl: %s)", *addr, *ctlPath)
	log.Fatal(srv.ListenAndServe())
}

func defaultCtlPath() string {
	for _, p := range []string{"/opt/homebrew/bin/agtermctl", "/usr/local/bin/agtermctl", "/Applications/agterm.app/Contents/MacOS/agtermctl"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "agtermctl"
}

func loadOrCreateToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		t := strings.TrimSpace(string(b))
		if len(t) < 32 {
			return "", fmt.Errorf("%s: token too short", path)
		}
		return t, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

func (s *server) routes() http.Handler {
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(static))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tree", s.handleTree)
	mux.HandleFunc("GET /api/text", s.handleText)
	mux.HandleFunc("POST /api/type", s.handleType)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/limits", s.handleLimits)
	mux.Handle("GET /", files)

	return securityHeaders(s.auth(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; manifest-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *server) tokenOK(t string) bool {
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1
}

// auth: a ?t=<token> link sets a long-lived cookie and redirects to a clean URL;
// everything else needs that cookie. Mutating requests also need a custom header,
// which a cross-site form or image cannot send.
func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("t"); t != "" {
			if !s.tokenOK(t) {
				http.Error(w, "wrong token", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    s.token,
				Path:     "/",
				MaxAge:   365 * 24 * 3600,
				HttpOnly: true,
				Secure:   true,
				SameSite: http.SameSiteStrictMode,
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil || !s.tokenOK(c.Value) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, unauthorizedPage)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) != "1" {
			writeErr(w, http.StatusForbidden, "missing "+csrfHeader+" header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

const unauthorizedPage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>agterm</title><body style="font:16px system-ui;padding:24px;background:#111;color:#ddd">
<p>Нужна ссылка с токеном. На Mac выполни <code>agterm-web -print-token</code> и открой
<code>https://&lt;mac&gt;.ts.net/?t=&lt;токен&gt;</code>.</p></body>`

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func ctlErr(w http.ResponseWriter, err error) {
	var ce *CtlError
	if errors.As(err, &ce) {
		writeErr(w, http.StatusBadGateway, ce.Msg)
		return
	}
	log.Printf("agtermctl: %v", err)
	writeErr(w, http.StatusInternalServerError, "agtermctl failed")
}

func validPane(p string) bool {
	switch p {
	case "", "left", "right", "scratch":
		return true
	}
	return false
}

func (s *server) handleTree(w http.ResponseWriter, r *http.Request) {
	tree, err := s.ctl.Tree(r.Context())
	if err != nil {
		ctlErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"workspaces": tree})
}

func (s *server) handleText(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	win, sess, pane := q.Get("window"), q.Get("session"), q.Get("pane")
	if !idRe.MatchString(win) || !idRe.MatchString(sess) || !validPane(pane) {
		writeErr(w, http.StatusBadRequest, "bad window, session or pane")
		return
	}
	lines := 0
	if l := q.Get("lines"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxLines {
			writeErr(w, http.StatusBadRequest, "bad lines")
			return
		}
		lines = n
	}
	text, err := s.ctl.Text(r.Context(), win, sess, pane, lines)
	if err != nil {
		ctlErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"text": text})
}

type typeReq struct {
	Window  string `json:"window"`
	Session string `json:"session"`
	Pane    string `json:"pane"`
	Text    string `json:"text"`
	// Mode: "keys" sends text as is (quick keys), "insert" puts text on the prompt line,
	// "submit" puts it there and presses Enter.
	Mode string `json:"mode"`
}

// typeChunks turns a request into the keystroke writes to send, in order.
// Multi-line text goes in as a bracketed paste, so its newlines do not submit
// half a prompt; Enter is a separate write sent after a pause, because agents
// that detect pastes by timing would otherwise take it as part of the paste.
func typeChunks(mode, text string) ([]string, error) {
	switch mode {
	case "keys":
		if text == "" {
			return nil, errors.New("empty keys")
		}
		return []string{text}, nil
	case "insert", "submit":
	default:
		return nil, errors.New("bad mode")
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var chunks []string
	if text != "" {
		if strings.Contains(text, "\n") {
			// A paste-end marker inside the text would close the paste early.
			text = strings.ReplaceAll(text, pasteEnd, "")
			chunks = append(chunks, pasteStart+text+pasteEnd)
		} else {
			chunks = append(chunks, text)
		}
	}
	if mode == "submit" {
		chunks = append(chunks, "\r")
	}
	if len(chunks) == 0 {
		return nil, errors.New("nothing to send")
	}
	return chunks, nil
}

func (s *server) handleType(w http.ResponseWriter, r *http.Request) {
	var req typeReq
	// JSON escaping can take up to 6 bytes per input byte (\u001b).
	dec := json.NewDecoder(io.LimitReader(r.Body, maxTypeLen*6+4096))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if !idRe.MatchString(req.Window) || !idRe.MatchString(req.Session) || !validPane(req.Pane) {
		writeErr(w, http.StatusBadRequest, "bad window, session or pane")
		return
	}
	if len(req.Text) > maxTypeLen {
		writeErr(w, http.StatusRequestEntityTooLarge, "text too long")
		return
	}
	if strings.ContainsRune(req.Text, 0) {
		writeErr(w, http.StatusBadRequest, "text contains NUL")
		return
	}
	chunks, err := typeChunks(req.Mode, req.Text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.typeInto(r.Context(), req.Window, req.Session, req.Pane, chunks); err != nil {
		ctlErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// typeInto sends keystroke chunks to a pane, holding that pane's write lock so
// two senders (browser, Telegram) never interleave their text and delayed Enter.
func (s *server) typeInto(ctx context.Context, window, session, pane string, chunks []string) error {
	mu := s.paneLock(window, session, pane)
	mu.Lock()
	defer mu.Unlock()
	return s.typeLocked(ctx, window, session, pane, chunks)
}

func (s *server) wasWritten(session string) bool {
	_, ok := s.written.Load(strings.ToUpper(session))
	return ok
}

// paneLock is the write lock of one pane (window/session/pane, any spelling).
func (s *server) paneLock(window, session, pane string) *sync.Mutex {
	if pane == "" {
		pane = "left"
	}
	m, _ := s.typeMu.LoadOrStore(strings.ToUpper(window+"/"+session+"/"+pane), &sync.Mutex{})
	return m.(*sync.Mutex)
}

// typeLocked sends the chunks; the caller holds paneLock. It stops at the
// first failure and never resends what went out.
func (s *server) typeLocked(ctx context.Context, window, session, pane string, chunks []string) error {
	if pane == "" {
		pane = "left"
	}
	s.written.Store(strings.ToUpper(session), struct{}{})
	for i, c := range chunks {
		if i > 0 {
			s.sleep(enterDelay)
		}
		if err := s.ctl.Type(ctx, window, session, pane, c); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) handleLimits(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.limits.Get(r.Context()))
}

// handleEvents streams agterm control events as Server-Sent Events.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "retry: 3000\n\n")
	fl.Flush()

	ch, cancel := s.hub.subscribe()
	defer cancel()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			fl.Flush()
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
			fl.Flush()
		}
	}
}
