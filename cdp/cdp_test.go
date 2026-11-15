package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUserDataDirFollowsChromePerOS(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	cases := []struct {
		goos    string
		channel Channel
		vars    map[string]string
		want    string
	}{
		{"darwin", Stable, nil, filepath.Join("/h", "Library", "Application Support", "Google", "Chrome")},
		{"darwin", Canary, nil, filepath.Join("/h", "Library", "Application Support", "Google", "Chrome Canary")},
		{"windows", "", map[string]string{"LOCALAPPDATA": "/lad"}, filepath.Join("/lad", "Google", "Chrome", "User Data")},
		{"windows", Canary, nil, filepath.Join("/h", "AppData", "Local", "Google", "Chrome SxS", "User Data")},
		{"linux", Stable, nil, filepath.Join("/h", ".config", "google-chrome")},
		{"linux", Dev, map[string]string{"XDG_CONFIG_HOME": "/xdg"}, filepath.Join("/xdg", "google-chrome-unstable")},
		{"linux", Beta, nil, filepath.Join("/h", ".config", "google-chrome-beta")},
	}
	for _, c := range cases {
		got, err := userDataDir(c.goos, c.channel, "/h", env(c.vars))
		if err != nil || got != c.want {
			t.Errorf("%s/%s = %q, %v; want %q", c.goos, c.channel, got, err, c.want)
		}
	}
	if _, err := userDataDir("linux", "nightly", "/h", env(nil)); err == nil {
		t.Error("unknown channel accepted")
	}
}

func TestReadActivePort(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadActivePort(dir); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("without the file = %v, want ErrNotEnabled", err)
	}
	// Chrome writes the port, a newline, the path; tolerate CRLF and blank
	// lines.
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("9222\r\n\r\n/devtools/browser/abc-123\r\n")
	got, err := ReadActivePort(dir)
	if err != nil || got != "ws://127.0.0.1:9222/devtools/browser/abc-123" {
		t.Fatalf("address = %q, %v", got, err)
	}
	for _, bad := range []string{"", "9222\n", "port\n/devtools/browser/x\n", "70000\n/devtools/browser/x\n", "9222\ndevtools\n"} {
		write(bad)
		if _, err := ReadActivePort(dir); err == nil {
			t.Errorf("malformed file %q accepted", bad)
		}
	}
}

func TestConnectThroughDevToolsActivePort(t *testing.T) {
	f := newFakeChrome(t)
	b := f.connect(t)
	if b.Endpoint() != f.wsURL() {
		t.Errorf("connected to %s, want %s", b.Endpoint(), f.wsURL())
	}
	v, err := b.Version(context.Background())
	if err != nil || v.Product != "Chrome/144.0.0.0" {
		t.Fatalf("version = %+v, %v", v, err)
	}
	// With auto-connect every HTTP probe would be one more prompt for the
	// user, so discovery must go straight to the websocket.
	if f.probes != 0 {
		t.Errorf("unexpected HTTP requests to /json: %d", f.probes)
	}
}

func TestExplicitAddressOverridesDiscovery(t *testing.T) {
	f := newFakeChrome(t)
	empty := t.TempDir() // a profile with no DevToolsActivePort: must not be consulted

	for _, address := range []string{f.wsURL(), "http://127.0.0.1:" + f.port(), "127.0.0.1:" + f.port()} {
		b, err := Connect(context.Background(), Options{URL: address, UserDataDir: empty})
		if err != nil {
			t.Fatalf("%s: %v", address, err)
		}
		if b.Endpoint() != f.wsURL() {
			t.Errorf("%s gave %s", address, b.Endpoint())
		}
		_ = b.Close()
	}

	t.Setenv(EnvURL, f.wsURL())
	b, err := Connect(context.Background(), Options{UserDataDir: empty})
	if err != nil {
		t.Fatalf("address from %s: %v", EnvURL, err)
	}
	_ = b.Close()
}

func TestNoChromeSaysWhatToSwitchOn(t *testing.T) {
	_, err := Connect(context.Background(), Options{UserDataDir: t.TempDir()})
	if !errors.Is(err, ErrNotEnabled) || !strings.Contains(err.Error(), "chrome://inspect/#remote-debugging") {
		t.Fatalf("without Chrome = %v", err)
	}

	// A file left behind by a Chrome that is gone points at a closed port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte(port+"\n/devtools/browser/x\n"), 0o600)
	if _, err := Connect(context.Background(), Options{UserDataDir: dir}); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("closed port = %v", err)
	}
}

