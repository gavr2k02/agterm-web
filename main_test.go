package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeCtl writes a shell script standing in for agtermctl: it logs its argv and
// stdin to files and prints canned JSON.
func fakeCtl(t *testing.T) (path, logDir string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
d="` + dir + `"
n=$(ls "$d" | grep -c 'args$')
printf '%s\n' "$@" > "$d/call$n.args"
case "$1 $2" in
  "window list") echo '{"ok":true,"result":{"windows":[{"id":"AAAAAAAA-1111-1111-1111-111111111111","name":"window 1","open":true},{"id":"BBBBBBBB-2222-2222-2222-222222222222","name":"w2","open":false}]}}' ;;
  "tree --window") echo '{"ok":true,"result":{"tree":{"workspaces":[{"id":"CCCC3333","name":"EM","sessions":[{"id":"DDDDDDDD-4444-4444-4444-444444444444","name":"◐ Fix bug","cwd":"/x","status":"blocked","foreground":["/usr/bin/claude","--resume"],"realized":true,"unseen":2}]}]}}}' ;;
  "session text") echo '{"ok":true,"result":{"text":"hello"}}' ;;
  "session type") cat > "$d/call$n.stdin"; echo '{"ok":true}' ;;
  *) echo '{"ok":false,"error":"no such session: x"}'; exit 1 ;;
