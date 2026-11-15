package cdp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// fakeChrome is a CDP endpoint that answers the commands this package sends,
// the way Chrome does, from a table of handlers a test can replace.
type fakeChrome struct {
	t    *testing.T
	srv  *httptest.Server
	path string

	// gate runs before the websocket upgrade; returning false means it
	// answered the request itself (a refusal).
	gate func(w http.ResponseWriter, r *http.Request) bool

	mu       sync.Mutex
	handlers map[string]func(c *fakeConn, req fakeRequest)
	tabs     []Tab
	calls    []fakeRequest
	probes   int // HTTP /json requests
	document int // bumped by every Page.navigate: which document the tab shows
}

type fakeRequest struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId"`
}

type fakeConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (c *fakeConn) send(v any) {
	data, _ := json.Marshal(v)
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.Write(context.Background(), websocket.MessageText, data)
}

func (c *fakeConn) reply(req fakeRequest, result any) {
	c.send(map[string]any{"id": req.ID, "result": result, "sessionId": req.SessionID})
}

func (c *fakeConn) replyError(req fakeRequest, code int, message string) {
	c.send(map[string]any{"id": req.ID, "error": map[string]any{"code": code, "message": message}})
}

func (c *fakeConn) event(method string, params any) {
	c.send(map[string]any{"method": method, "params": params})
}

func newFakeChrome(t *testing.T) *fakeChrome {
	t.Helper()
	f := &fakeChrome{t: t, path: "/devtools/browser/0b1c2d3e", handlers: map[string]func(*fakeConn, fakeRequest){}}
	f.handlers["Browser.getVersion"] = func(c *fakeConn, req fakeRequest) {
		c.reply(req, Version{Product: "Chrome/144.0.0.0", ProtocolVersion: "1.3"})
	}
	f.handlers["Target.getTargets"] = func(c *fakeConn, req fakeRequest) {
		f.mu.Lock()
		infos := append([]Tab{{ID: "SW", Type: "service_worker", URL: "https://web.telegram.org/k/sw.js"}}, f.tabs...)
		f.mu.Unlock()
		c.reply(req, map[string]any{"targetInfos": infos})
	}
	f.handlers["Target.createTarget"] = func(c *fakeConn, req fakeRequest) {
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(req.Params, &p)
		f.mu.Lock()
		id := "T" + strconv.Itoa(len(f.tabs)+1)
		f.tabs = append(f.tabs, Tab{ID: id, Type: "page", URL: p.URL})
		f.mu.Unlock()
		c.reply(req, map[string]any{"targetId": id})
	}
	f.handlers["Target.attachToTarget"] = func(c *fakeConn, req fakeRequest) {
		var p struct {
			TargetID string `json:"targetId"`
			Flatten  bool   `json:"flatten"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if !p.Flatten {
			c.replyError(req, -32602, "flatten expected")
			return
		}
		c.reply(req, map[string]any{"sessionId": "S-" + p.TargetID})
	}
	f.handlers["Target.detachFromTarget"] = func(c *fakeConn, req fakeRequest) { c.reply(req, map[string]any{}) }
	f.handlers["Page.navigate"] = func(c *fakeConn, req fakeRequest) {
		f.mu.Lock()
		f.document++
		f.mu.Unlock()
		c.reply(req, map[string]any{"frameId": "F", "loaderId": "L"})
	}
	f.handlers["Runtime.evaluate"] = func(c *fakeConn, req fakeRequest) {
		// Enough of a page for Navigate's readiness check: every document
		// is ready at once.
		if strings.Contains(expressionOf(t, req), "performance.timeOrigin") {
			f.mu.Lock()
			state := documentState{Origin: float64(1000 + f.document), Ready: true}
			f.mu.Unlock()
			c.reply(req, evalValue(map[string]any{"ok": true, "value": state}))
			return
		}
		c.reply(req, evalValue(map[string]any{"ok": true, "value": true}))
	}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/json") {
			f.mu.Lock()
			f.probes++
			f.mu.Unlock()
		}
		if r.URL.Path == "/json/version" {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"webSocketDebuggerUrl": "ws://" + r.Host + f.path,
			})
			return
		}
		if r.URL.Path != f.path {
			http.NotFound(w, r)
			return
		}
		if f.gate != nil && !f.gate(w, r) {
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(-1)
		conn := &fakeConn{ws: ws}
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var req fakeRequest
			if err := json.Unmarshal(data, &req); err != nil {
				t.Errorf("not JSON: %s", data)
				return
			}
			f.mu.Lock()
			f.calls = append(f.calls, req)
			handler := f.handlers[req.Method]
			f.mu.Unlock()
			if handler == nil {
				conn.replyError(req, -32601, "'"+req.Method+"' wasn't found")
				continue
			}
			go handler(conn, req)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// on replaces the handler of one method.
func (f *fakeChrome) on(method string, handler func(c *fakeConn, req fakeRequest)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = handler
}

func (f *fakeChrome) port() string {
	_, port, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	return port
}

func (f *fakeChrome) wsURL() string { return "ws://127.0.0.1:" + f.port() + f.path }

// profile writes a user data directory whose DevToolsActivePort points here.
func (f *fakeChrome) profile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	content := f.port() + "\n" + f.path + "\n"
	if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f *fakeChrome) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

// evalValue is a Runtime.evaluate answer carrying what the wrapper returns:
// the outcome as a JSON string.
func evalValue(payload map[string]any) map[string]any {
	encoded, _ := json.Marshal(payload)
	return map[string]any{"result": map[string]any{"type": "string", "value": string(encoded)}}
}

// expressionOf takes the wrapper off an evaluated expression.
func expressionOf(t *testing.T, req fakeRequest) string {
	t.Helper()
	var p struct {
		Expression    string `json:"expression"`
		AwaitPromise  bool   `json:"awaitPromise"`
		ReturnByValue bool   `json:"returnByValue"`
	}
	_ = json.Unmarshal(req.Params, &p)
	if !p.AwaitPromise || !p.ReturnByValue {
		t.Errorf("evaluate without awaitPromise/returnByValue: %s", req.Params)
	}
	if !strings.HasPrefix(p.Expression, wrapperPrefix) || !strings.HasSuffix(p.Expression, wrapperSuffix) {
		t.Errorf("expression without the wrapper: %s", p.Expression)
		return p.Expression
	}
	return strings.TrimSuffix(strings.TrimPrefix(p.Expression, wrapperPrefix), wrapperSuffix)
}

// connect connects to the fake through its DevToolsActivePort.
func (f *fakeChrome) connect(t *testing.T) *Browser {
	t.Helper()
	b, err := Connect(context.Background(), Options{UserDataDir: f.profile(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}
