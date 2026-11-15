// Package cdp drives the user's own Chrome over the Chrome DevTools Protocol.
//
// It connects the way Chrome's auto-connect does: the user switches remote
// debugging on at chrome://inspect/#remote-debugging (Chrome 144+), Chrome
// writes the address into DevToolsActivePort in its user data directory, and
// asks the user to allow each new connection. The collectors then run in tabs
// where the user is already logged in, with no second browser or profile.
//
// The client is deliberately small: one websocket, commands matched to answers
// by id, and page commands sent over flattened target sessions.
package cdp

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Options say how to find Chrome.
type Options struct {
	// URL overrides discovery: a ws:// browser address, or the http:// address
	// of a Chrome started with --remote-debugging-port. When empty, the
	// SOCIAL_CONTACTS_CDP_URL environment variable is used, then
	// DevToolsActivePort.
	URL string
	// Channel picks which installed Chrome to connect to; Stable by default.
	Channel Channel
	// UserDataDir overrides the channel's default profile directory, for a
	// Chromium build or a non-default --user-data-dir.
	UserDataDir string
	// ApprovalTimeout is how long to wait for the user to allow the
	// connection in Chrome's prompt. Default one minute.
	ApprovalTimeout time.Duration
	// OnAwaitingApproval is called once if the connection has not been
	// allowed after about a second, so the caller can tell the user to look at
	// Chrome.
	OnAwaitingApproval func()
}

// Browser is a connection to the user's Chrome.
type Browser struct {
	conn     *Conn
	endpoint string
}

// Connect finds Chrome and connects to it. The errors worth showing the user
// are ErrNotEnabled, ErrProfileUnreadable, ErrApprovalPending and ErrDenied.
func Connect(ctx context.Context, opts Options) (*Browser, error) {
	wsURL, err := endpoint(ctx, opts)
	if err != nil {
		return nil, err
	}
	timeout := opts.ApprovalTimeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	conn, err := Dial(ctx, wsURL, timeout, opts.OnAwaitingApproval)
	if err != nil {
		return nil, err
	}
	return &Browser{conn: conn, endpoint: wsURL}, nil
}

// Endpoint is the websocket address connected to.
func (b *Browser) Endpoint() string { return b.endpoint }

// Conn is the underlying connection, for commands this package does not wrap.
func (b *Browser) Conn() *Conn { return b.conn }

// Close disconnects. Chrome and its tabs stay as they are.
func (b *Browser) Close() error { return b.conn.Close() }

// Version is what Browser.getVersion reports.
type Version struct {
	Product         string `json:"product"`
	ProtocolVersion string `json:"protocolVersion"`
	UserAgent       string `json:"userAgent"`
	JSVersion       string `json:"jsVersion"`
}

// Version asks Chrome what it is.
func (b *Browser) Version(ctx context.Context) (Version, error) {
	var v Version
	err := b.conn.Call(ctx, "", "Browser.getVersion", nil, &v)
	return v, err
}

// Tab is one open page target.
type Tab struct {
	ID    string `json:"targetId"`
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

// Tabs lists the open tabs.
func (b *Browser) Tabs(ctx context.Context) ([]Tab, error) {
	var res struct {
		TargetInfos []Tab `json:"targetInfos"`
	}
	if err := b.conn.Call(ctx, "", "Target.getTargets", nil, &res); err != nil {
		return nil, err
	}
	tabs := res.TargetInfos[:0]
	for _, t := range res.TargetInfos {
		if t.Type == "page" {
			tabs = append(tabs, t)
		}
	}
	return tabs, nil
}

// Site is where a collector works: which open tab will do, and what to open
// when none does.
type Site struct {
	Match func(url string) bool
	URL   string
}

var (
	// Telegram is the Telegram Web K client the Telegram collector runs in.
	Telegram = Site{
		URL: "https://web.telegram.org/k/",
		Match: func(raw string) bool {
			u, err := url.Parse(raw)
			return err == nil && u.Hostname() == "web.telegram.org" &&
				(u.Path == "/k" || strings.HasPrefix(u.Path, "/k/"))
		},
	}
	// Instagram is the Instagram site the Instagram collector runs in.
	Instagram = Site{
		URL: "https://www.instagram.com/",
		Match: func(raw string) bool {
			u, err := url.Parse(raw)
			if err != nil {
				return false
			}
			host := strings.TrimPrefix(u.Hostname(), "www.")
			return host == "instagram.com"
		},
	}
)

// Page returns a page in a tab of the site: the first open tab that matches,
// or a new tab opened at the site's URL. The user's tab is never closed by
// this package.
func (b *Browser) Page(ctx context.Context, site Site) (*Page, error) {
	tabs, err := b.Tabs(ctx)
	if err != nil {
		return nil, err
	}
	for _, tab := range tabs {
		if site.Match != nil && site.Match(tab.URL) {
			return b.Attach(ctx, tab.ID)
		}
	}
	return b.Open(ctx, site.URL)
}

// Open opens a new tab at the URL and attaches to it.
func (b *Browser) Open(ctx context.Context, rawURL string) (*Page, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := b.conn.Call(ctx, "", "Target.createTarget", map[string]any{"url": rawURL}, &created); err != nil {
		return nil, fmt.Errorf("не открыть вкладку %s: %w", rawURL, err)
	}
	return b.Attach(ctx, created.TargetID)
}

// Attach drives an existing tab by its target id.
func (b *Browser) Attach(ctx context.Context, targetID string) (*Page, error) {
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	err := b.conn.Call(ctx, "", "Target.attachToTarget",
		map[string]any{"targetId": targetID, "flatten": true}, &attached)
	if err != nil {
		return nil, fmt.Errorf("не подключиться к вкладке: %w", err)
	}
	return &Page{conn: b.conn, targetID: targetID, sessionID: attached.SessionID}, nil
}
