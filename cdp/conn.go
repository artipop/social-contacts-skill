package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// Conn is one websocket to Chrome's browser target. Commands carry an id and
// are answered by id; page commands travel over the same socket tagged with
// the session id of a flattened Target.attachToTarget session.
type Conn struct {
	ws     *websocket.Conn
	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[int64]*pendingCall
	detached map[string]bool
	err      error // set once, when the connection ends
	done     chan struct{}
}

type pendingCall struct {
	session string
	answer  chan message
	fail    chan error
}

type message struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// readLimit bounds one incoming message. The default of the websocket library
// (32 KiB) is far below a page of Telegram participants.
const readLimit = 256 << 20

// writeTimeout bounds sending one command. The websocket library closes the
// whole connection when a write's context ends, so writes get their own
// deadline rather than the caller's context.
const writeTimeout = 10 * time.Second

// awaitingNotice is how long a handshake may take before it is reported as
// waiting for the user's approval.
var awaitingNotice = time.Second

// Dial opens the websocket. With auto-connect, Chrome holds the handshake open
// while it asks the user to allow the connection, so the handshake is given
// approvalTimeout rather than a network timeout, and onAwaiting (if set) is
// called once it has taken long enough that a prompt is the likely reason.
func Dial(ctx context.Context, wsURL string, approvalTimeout time.Duration, onAwaiting func()) (*Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, approvalTimeout)
	defer cancel()

	if onAwaiting != nil {
		notice := time.AfterFunc(awaitingNotice, onAwaiting)
		defer notice.Stop()
	}

	ws, resp, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case dialCtx.Err() != nil:
			return nil, fmt.Errorf("%w (ждали %s)", ErrApprovalPending, approvalTimeout)
		case isRefused(err):
			return nil, fmt.Errorf("%w (порт %s не отвечает)", ErrNotEnabled, wsURL)
		case resp != nil && resp.StatusCode != http.StatusSwitchingProtocols:
			return nil, fmt.Errorf("%w (HTTP %d)", ErrDenied, resp.StatusCode)
		default:
			// The port accepted the connection and then dropped it before
			// answering: what Chrome does when the prompt is declined.
			return nil, fmt.Errorf("%w (%v)", ErrDenied, err)
		}
	}
	ws.SetReadLimit(readLimit)

	c := &Conn{
		ws:       ws,
		pending:  make(map[int64]*pendingCall),
		detached: make(map[string]bool),
		done:     make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func isRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	// WSAECONNREFUSED: Windows reports a refused connection under its own
	// number, which syscall.ECONNREFUSED does not match.
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == 10061
}

// Call sends one command and decodes its result into out (nil discards it).
// An empty session addresses the browser target.
func (c *Conn) Call(ctx context.Context, session, method string, params, out any) error {
	if params == nil {
		params = struct{}{}
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}

	id := c.nextID.Add(1)
	call := &pendingCall{session: session, answer: make(chan message, 1), fail: make(chan error, 1)}
	c.mu.Lock()
	switch {
	case c.err != nil:
		c.mu.Unlock()
		return c.err
	case session != "" && c.detached[session]:
		c.mu.Unlock()
		return ErrTabClosed
	}
	c.pending[id] = call
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	data, err := json.Marshal(message{ID: id, Method: method, Params: rawParams, SessionID: session})
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	err = c.ws.Write(writeCtx, websocket.MessageText, data)
	cancel()
	if err != nil {
		c.shutdown(err)
		return c.Err()
	}

	select {
	case msg := <-call.answer:
		if msg.Error != nil {
			if session != "" && sessionGone(msg.Error.Message) {
				return fmt.Errorf("%w (%s)", ErrTabClosed, msg.Error.Message)
			}
			return &ProtocolError{Method: method, Code: msg.Error.Code, Message: msg.Error.Message}
		}
		if out == nil || len(msg.Result) == 0 {
			return nil
		}
		return json.Unmarshal(msg.Result, out)
	case err := <-call.fail:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sessionGone recognises Chrome's answer to a command for a session that no
// longer exists — the tab closed between our last event and this command.
func sessionGone(message string) bool {
	m := strings.ToLower(message)
	return strings.Contains(m, "session with given id not found") ||
		strings.Contains(m, "target closed") ||
		strings.Contains(m, "no target with given id")
}

func (c *Conn) readLoop() {
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.shutdown(err)
			return
		}
		var msg message
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg.ID != 0 {
			c.mu.Lock()
			call := c.pending[msg.ID]
			c.mu.Unlock()
			if call != nil {
				call.answer <- msg
			}
			continue
		}
		c.event(msg)
	}
}

// event handles what the transport itself must know: a page session ending.
// The flattened session's tab closing arrives as Target.detachedFromTarget on
// the browser connection; Inspector.detached on the session says the same.
func (c *Conn) event(msg message) {
	var session string
	switch msg.Method {
	case "Target.detachedFromTarget":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		session = p.SessionID
	case "Inspector.detached":
		session = msg.SessionID
	}
	if session == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.detached[session] = true
	for _, call := range c.pending {
		if call.session == session {
			select {
			case call.fail <- ErrTabClosed:
			default:
			}
		}
	}
}

// sessionClosed reports whether Chrome has detached the session.
func (c *Conn) sessionClosed(session string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.detached[session]
}

func (c *Conn) shutdown(cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = fmt.Errorf("%w: %v", ErrDisconnected, cause)
	for _, call := range c.pending {
		select {
		case call.fail <- c.err:
		default:
		}
	}
	close(c.done)
}

// Err is why the connection ended, or nil while it is up.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Close ends the connection. The tabs stay as they are.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.err == nil {
		c.err = fmt.Errorf("%w: закрыто", ErrDisconnected)
		for _, call := range c.pending {
			select {
			case call.fail <- c.err:
			default:
			}
		}
		close(c.done)
	}
	c.mu.Unlock()
	return c.ws.Close(websocket.StatusNormalClosure, "")
}