esac
`
	p := filepath.Join(t.TempDir(), "agtermctl")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p, dir
}

func newTestServer(t *testing.T) (*server, string) {
	p, dir := fakeCtl(t)
	tmp := t.TempDir()
	limits := NewLimitsSource(filepath.Join(tmp, "no-cswap"), filepath.Join(tmp, "no-codex"))
	return &server{ctl: &Ctl{Path: p, Timeout: 5 * time.Second}, limits: limits, token: testToken, hub: newHub(p), sleep: func(time.Duration) {}}, dir
}

func authed(r *http.Request) *http.Request {
	r.AddCookie(&http.Cookie{Name: cookieName, Value: testToken})
	return r
}

func TestAuth(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.routes()

	cases := []struct {
		name string
		req  *http.Request
		code int
	}{
		{"no cookie api", httptest.NewRequest("GET", "/api/tree", nil), 401},
		{"no cookie page", httptest.NewRequest("GET", "/", nil), 401},
		{"wrong cookie", func() *http.Request {
			r := httptest.NewRequest("GET", "/api/tree", nil)
			r.AddCookie(&http.Cookie{Name: cookieName, Value: strings.Repeat("0", 64)})
			return r
		}(), 401},
		{"wrong link token", httptest.NewRequest("GET", "/?t=nope", nil), 401},
		{"good link token", httptest.NewRequest("GET", "/?t="+testToken, nil), 303},
		{"post without csrf header", authed(httptest.NewRequest("POST", "/api/type", strings.NewReader(`{}`))), 403},
		{"authed tree", authed(httptest.NewRequest("GET", "/api/tree", nil)), 200},
		{"no cookie limits", httptest.NewRequest("GET", "/api/limits", nil), 401},
		{"authed limits", authed(httptest.NewRequest("GET", "/api/limits", nil)), 200},
		{"authed page", authed(httptest.NewRequest("GET", "/", nil)), 200},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, c.req)
		if w.Code != c.code {
			t.Errorf("%s: got %d, want %d (%s)", c.name, w.Code, c.code, w.Body.String())
		}
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/?t="+testToken, nil))
	ck := w.Result().Cookies()
	if len(ck) != 1 || ck[0].Value != testToken || !ck[0].HttpOnly || !ck[0].Secure || ck[0].SameSite != http.SameSiteStrictMode {
		t.Errorf("bad cookie: %+v", ck)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("redirect to %q, want /", loc)
	}
}

func TestTree(t *testing.T) {
	s, _ := newTestServer(t)
	tree, err := s.ctl.Tree(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 1 || len(tree[0].Sessions) != 1 {
		t.Fatalf("closed window must be skipped: %+v", tree)
	}
	got := tree[0].Sessions[0]
	if got.Window != "AAAAAAAA-1111-1111-1111-111111111111" || got.Status != "blocked" || got.Program != "claude" || got.Unseen != 2 {
		t.Errorf("unexpected session: %+v", got)
	}
}

func TestTypeChunks(t *testing.T) {
	cases := []struct {
		mode, text string
		want       []string
		err        bool
	}{
		{"keys", "\x1b", []string{"\x1b"}, false},
		{"keys", "", nil, true},
		{"submit", "hi", []string{"hi", "\r"}, false},
		{"submit", "", []string{"\r"}, false},
		{"insert", "hi", []string{"hi"}, false},
		{"insert", "", nil, true},
		{"submit", "a\r\nb", []string{pasteStart + "a\nb" + pasteEnd, "\r"}, false},
		{"insert", "a\nb" + pasteEnd + "c", []string{pasteStart + "a\nbc" + pasteEnd}, false},
		{"bogus", "x", nil, true},
	}
	for _, c := range cases {
		got, err := typeChunks(c.mode, c.text)
		if (err != nil) != c.err || !reflect.DeepEqual(got, c.want) {
			t.Errorf("typeChunks(%q,%q) = %q,%v; want %q, err=%v", c.mode, c.text, got, err, c.want, c.err)
		}
	}
}

func TestTypeHandler(t *testing.T) {
	s, dir := newTestServer(t)
	h := s.routes()
	post := func(body string) *httptest.ResponseRecorder {
		r := authed(httptest.NewRequest("POST", "/api/type", strings.NewReader(body)))
		r.Header.Set(csrfHeader, "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post(`{"window":"AAAAAAAA-1111-1111-1111-111111111111","session":"DDDDDDDD-4444-4444-4444-444444444444","text":"--help","mode":"submit"}`); w.Code != 200 {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	b0, _ := os.ReadFile(filepath.Join(dir, "call0.stdin"))
	b1, _ := os.ReadFile(filepath.Join(dir, "call1.stdin"))
	if string(b0) != "--help" || string(b1) != "\r" {
		t.Errorf("stdin = %q, %q", b0, b1)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "call0.args"))
	if !strings.Contains(string(args), "--stdin\n") || !strings.Contains(string(args), "--window\nAAAAAAAA-1111-1111-1111-111111111111\n") {
		t.Errorf("args: %q", args)
	}

	bad := []string{
		`{"window":"AAAAAAAA-1111-1111-1111-111111111111","session":"--target","text":"x","mode":"keys"}`,
		`{"window":"AAAAAAAA-1111-1111-1111-111111111111","session":"DDDDDDDD-4444-4444-4444-444444444444","pane":"overlay","text":"x","mode":"keys"}`,
		`{"window":"AAAAAAAA-1111-1111-1111-111111111111","session":"DDDDDDDD-4444-4444-4444-444444444444","text":"a\u0000b","mode":"keys"}`,
		`{"window":"AAAAAAAA-1111-1111-1111-111111111111","session":"DDDDDDDD-4444-4444-4444-444444444444","text":"` + strings.Repeat("x", maxTypeLen+1) + `","mode":"keys"}`,
		`not json`,
	}
	for _, b := range bad {
		if w := post(b); w.Code < 400 || w.Code >= 500 {
			t.Errorf("expected 4xx for %.60s: %d", b, w.Code)
		}
	}
}

func TestTextHandler(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.routes()
	for path, code := range map[string]int{
		"/api/text?window=AAAAAAAA-1111-1111-1111-111111111111&session=DDDDDDDD-4444-4444-4444-444444444444":           200,
		"/api/text?window=AAAAAAAA-1111-1111-1111-111111111111&session=DDDDDDDD-4444-4444-4444-444444444444&lines=300": 200,
		"/api/text?window=AAAAAAAA-1111-1111-1111-111111111111&session=DDDDDDDD-4444-4444-4444-444444444444&lines=0":   400,
		"/api/text?window=AAAAAAAA-1111-1111-1111-111111111111&session=-x":                                             400,
		"/api/text?window=AAAAAAAA-1111-1111-1111-111111111111&session=EEEEEEEE-5555-5555-5555-555555555555&pane=left": 200,
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, authed(httptest.NewRequest("GET", path, nil)))
		if w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
}

func TestCtlErrorSurfaced(t *testing.T) {
	s, _ := newTestServer(t)
	err := s.ctl.runJSON(context.Background(), nil, "session", "close")
	if ce, ok := err.(*CtlError); !ok || ce.Msg != "no such session: x" {
		t.Errorf("got %v", err)
	}
}

func TestHubStopsWhenIdle(t *testing.T) {
	h := &hub{subs: map[chan string]struct{}{}}
	started, stopped := make(chan struct{}, 4), make(chan struct{}, 4)
	h.start = func(ctx context.Context, out chan<- string) {
		started <- struct{}{}
		out <- "x"
		<-ctx.Done()
		stopped <- struct{}{}
	}
	a, ca := h.subscribe()
	b, cb := h.subscribe()
	<-started
	for _, ch := range []<-chan string{a, b} {
		select {
		case l := <-ch:
			if l != "x" {
				t.Fatal(l)
			}
		case <-time.After(time.Second):
			t.Fatal("no fanout")
		}
	}
	ca()
	ca() // idempotent
	select {
	case <-stopped:
		t.Fatal("stopped with a subscriber left")
	case <-time.After(50 * time.Millisecond):
	}
	cb()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("not stopped after last unsubscribe")
	}
	select {
	case <-started:
		t.Fatal("started twice")
	default:
	}
}

func TestLoadOrCreateToken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "token")
	a, err := loadOrCreateToken(p)
	if err != nil || len(a) != 64 {
		t.Fatal(a, err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("perm %v", st.Mode().Perm())
	}
	b, _ := loadOrCreateToken(p)
	if a != b {
		t.Error("token changed")
	}
}

func TestConcurrentSubmitsDoNotInterleave(t *testing.T) {
	s, dir := newTestServer(t)
	s.sleep = func(time.Duration) { time.Sleep(30 * time.Millisecond) }
	h := s.routes()
	post := func(session, text string, done chan<- int) {
		r := authed(httptest.NewRequest("POST", "/api/type", strings.NewReader(`{"window":"AAAAAAAA-1111-1111-1111-111111111111","session":"`+session+`","text":"`+text+`","mode":"submit"}`)))
		r.Header.Set(csrfHeader, "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		done <- w.Code
	}
	done := make(chan int, 2)
	// Two spellings of the same session must share one write lock.
	go post("DDDDDDDD-4444-4444-4444-444444444444", "first", done)
	go post("dddddddd-4444-4444-4444-444444444444", "second", done)
	for i := 0; i < 2; i++ {
		if c := <-done; c != 200 {
			t.Fatalf("code %d", c)
		}
	}
	var seq []string
	for i := 0; i < 4; i++ {
		b, err := os.ReadFile(filepath.Join(dir, "call"+strconv.Itoa(i)+".stdin"))
		if err != nil {
			t.Fatal(err)
		}
		seq = append(seq, string(b))
	}
	ok := reflect.DeepEqual(seq, []string{"first", "\r", "second", "\r"}) ||
		reflect.DeepEqual(seq, []string{"second", "\r", "first", "\r"})
	if !ok {
		t.Errorf("interleaved writes: %q", seq)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "call0.args"))
	if !strings.Contains(string(args), "--pane\nleft\n") {
		t.Errorf("pane must default to left explicitly: %q", args)
	}
}

func TestTypeLimitCountsDecodedText(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.routes()
	body := `{"window":"AAAAAAAA-1111-1111-1111-111111111111","session":"DDDDDDDD-4444-4444-4444-444444444444","text":"` + strings.Repeat(`\t`, maxTypeLen) + `","mode":"keys"}`
	r := authed(httptest.NewRequest("POST", "/api/type", strings.NewReader(body)))
	r.Header.Set(csrfHeader, "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("16 KiB of escaped tabs: %d %s", w.Code, w.Body.String())
	}
}
