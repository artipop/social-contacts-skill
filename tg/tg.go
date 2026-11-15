// Package tg collects Telegram channel and group members.
//
// It asks Telegram's own manager layer rather than scraping the rendered member
// list: a bridge function is evaluated inside the user's logged-in Telegram Web
// page and calls rootScope.managers directly. That is both faster and far less
// fragile — a redesign of the members dialog does not break it — and it yields
// @usernames and phone numbers that the DOM never shows without opening every
// profile card.
//
// Everything collected here stays on the user's machine.
package tg

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/artipop/social-contacts-skill/browser"
)

const telegramWebURL = "https://web.telegram.org/k/"

// Member is one person found in a channel's audience.
type Member struct {
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	Phone     string `json:"phone"`
	IsBot     bool   `json:"is_bot"`
	IsPremium bool   `json:"is_premium"`
	IsAdmin   bool   `json:"is_admin"`
	Role      string `json:"role"`
	Bio       string `json:"bio"`
}

// PublicURL is where the member can be reached, empty for an account with no
// public username.
func (m Member) PublicURL() string {
	if m.Username == "" {
		return ""
	}
	return "https://t.me/" + m.Username
}

// Options tune one collection run.
type Options struct {
	// PageLimit is how many participants to ask for per call.
	PageLimit int
	// MaxMembers caps a channel; 0 means everything reachable.
	MaxMembers int
	// DeepSearch sweeps the alphabet to get past the ~200 recent-participants
	// cap non-admins are subject to. Much slower, and much more thorough.
	DeepSearch bool
	// CollectBios fetches each member's "about" — one call per user, so it is
	// slow and rate-limited.
	CollectBios bool
	MaxBios     int
	// ChannelTimeout bounds one channel, so a wedged one cannot hold the rest.
	ChannelTimeout time.Duration
	// Progress reports what is happening, for the activity line.
	Progress func(status string, fields map[string]any)
}

func (o Options) withDefaults() Options {
	if o.PageLimit <= 0 {
		o.PageLimit = 200
	}
	if o.ChannelTimeout <= 0 {
		o.ChannelTimeout = 180 * time.Second
	}
	if o.Progress == nil {
		o.Progress = func(string, map[string]any) {}
	}
	return o
}

const (
	// pagePause paces successive participant pages so the manager layer is not
	// hammered into a flood-wait we could have avoided.
	pagePause = 300 * time.Millisecond
	// maxFloodWait is the longest "come back in N seconds" worth honouring.
	// Beyond this, Telegram is telling us to stop rather than to wait.
	maxFloodWait = 120 * time.Second
)

// searchQueries is the alphabet for the deep sweep — latin, cyrillic, digits.
// The empty query first returns the same set as "recent".
var searchQueries = append([]string{""},
	strings.Split("abcdefghijklmnopqrstuvwxyzабвгдеёжзийклмнопрстуфхцчшщэюя0123456789", "")...)

// Collector holds an open Telegram Web page for a run of several channels.
type Collector struct {
	page browser.Page
	opts Options
}

// Open takes a Telegram Web page and waits for it to be usable. Errors
// here are the ones worth telling the user about: not logged in, or the page's
// data layer never came up.
func Open(ctx context.Context, page browser.Page, opts Options) (*Collector, error) {
	opts = opts.withDefaults()

	current, _ := page.URL(ctx)
	if !strings.Contains(current, "web.telegram.org") {
		if err := page.Navigate(ctx, telegramWebURL); err != nil {
			return nil, fmt.Errorf("не открыть Telegram Web: %w", err)
		}
		_ = browser.Sleep(ctx, time.Second)
	}

	c := &Collector{page: page, opts: opts}
	if !browser.WaitUntil(ctx, 20*time.Second, 2*time.Second, func() bool { return c.loggedIn(ctx) }) {
		return nil, fmt.Errorf("в Telegram Web не выполнен вход — откройте web.telegram.org/k в браузере и войдите")
	}
	// The manager layer boots after the page does, and calling into it early
	// fails in a way that looks like a missing channel rather than a race.
	if !browser.WaitUntil(ctx, 30*time.Second, time.Second, func() bool {
		res, err := c.call(ctx, map[string]any{"op": "whoami"})
		return err == nil && res.OK
	}) {
		return nil, fmt.Errorf("Telegram Web ещё не готов — подождите, пока прогрузится, и повторите")
	}
	return c, nil
}

func (c *Collector) Close() { c.page.Close() }

func (c *Collector) loggedIn(ctx context.Context) bool {
	var ok bool
	if err := c.page.Eval(ctx, jsIsLoggedIn, nil, &ok); err != nil {
		return false
	}
	return ok
}

