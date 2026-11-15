package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/artipop/social-contacts-skill/browser"
	"github.com/artipop/social-contacts-skill/cdp"
	"github.com/artipop/social-contacts-skill/ig"
	"github.com/artipop/social-contacts-skill/tg"
)

// session is the one browser connection a run uses. The browser asks the user
// to allow every new connection, so a run never opens a second one.
type session interface {
	Version(ctx context.Context) (cdp.Version, error)
	Page(ctx context.Context, site cdp.Site) (page, error)
	Activate(ctx context.Context, targetID string) error
	Close() error
}

// page is a tab a collector drives, which can also say whether it is still
// there.
type page interface {
	browser.Page
	TargetID() string
	// Err is non-nil once the tab or the connection is gone.
	Err() error
}

type browserFlags struct {
	cdpURL      string
	userDataDir string
	channel     string
}

func (b *browserFlags) register(fs *flag.FlagSet, getenv func(string) string) {
	fs.StringVar(&b.cdpURL, "cdp-url", getenv(cdp.EnvURL),
		"адрес отладки браузера: ws://… или http://host:port (по умолчанию $"+cdp.EnvURL+")")
	fs.StringVar(&b.userDataDir, "user-data-dir", "",
		"каталог профиля Chromium-браузера, в котором лежит DevToolsActivePort")
	fs.StringVar(&b.channel, "channel", "stable", "канал Chrome: stable, beta, dev, canary")
}

type cdpSession struct{ *cdp.Browser }

func (s cdpSession) Page(ctx context.Context, site cdp.Site) (page, error) {
	p, err := s.Browser.Page(ctx, site)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func connectCDP(ctx context.Context, opts browserFlags, onAwaiting func()) (session, error) {
	b, err := cdp.Connect(ctx, cdp.Options{
		URL:                opts.cdpURL,
		UserDataDir:        opts.userDataDir,
		Channel:            cdp.Channel(opts.channel),
		OnAwaitingApproval: onAwaiting,
	})
	if err != nil {
		return nil, err
	}
	return cdpSession{b}, nil
}

// connectBrowser connects once for the whole run, telling the user when the
// browser is waiting for them to allow it.
func (a *app) connectBrowser(ctx context.Context, opts browserFlags) (session, int) {
	s, err := a.connect(ctx, opts, func() {
		fmt.Fprintln(a.stderr, "браузер спрашивает, разрешить ли подключение: нажмите «Разрешить» в его окне")
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, exitInterrupted
		}
		fmt.Fprintf(a.stderr, "браузер недоступен: %v\n", err)
		return nil, exitBrowser
	}
	return s, exitOK
}

// openPage gets the site's tab: an open one, or a new one.
func (a *app) openPage(ctx context.Context, s session, site cdp.Site) (page, int) {
	p, err := s.Page(ctx, site)
	if err != nil {
		if ctx.Err() != nil {
			return nil, exitInterrupted
		}
		fmt.Fprintf(a.stderr, "не открыть вкладку %s: %v\n", site.URL, err)
		return nil, exitBrowser
	}
	return p, exitOK
}

// lostBrowser reports whether the page can no longer be driven, and says so.
func (a *app) lostBrowser(p page) bool {
	if err := p.Err(); err != nil {
		fmt.Fprintf(a.stderr, "браузер больше недоступен: %v\n", err)
		return true
	}
	return false
}

// collectorOpenFailed maps an error opening a collector to an exit code.
func (a *app) collectorOpenFailed(ctx context.Context, p page, err error) int {
	fmt.Fprintln(a.stderr, err)
	switch {
	case ctx.Err() != nil:
		return exitInterrupted
	case errors.Is(err, tg.ErrNotLoggedIn), errors.Is(err, ig.ErrNotLoggedIn):
		return exitLogin
	case p.Err() != nil:
		return exitBrowser
	}
	return exitError
}

type tgOptions = tg.Options
type igOptions = ig.Options

type telegramCollector interface {
	Collect(ctx context.Context, channelURL string) tg.Result
	Close()
}

type instagramSearcher interface {
	Search(ctx context.Context, profileURL string, people []string) ig.Result
	Close()
}

func openTelegram(ctx context.Context, p page, opts tgOptions) (telegramCollector, error) {
	c, err := tg.Open(ctx, p, opts)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func openInstagram(ctx context.Context, p page, opts igOptions) (instagramSearcher, error) {
	s, err := ig.Open(ctx, p, opts)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// networkSite names the sites a user logs into.
func networkSite(name string) (cdp.Site, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "telegram", "tg":
		return cdp.Telegram, true
	case "instagram", "ig":
		return cdp.Instagram, true
	}
	return cdp.Site{}, false
}
