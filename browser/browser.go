// Package browser is what the collectors need from a browser tab: evaluate a
// function in a page where the user is logged in, read what it returns, and
// point the tab somewhere else.
//
// The collectors depend on this interface rather than on a transport, so the
// same Telegram and Instagram code runs over the Chrome DevTools Protocol
// (package cdp) or over any other way of reaching a logged-in page.
package browser

import (
	"context"
	"time"
)

// Page is one logged-in tab, driven.
type Page interface {
	// URL is where the page actually is, asked of the page itself: for a
	// single-page app that is not necessarily the address it was sent to.
	URL(ctx context.Context) (string, error)
	// Navigate points the tab at a URL and waits until the new document
	// answers.
	Navigate(ctx context.Context, url string) error
	// Eval calls a JavaScript function with one JSON-encoded argument (nil
	// passes undefined) and decodes what it returns into out (nil discards
	// it). The function may be async: its promise is awaited in the page.
	Eval(ctx context.Context, fn string, arg any, out any) error
	// EvalExpr evaluates a bare expression, awaiting it if it is a promise.
	EvalExpr(ctx context.Context, expression string, out any) error
	// Close releases the page. It never closes the user's tab.
	Close()
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