// bridgeResult is what one op returns. The fields are a union across ops; each
// caller reads the ones its op fills.
type bridgeResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`

	// resolve
	ID          string `json:"id"`
	Title       string `json:"title"`
	Username    string `json:"username"`
	IsUser      bool   `json:"is_user"`
	IsBroadcast bool   `json:"is_broadcast"`
	IsMegagroup bool   `json:"is_megagroup"`

	// channel_full
	LinkedChatID        string `json:"linked_chat_id"`
	CanViewParticipants bool   `json:"can_view_participants"`

	// participants
	Count     int      `json:"count"`
	Returned  int      `json:"returned"`
	Members   []Member `json:"members"`
	FloodWait int      `json:"flood_wait"`

	// user_full
	About string `json:"about"`
}

func (c *Collector) call(ctx context.Context, params map[string]any) (bridgeResult, error) {
	var res bridgeResult
	if err := c.page.Eval(ctx, jsBridge, params, &res); err != nil {
		return bridgeResult{}, err
	}
	return res, nil
}

// floodWait is how long Telegram asks us to wait. The structured hint is
// preferred; the error text is the fallback. This is the server's own number,
// so it is honoured exactly rather than turned into a guess or a backoff.
func (r bridgeResult) floodWait() time.Duration {
	if r.FloodWait > 0 {
		return time.Duration(r.FloodWait) * time.Second
	}
	if m := floodWaitRe.FindStringSubmatch(r.Error); m != nil {
		var seconds int
		fmt.Sscanf(m[1], "%d", &seconds)
		return time.Duration(seconds) * time.Second
	}
	return 0
}

var floodWaitRe = regexp.MustCompile(`FLOOD_WAIT_(\d+)`)

// Result is what one channel yielded.
type Result struct {
	Channel string
	Members []Member
	// Total is what Telegram says the audience is, which can exceed what a
	// non-admin is allowed to page through.
	Total  int
	Status string
	Error  string
}

// Collect walks one channel. Errors are returned inside the Result rather than
// as a Go error: one unreachable channel must not end a run over many.
func (c *Collector) Collect(ctx context.Context, channelURL string) Result {
	ctx, cancel := context.WithTimeout(ctx, c.opts.ChannelTimeout)
	defer cancel()

	target, result := c.resolveTarget(ctx, channelURL)
	if result != nil {
		return *result
	}

	c.opts.Progress("tg_collecting", map[string]any{"channel": channelURL})
	members, total, lastErr := c.pageParticipants(ctx, target)

	// Members the API returned as a bare id — privacy-hidden or deleted
	// accounts — carry no way to reach them, so they are not results.
	reachable := members[:0]
	for _, m := range members {
		if strings.TrimSpace(m.Name) != "" || strings.TrimSpace(m.Username) != "" {
			reachable = append(reachable, m)
		}
	}
	members = reachable

	if len(members) == 0 {
		return Result{Channel: channelURL, Total: total, Status: "members_not_found", Error: lastErr}
	}
	if c.opts.CollectBios {
		c.collectBios(ctx, members)
	}
	return Result{Channel: channelURL, Members: members, Total: total, Status: "ok"}
}

type tgTarget struct {
	chatID   string
	kind     string
	username string
}

// resolveTarget turns a link into something whose participants can be read.
//
// A broadcast channel hides its subscribers from the API, so the linked
// discussion group is targeted instead — that group's members are readable and
// are, in practice, the engaged part of the audience anyway. A supergroup is
// targeted directly.
func (c *Collector) resolveTarget(ctx context.Context, channelURL string) (tgTarget, *Result) {
	username := usernameFromURL(channelURL)
	if username == "" {
		return tgTarget{}, &Result{
			Channel: channelURL, Status: "unsupported_url",
			Error: "нет публичного @username (пригласительные ссылки не поддерживаются)",
		}
	}

	resolved, err := c.call(ctx, map[string]any{"op": "resolve", "username": username})
	if err != nil {
		return tgTarget{}, &Result{Channel: channelURL, Status: "resolve_failed", Error: err.Error()}
	}
	if !resolved.OK {
		return tgTarget{}, &Result{Channel: channelURL, Status: "resolve_failed", Error: resolved.Error}
	}
	if resolved.IsUser {
		return tgTarget{}, &Result{
			Channel: channelURL, Status: "is_user",
			Error: "ссылка ведёт на пользователя, а не на канал или группу",
		}
	}

	kind := "chat"
	switch {
	case resolved.IsMegagroup:
		kind = "megagroup"
	case resolved.IsBroadcast:
		kind = "channel"
	}

	if resolved.IsBroadcast && !resolved.IsMegagroup {
		full, err := c.call(ctx, map[string]any{"op": "channel_full", "chat_id": resolved.ID})
		switch {
		case err == nil && full.OK && full.LinkedChatID != "":
			return tgTarget{chatID: full.LinkedChatID, kind: "discussion", username: resolved.Username}, nil
		case err == nil && full.OK && !full.CanViewParticipants:
			return tgTarget{}, &Result{
				Channel: channelURL, Status: "no_linked_discussion",
				Error: "у канала нет группы обсуждения, а подписчики скрыты",
			}
		}
		// Otherwise we are an admin of the channel and may read it directly.
	}
	return tgTarget{chatID: resolved.ID, kind: kind, username: resolved.Username}, nil
}

// pageParticipants walks the participant pages, deduplicating as it goes.
func (c *Collector) pageParticipants(ctx context.Context, target tgTarget) (members []Member, total int, lastErr string) {
	seen := map[string]Member{}
	order := []string{}

	queries := []string{""}
	useQuery := false
	if c.opts.DeepSearch {
		queries = searchQueries
		useQuery = true
	}

	for _, query := range queries {
		offset := 0
		for {
			if ctx.Err() != nil {
				return collected(seen, order), total, lastErr
			}
			params := map[string]any{
				"op": "participants", "chat_id": target.chatID,
				"limit": c.opts.PageLimit, "offset": offset,
			}
			if useQuery {
				params["q"] = query
			}

			res, err := c.call(ctx, params)
			if err != nil {
				lastErr = err.Error()
				break
			}
			if !res.OK {
				lastErr = res.Error
				// Telegram asked us to slow down. Say so — a silent pause looks
				// like a hang — then wait exactly as long as it asked and retry
				// the same offset.
				if wait := res.floodWait(); wait > 0 {
					c.opts.Progress("tg_flood_wait", map[string]any{
						"channel": firstNonEmpty(target.username, target.chatID),
						"seconds": int(wait.Seconds()), "honored": wait <= maxFloodWait,
					})
					if wait <= maxFloodWait {
						if browser.Sleep(ctx, wait+time.Second) != nil {
							return collected(seen, order), total, lastErr
						}
						continue
					}
				}
				break
			}

			if res.Count > total {
				total = res.Count
			}
			for _, m := range res.Members {
				if m.UserID == "" {
					continue
				}
				if _, dup := seen[m.UserID]; dup {
					continue
				}
				seen[m.UserID] = m
				order = append(order, m.UserID)
			}
			if c.opts.MaxMembers > 0 && len(order) >= c.opts.MaxMembers {
				return collected(seen, order)[:c.opts.MaxMembers], total, lastErr
			}

			offset += res.Returned
			// A short or empty page is the end of this query, and walking past
			// the reported count is pointless on the plain (unqueried) sweep.
			if res.Returned < c.opts.PageLimit || res.Returned == 0 {
				break
			}
			if total > 0 && offset >= total && !useQuery {
				break
			}
			if browser.Sleep(ctx, pagePause) != nil {
				return collected(seen, order), total, lastErr
			}
		}
	}
	return collected(seen, order), total, lastErr
}

func collected(seen map[string]Member, order []string) []Member {
	out := make([]Member, 0, len(order))
	for _, id := range order {
		out = append(out, seen[id])
	}
	return out
}

// collectBios fills each member's "about". One call per user, so it is gated
// behind an option and a cap; a failure for one person is skipped rather than
// failing the channel.
func (c *Collector) collectBios(ctx context.Context, members []Member) {
	budget := len(members)
	if c.opts.MaxBios > 0 && c.opts.MaxBios < budget {
		budget = c.opts.MaxBios
	}
	for i := range members[:budget] {
		if ctx.Err() != nil {
			return
		}
		if members[i].UserID == "" {
			continue
		}
		res, err := c.call(ctx, map[string]any{"op": "user_full", "user_id": members[i].UserID})
		if err == nil && res.OK {
			members[i].Bio = strings.TrimSpace(res.About)
		}
	}
}

var (
	usernameRe = regexp.MustCompile(`^[A-Za-z0-9_]{4,32}$`)
	botRe      = regexp.MustCompile(`(?i)_bot$|/bot`)
)

// Collectable reports whether a link is worth opening. Bots have no member list.
func Collectable(rawURL string) bool {
	return rawURL != "" && !botRe.MatchString(rawURL)
}

// usernameFromURL pulls a public @username out of a t.me / telegram.me /
// web.telegram.org link. An invite link carries no resolvable name, so it is
// rejected rather than half-handled.
func usernameFromURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "@") {
		candidate := value[1:]
		if usernameRe.MatchString(candidate) {
			return candidate
		}
		return ""
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")

	if host == "web.telegram.org" {
		fragment := strings.TrimPrefix(strings.TrimSpace(parsed.Fragment), "/")
		fragment = strings.TrimPrefix(fragment, "@")
		if usernameRe.MatchString(fragment) {
			return fragment
		}
		return ""
	}
	if host != "t.me" && host != "telegram.me" {
		return ""
	}
	path := strings.Trim(parsed.Path, "/")
	path = strings.TrimPrefix(path, "s/")
	if path == "" || strings.HasPrefix(path, "+") || strings.HasPrefix(path, "joinchat") {
		return ""
	}
	name, _, _ := strings.Cut(path, "/")
	if usernameRe.MatchString(name) {
		return name
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
