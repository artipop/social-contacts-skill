package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/artipop/social-contacts-skill/browser"
)

// Page is one tab, driven through a flattened target session. It satisfies
// browser.Page.
type Page struct {
	conn      *Conn
	targetID  string
	sessionID string
}

var _ browser.Page = (*Page)(nil)

// evalTimeout bounds one call. Generous, because some calls ask Telegram for a
// page of participants and wait on the network to answer.
const evalTimeout = 90 * time.Second

// TargetID identifies the tab.
func (p *Page) TargetID() string { return p.targetID }

// Err reports why the page can no longer be driven — ErrTabClosed or
// ErrDisconnected — or nil while it can. A collector reports failures per
// item as text; this tells its caller whether to stop the whole run.
func (p *Page) Err() error {
	if err := p.conn.Err(); err != nil {
		return err
	}
	if p.conn.sessionClosed(p.sessionID) {
		return ErrTabClosed
	}
	return nil
}

// Close detaches from the tab and leaves it open: it is the user's.
func (p *Page) Close() {
	if p.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = p.conn.Call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": p.sessionID}, nil)
}

// URL is where the page actually is, asked of the page.
func (p *Page) URL(ctx context.Context) (string, error) {
	var href string
	if err := p.EvalExpr(ctx, "location.href", &href); err != nil {
		return "", err
	}
	return href, nil
}

// Navigate loads a URL in the tab and waits for the new document to answer.
//
// A failed load comes back as Chrome's own error text (net::ERR_…), which is
// what lets a collector tell a VPN or DNS failure from anything else. Arrival
// is confirmed by asking the page, which also works for a single-page app that
// changes URL without loading anything.
func (p *Page) Navigate(ctx context.Context, url string) error {
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	if err := p.conn.Call(ctx, p.sessionID, "Page.navigate", map[string]any{"url": url}, &nav); err != nil {
		return err
	}
	if nav.ErrorText != "" {
		return fmt.Errorf("%s at %s", nav.ErrorText, url)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := browser.Sleep(ctx, 300*time.Millisecond); err != nil {
			return err
		}
		var ready bool
		err := p.Eval(ctx, "() => document.readyState !== 'loading'", nil, &ready)
		if err == nil && ready {
			return nil
		}
		if errors.Is(err, ErrTabClosed) || errors.Is(err, ErrDisconnected) {
			return err
		}
	}
	return fmt.Errorf("страница %s не загрузилась", url)
}

// Eval calls a JavaScript function with one argument and decodes what it
// returns.
func (p *Page) Eval(ctx context.Context, fn string, arg any, out any) error {
	encodedArg := []byte("undefined")
	if arg != nil {
		var err error
		if encodedArg, err = json.Marshal(arg); err != nil {
			return err
		}
	}
	return p.run(ctx, "("+fn+")("+string(encodedArg)+")", out)
}

// EvalExpr evaluates a bare expression.
func (p *Page) EvalExpr(ctx context.Context, expression string, out any) error {
	return p.run(ctx, expression, out)
}

// The wrapper around every evaluated expression. It travels with each call
// rather than being installed once, because a navigation replaces the document
// and would take any installed helper with it.
//
// It awaits the expression, catches whatever it throws, and returns the
// outcome as one JSON string. A string survives returnByValue whatever the
// value is, and JSON.stringify decides what a result looks like; a thrown
// Telegram error is an object with a .type rather than an Error, and the catch
// keeps that text.
const (
	wrapperPrefix = "(async () => {\n  try {\n    return JSON.stringify({ ok: true, value: await (\n"
	wrapperSuffix = "\n    ) });\n  } catch (e) {\n" +
		"    return JSON.stringify({ ok: false, error: String((e && (e.message || e.type)) || e) });\n" +
		"  }\n})()"
)

func wrap(expression string) string { return wrapperPrefix + expression + wrapperSuffix }

func (p *Page) run(ctx context.Context, expression string, out any) error {
	callCtx, cancel := context.WithTimeout(ctx, evalTimeout)
	defer cancel()

	var res struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	err := p.conn.Call(callCtx, p.sessionID, "Runtime.evaluate", map[string]any{
		"expression":    wrap(expression),
		"awaitPromise":  true,
		"returnByValue": true,
	}, &res)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("страница не ответила за %s", evalTimeout)
		}
		return err
	}
	if d := res.ExceptionDetails; d != nil {
		// Only what the wrapper cannot catch lands here: a syntax error in
		// the expression itself.
		text := d.Text
		if d.Exception != nil && d.Exception.Description != "" {
			text, _, _ = strings.Cut(d.Exception.Description, "\n")
		}
		return fmt.Errorf("ошибка в странице: %s", text)
	}

	var encoded string
	if err := json.Unmarshal(res.Result.Value, &encoded); err != nil {
		return fmt.Errorf("страница вернула не то, что ждали (%s)", res.Result.Type)
	}
	var payload struct {
		OK    bool            `json:"ok"`
		Value json.RawMessage `json:"value"`
		Error string          `json:"error"`
	}
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		return fmt.Errorf("страница вернула не JSON: %w", err)
	}
	if !payload.OK {
		return fmt.Errorf("ошибка в странице: %s", payload.Error)
	}
	if out == nil || len(payload.Value) == 0 {
		return nil
	}
	return json.Unmarshal(payload.Value, out)
}