func TestDeniedConnection(t *testing.T) {
	f := newFakeChrome(t)
	f.gate = func(w http.ResponseWriter, _ *http.Request) bool {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	_, err := Connect(context.Background(), Options{UserDataDir: f.profile(t)})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("refusal = %v", err)
	}

	// Dropping the connection without an answer reads the same way.
	f.gate = func(w http.ResponseWriter, _ *http.Request) bool {
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
		return false
	}
	if _, err := Connect(context.Background(), Options{UserDataDir: f.profile(t)}); !errors.Is(err, ErrDenied) {
		t.Fatalf("dropped connection = %v", err)
	}
}

func TestWaitsForTheUserToAllow(t *testing.T) {
	defer func(d time.Duration) { awaitingNotice = d }(awaitingNotice)
	awaitingNotice = 20 * time.Millisecond

	// Chrome holds the handshake while its prompt is on screen.
	f := newFakeChrome(t)
	f.gate = func(http.ResponseWriter, *http.Request) bool {
		time.Sleep(300 * time.Millisecond)
		return true
	}
	var told atomic.Bool
	b, err := Connect(context.Background(), Options{
		UserDataDir: f.profile(t), ApprovalTimeout: 5 * time.Second,
		OnAwaitingApproval: func() { told.Store(true) },
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	if !told.Load() {
		t.Error("the user was not told that Chrome is waiting for approval")
	}

	// And if nobody answers, that is what the error says.
	f.gate = func(_ http.ResponseWriter, r *http.Request) bool {
		<-r.Context().Done()
		return false
	}
	_, err = Connect(context.Background(), Options{UserDataDir: f.profile(t), ApprovalTimeout: 200 * time.Millisecond})
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("no answer = %v", err)
	}
}

func TestPageReusesAnOpenTab(t *testing.T) {
	f := newFakeChrome(t)
	f.tabs = []Tab{
		{ID: "A", Type: "page", URL: "https://example.com/"},
		{ID: "B", Type: "page", URL: "https://web.telegram.org/a/"},
		{ID: "C", Type: "page", URL: "https://web.telegram.org/k/#@durov"},
	}
	b := f.connect(t)
	page, err := b.Page(context.Background(), Telegram)
	if err != nil {
		t.Fatal(err)
	}
	if page.TargetID() != "C" {
		t.Errorf("picked tab %s, want C (web.telegram.org/k)", page.TargetID())
	}
	for _, m := range f.methods() {
		if m == "Target.createTarget" {
			t.Error("opened a new tab although a matching one exists")
		}
	}
}

func TestPageOpensATabWhenNoneMatches(t *testing.T) {
	f := newFakeChrome(t)
	f.tabs = []Tab{{ID: "A", Type: "page", URL: "https://example.com/"}}
	b := f.connect(t)
	page, err := b.Page(context.Background(), Instagram)
	if err != nil {
		t.Fatal(err)
	}
	if page.TargetID() != "T2" || f.tabs[1].URL != Instagram.URL {
		t.Errorf("opened %+v, page %s", f.tabs, page.TargetID())
	}
}

func TestSiteMatching(t *testing.T) {
	for url, want := range map[string]bool{
		"https://web.telegram.org/k/":             true,
		"https://web.telegram.org/k/#-100123":     true,
		"https://web.telegram.org/a/":             false,
		"https://evil.example/web.telegram.org/k": false,
	} {
		if Telegram.Match(url) != want {
			t.Errorf("Telegram.Match(%q) != %v", url, want)
		}
	}
	for url, want := range map[string]bool{
		"https://www.instagram.com/nasa/": true,
		"https://instagram.com/":          true,
		"https://help.instagram.com/":     false,
	} {
		if Instagram.Match(url) != want {
			t.Errorf("Instagram.Match(%q) != %v", url, want)
		}
	}
}

func openPage(t *testing.T, f *fakeChrome) *Page {
	t.Helper()
	f.tabs = []Tab{{ID: "P", Type: "page", URL: "https://web.telegram.org/k/"}}
	page, err := f.connect(t).Page(context.Background(), Telegram)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestEvalCarriesTheArgumentAndReturnsTheValue(t *testing.T) {
	f := newFakeChrome(t)
	f.on("Runtime.evaluate", func(c *fakeConn, req fakeRequest) {
		if req.SessionID != "S-P" {
			t.Errorf("evaluate was not sent to the tab session: %q", req.SessionID)
		}
		// The argument travels inside the expression, encoded as JSON.
		if expr := expressionOf(t, req); expr != `(async (p) => p)({"op":"whoami"})` {
			t.Errorf("expression = %s", expr)
		}
		c.reply(req, evalValue(map[string]any{"ok": true, "value": map[string]any{"ok": true, "my_id": "42"}}))
	})
	page := openPage(t, f)

	var got struct {
		OK   bool   `json:"ok"`
		MyID string `json:"my_id"`
	}
	if err := page.Eval(context.Background(), "async (p) => p", map[string]any{"op": "whoami"}, &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.MyID != "42" {
		t.Fatalf("got %+v", got)
	}
}

func TestAThrownErrorComesBackAsAnError(t *testing.T) {
	f := newFakeChrome(t)
	f.on("Runtime.evaluate", func(c *fakeConn, req fakeRequest) {
		if strings.Contains(expressionOf(t, req), "syntax") {
			// What the wrapper cannot catch: the expression does not parse.
			c.reply(req, map[string]any{
				"result": map[string]any{"type": "object", "subtype": "error"},
				"exceptionDetails": map[string]any{
					"text":      "Uncaught",
					"exception": map[string]any{"description": "SyntaxError: Unexpected token ')'\n    at <anonymous>:1:1"},
				},
			})
			return
		}
		c.reply(req, evalValue(map[string]any{"ok": false, "error": "FLOOD_WAIT_17"}))
	})
	page := openPage(t, f)

	err := page.EvalExpr(context.Background(), "boom()", nil)
	if err == nil || !strings.Contains(err.Error(), "FLOOD_WAIT_17") {
		t.Fatalf("page error = %v", err)
	}
	err = page.EvalExpr(context.Background(), "syntax)", nil)
	if err == nil || !strings.Contains(err.Error(), "SyntaxError: Unexpected token ')'") || strings.Contains(err.Error(), "anonymous") {
		t.Fatalf("syntax error = %v", err)
	}
}

func TestUndefinedAndNullDecodeToNothing(t *testing.T) {
	f := newFakeChrome(t)
	f.on("Runtime.evaluate", func(c *fakeConn, req fakeRequest) {
		c.reply(req, evalValue(map[string]any{"ok": true})) // JSON.stringify drops an undefined value
	})
	page := openPage(t, f)
	got := "untouched"
	if err := page.EvalExpr(context.Background(), "undefined", &got); err != nil || got != "untouched" {
		t.Fatalf("undefined = %q, %v", got, err)
	}
}

func TestTabClosedMidCall(t *testing.T) {
	f := newFakeChrome(t)
	f.on("Runtime.evaluate", func(c *fakeConn, req fakeRequest) {
		// The user closes the tab while the call is running: Chrome detaches
		// the session and never answers the command.
		c.event("Target.detachedFromTarget", map[string]any{"sessionId": req.SessionID, "targetId": "P"})
	})
	page := openPage(t, f)

	start := time.Now()
	err := page.EvalExpr(context.Background(), "longCall()", nil)
	if !errors.Is(err, ErrTabClosed) {
		t.Fatalf("closed tab = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a closed tab was waited on until the timeout")
	}
	if !errors.Is(page.Err(), ErrTabClosed) {
		t.Errorf("Err() = %v", page.Err())
	}
	// Later calls fail at once instead of going out to a session that is gone.
	if err := page.EvalExpr(context.Background(), "1", nil); !errors.Is(err, ErrTabClosed) {
		t.Fatalf("call after close = %v", err)
	}
}

func TestStaleSessionAnswerIsTabClosed(t *testing.T) {
	f := newFakeChrome(t)
	f.on("Runtime.evaluate", func(c *fakeConn, req fakeRequest) {
		c.replyError(req, -32001, "Session with given id not found.")
	})
	page := openPage(t, f)
	if err := page.EvalExpr(context.Background(), "1", nil); !errors.Is(err, ErrTabClosed) {
		t.Fatalf("answer about a missing session = %v", err)
	}
}

func TestDisconnectMidCall(t *testing.T) {
	f := newFakeChrome(t)
	f.on("Runtime.evaluate", func(c *fakeConn, _ fakeRequest) {
		_ = c.ws.CloseNow() // Chrome quits
	})
	page := openPage(t, f)

	err := page.EvalExpr(context.Background(), "1", nil)
	if !errors.Is(err, ErrDisconnected) {
		t.Fatalf("disconnect = %v", err)
	}
	if !errors.Is(page.Err(), ErrDisconnected) {
		t.Errorf("Err() = %v", page.Err())
	}
	if err := page.EvalExpr(context.Background(), "1", nil); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("call after disconnect = %v", err)
	}
}

func TestCancellingStopsWaiting(t *testing.T) {
	f := newFakeChrome(t)
	f.on("Runtime.evaluate", func(*fakeConn, fakeRequest) { /* the page never answers */ })
	page := openPage(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if err := page.EvalExpr(ctx, "neverResolves()", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancellation waited for an answer")
	}
	// A cancelled call leaves the connection usable.
	if page.Err() != nil {
		t.Fatalf("connection broken after cancellation: %v", page.Err())
	}
}

func TestNavigateReportsChromesLoadError(t *testing.T) {
	f := newFakeChrome(t)
	var navigated atomic.Value
	f.on("Page.navigate", func(c *fakeConn, req fakeRequest) {
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(req.Params, &p)
		navigated.Store(p.URL)
		if strings.Contains(p.URL, "blocked") {
			c.reply(req, map[string]any{"frameId": "F", "errorText": "net::ERR_TUNNEL_CONNECTION_FAILED"})
			return
		}
		f.mu.Lock()
		f.document++
		f.mu.Unlock()
		c.reply(req, map[string]any{"frameId": "F", "loaderId": "L"})
	})
	page := openPage(t, f)

	if err := page.Navigate(context.Background(), "https://www.instagram.com/nasa/"); err != nil {
		t.Fatal(err)
	}
	if navigated.Load() != "https://www.instagram.com/nasa/" {
		t.Errorf("navigated to %v", navigated.Load())
	}
	// The collectors classify a VPN or DNS failure by Chrome's own text.
	err := page.Navigate(context.Background(), "https://www.instagram.com/blocked/")
	if err == nil || !strings.Contains(err.Error(), "net::ERR_TUNNEL_CONNECTION_FAILED") {
		t.Fatalf("load error = %v", err)
	}
}

// Page.navigate answers while the old document is still there and ready. The
// wait must hold out for the new document, through its own loading phase.
func TestNavigateWaitsForTheNewDocument(t *testing.T) {
	f := newFakeChrome(t)
	var (
		mu        sync.Mutex
		navigated bool
		polls     int  // readiness checks since the navigation
		replaced  bool // the new document has arrived
		sawNew    bool // a check answered with the new, ready document
	)
	f.on("Page.navigate", func(c *fakeConn, req fakeRequest) {
		mu.Lock()
		navigated = true
		mu.Unlock()
		c.reply(req, map[string]any{"frameId": "F", "loaderId": "L2"})
	})
	f.on("Runtime.evaluate", func(c *fakeConn, req fakeRequest) {
		if !strings.Contains(expressionOf(t, req), "performance.timeOrigin") {
			c.reply(req, evalValue(map[string]any{"ok": true, "value": true}))
			return
		}
		mu.Lock()
		state := documentState{Origin: 1000, Ready: true} // the old document
		if navigated {
			polls++
			switch {
			case polls <= 2: // the old document lingers, still complete
			case polls == 3: // the new one, still parsing
				replaced = true
				state = documentState{Origin: 2000, Ready: false}
			default:
				state = documentState{Origin: 2000, Ready: true}
				sawNew = true
			}
		}
		mu.Unlock()
		c.reply(req, evalValue(map[string]any{"ok": true, "value": state}))
	})
	page := openPage(t, f)

	if err := page.Navigate(context.Background(), "https://www.instagram.com/nasa/"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !replaced || !sawNew {
		t.Fatalf("Navigate returned after %d checks, before the new document was ready", polls)
	}
}

// A fragment change keeps the document: Chrome answers without a loaderId,
// and the unchanged document is the arrival.
func TestNavigateWithinTheDocument(t *testing.T) {
	f := newFakeChrome(t)
	f.on("Page.navigate", func(c *fakeConn, req fakeRequest) {
		c.reply(req, map[string]any{"frameId": "F"})
	})
	page := openPage(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := page.Navigate(ctx, "https://web.telegram.org/k/#@somegroup"); err != nil {
		t.Fatal(err)
	}
}

func TestWrapperSurvivesAwkwardExpressions(t *testing.T) {
	// The expression is embedded as source, so a trailing line comment must
	// not swallow the wrapper's closing half.
	js := wrap("1 // trailing comment")
	if !strings.Contains(js, "// trailing comment\n") || !strings.HasSuffix(js, "})()") {
		t.Fatalf("wrapper is broken:\n%s", js)
	}
}

func TestActivateBringsTheTabForward(t *testing.T) {
	f := newFakeChrome(t)
	var activated atomic.Value
	f.on("Target.activateTarget", func(c *fakeConn, req fakeRequest) {
		var p struct {
			TargetID string `json:"targetId"`
		}
		_ = json.Unmarshal(req.Params, &p)
		activated.Store(p.TargetID)
		c.reply(req, map[string]any{})
	})
	b := f.connect(t)
	if err := b.Activate(context.Background(), "T7"); err != nil {
		t.Fatal(err)
	}
	if activated.Load() != "T7" {
		t.Errorf("activated %v", activated.Load())
	}
}
