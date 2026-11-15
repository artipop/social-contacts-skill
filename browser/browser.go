// Package browser runs JavaScript inside a webview window owned by the host
// application and gets the answer back.
//
// The collectors need one thing from a browser: evaluate a function in a page
// where the user is logged in, and read what it returns. The script goes in
// through the webview and the result comes back over a loopback endpoint.
package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Window is the three things this package needs of a window, as plain
// functions.
//
// Functions rather than an interface because the window type they come from
// returns itself from SetURL for chaining, and an interface would have to spell
// that out — binding this package to the shape of a UI toolkit for no reason.
// It also makes a fake window in a test three closures.
type Window struct {
	ExecJS func(js string)
	SetURL func(url string)
}

// Bridge is where results come back to. One per app: it binds a loopback
// listener and hands out an address the injected script posts to.
type Bridge struct {
	url      string
	listener net.Listener
	server   *http.Server

	mu      sync.Mutex
	pending map[string]chan result
}

type result struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value"`
	Error string          `json:"error"`
	ID    string          `json:"id"`
}

// NewBridge starts the endpoint. Loopback only, on a port chosen per launch.
//
// Nothing here is a capability worth having: the endpoint accepts results
// addressed to a request id we generated moments ago and are waiting on, and
// anything else is dropped. A result for an unknown id buys nothing.
func NewBridge() (*Bridge, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &Bridge{
		listener: listener,
		url:      fmt.Sprintf("http://127.0.0.1:%d/result", listener.Addr().(*net.TCPAddr).Port),
		pending:  make(map[string]chan result),
	}
	b.server = &http.Server{Handler: http.HandlerFunc(b.handle)}
	go func() { _ = b.server.Serve(listener) }()
	return b, nil
}

func (b *Bridge) Close() {
	if b == nil || b.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = b.server.Shutdown(ctx)
}

func (b *Bridge) handle(w http.ResponseWriter, r *http.Request) {
	// The script posts from https://web.telegram.org and friends, so the
	// response needs to be readable cross-origin for its fetch to settle
	// cleanly. It carries nothing.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Headers", "content-type")
		// WebView2 is Chromium, which asks before a public page may reach a
		// loopback address (Private Network Access). WKWebView does not ask.
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return
	}
	var res result
	if err := json.Unmarshal(body, &res); err != nil || res.ID == "" {
		return
	}

	b.mu.Lock()
	ch := b.pending[res.ID]
	b.mu.Unlock()
	if ch == nil {
		return // nobody is waiting: the call timed out, or the id was made up
	}
	select {
	case ch <- res:
	default:
	}
}

func (b *Bridge) await(id string) (<-chan result, func()) {
	ch := make(chan result, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}
}

// Page is one social window, driven.
type Page struct {
	window Window
	bridge *Bridge
}

// NewPage wraps a window the app already opened.
func NewPage(window Window, bridge *Bridge) *Page {
	return &Page{window: window, bridge: bridge}
}

func (p *Page) usable() bool {
	return p != nil && p.window.ExecJS != nil && p.bridge != nil
}

// Close does nothing to the window: it is the user's, and they logged into it.
// The collectors open and close pages freely; the window outlives all of it.
func (p *Page) Close() {}

// URL is where the window currently is, asked of the page rather than of the
// window. The window only knows what it was told to load; the page knows where
// it actually ended up, which for a single-page app is not the same thing.
func (p *Page) URL(ctx context.Context) (string, error) {
	var href string
	if err := p.EvalExpr(ctx, "location.href", &href); err != nil {
		return "", err
	}
	return href, nil
}

// Navigate points the window at a URL and waits for the page to be there.
//
// There is no navigation event to wait on through this channel, so arrival is
// confirmed by asking the page where it is — which is also the only signal that
// works for a single-page app that changes URL without loading anything.
func (p *Page) Navigate(ctx context.Context, url string) error {
	if !p.usable() || p.window.SetURL == nil {
		return fmt.Errorf("окно не открыто")
	}
	p.window.SetURL(url)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := Sleep(ctx, 300*time.Millisecond); err != nil {
			return err
		}
		// A page that answers at all has a document to answer with.
		var ready bool
		if err := p.Eval(ctx, "() => document.readyState !== 'loading'", nil, &ready); err == nil && ready {
			return nil
		}
	}
	return fmt.Errorf("страница %s не загрузилась", url)
}

// Eval calls a JavaScript function with one argument and decodes what it
// returns. The function may be async: its promise is awaited in the page.
//
// The script is self-contained rather than calling into something injected at
// startup, because a navigation replaces the document and would take any such
// helper with it. Everything one call needs travels with the call.
func (p *Page) Eval(ctx context.Context, fn string, arg any, out any) error {
	if !p.usable() {
		return fmt.Errorf("окно не открыто")
	}
	encodedArg := []byte("undefined")
	if arg != nil {
		var err error
		if encodedArg, err = json.Marshal(arg); err != nil {
			return err
		}
	}
	return p.run(ctx, "("+fn+")("+string(encodedArg)+")", out)
}

// EvalExpr evaluates a bare expression, for the calls with nothing to pass in.
func (p *Page) EvalExpr(ctx context.Context, expression string, out any) error {
	if !p.usable() {
		return fmt.Errorf("окно не открыто")
	}
	return p.run(ctx, expression, out)
}

func (p *Page) run(ctx context.Context, expression string, out any) error {
	id := newID()
	answers, release := p.bridge.await(id)
	defer release()

	p.window.ExecJS(script(id, expression, p.bridge.url))

	select {
	case res := <-answers:
		if !res.OK {
			return fmt.Errorf("ошибка в странице: %s", res.Error)
		}
		if out == nil || len(res.Value) == 0 {
			return nil
		}
		return json.Unmarshal(res.Value, out)
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(evalTimeout):
		// Nothing came back. Usually the page was replaced mid-call, or the
		// expression threw somewhere the wrapper could not see.
		return fmt.Errorf("страница не ответила за %s", evalTimeout)
	}
}

// evalTimeout bounds one call. Generous, because some of these ask Telegram for
// a page of participants and wait on the network to answer.
const evalTimeout = 90 * time.Second

// script wraps an expression so its result — or its failure — comes back.
//
// The body catches everything: an expression that throws must still report, or
// the caller waits out the timeout for an answer that already exists. The post
// is text/plain so it stays a simple request and needs no preflight.
func script(id, expression, endpoint string) string {
	idJSON, _ := json.Marshal(id)
	endpointJSON, _ := json.Marshal(endpoint)
	return `(async () => {
  let payload;
  try {
    payload = { id: ` + string(idJSON) + `, ok: true, value: await (` + expression + `) };
  } catch (e) {
    payload = { id: ` + string(idJSON) + `, ok: false, error: String((e && (e.message || e.type)) || e) };
  }
  try {
    await fetch(` + string(endpointJSON) + `, {
      method: "POST",
      headers: { "Content-Type": "text/plain" },
      body: JSON.stringify(payload),
    });
  } catch (e) { /* the app is gone; nothing to report to */ }
})()`
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Sleep pauses, returning early if the caller gives up. Every collector pauses
// between pages and scrolls; doing it without watching for cancellation is what
// makes a Stop take a minute to land.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitUntil polls check until it passes, the budget runs out, or the caller
// gives up. For the conditions that have no event to wait on — a login
// completing, a data layer finishing its boot.
func WaitUntil(ctx context.Context, timeout, interval time.Duration, check func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if check() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		if err := Sleep(ctx, interval); err != nil {
			return false
		}
	}
}
