// Package ig searches a company's Instagram followers for its decision-makers.
//
// This closes a loop that starts in the company registry: we know who runs the
// company, and if one of them follows the company's own account we have a way to
// reach them personally. The search is done through the followers dialog's own
// search box — asking Instagram to filter is the only way through a list that
// may be hundreds of thousands long.
//
// Unlike Telegram there is no data layer to call, so this reads the DOM. It is
// therefore the most fragile thing here, and the code leans on roles and shapes
// rather than class names, and reports *why* it found nothing — a captcha, a
// rate limit and an empty result look identical otherwise.
//
// Everything found stays on the user's machine.
package ig

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/artipop/social-contacts-skill/browser"
	"github.com/artipop/social-contacts-skill/names"
)

// Match is one person found among a profile's followers.
type Match struct {
	Person      string `json:"person"`
	Query       string `json:"query"`
	Username    string `json:"username"`
	FullName    string `json:"full_name"`
	ProfileURL  string `json:"profile_url"`
	MatchedText string `json:"matched_text"`
	Score       int    `json:"score"`
	Confidence  string `json:"confidence"`
}

// Result is what one profile yielded.
type Result struct {
	Profile string
	Matches []Match
	// Status is "ok", or why the profile could not be searched. A blocked
	// profile is not the same as one with no matches, and the user needs to
	// know which happened.
	Status string
	Error  string
}

// Blocked reports that the profile was never actually searched.
func (r Result) Blocked() bool { return blockStatuses[r.Status] }

// blockStatuses mean "we never got to look" — a captcha or checkpoint, a rate
// limit, a login wall, an unavailable page, or a network failure that usually
// means a VPN or geo problem. Recording these instead of a bare zero is what
// stops the user retrying into a wall.
var blockStatuses = map[string]bool{
	"captcha_or_challenge": true,
	"rate_limited":         true,
	"login_required":       true,
	"page_unavailable":     true,
	"network_error":        true,
	"nav_error":            true,
	"followers_dialog":     true,
}

// Page-state markers, Russian and English, matched against the body text.
var (
	captchaMarkers = []string{
		"confirm you're human", "confirm you’re human", "подтвердите, что вы человек",
		"i'm not a robot", "я не робот", "recaptcha", "captcha", "подозрительн",
		"unusual activity", "необычная активность", "verify it's you", "verify your account",
		"подтвердите свою личность", "подтвердите, что это вы", "help us confirm",
		"подозрительная попытка входа", "we detected",
	}
	challengeURLMarkers = []string{"/challenge", "/checkpoint"}
	rateLimitMarkers    = []string{
		"please wait a few minutes", "подождите несколько минут", "try again later",
		"повторите попытку позже", "too many requests", "слишком много запросов",
	}
	unavailableMarkers = []string{
		"page isn't available", "page isn’t available", "page not available",
		"страница недоступна", "this content isn't available", "content isn't available",
		"sorry, this page", "ссылка устарела", "недоступн",
	}
	// Chromium network errors that usually mean a VPN, proxy, DNS or geo problem
	// rather than anything about the profile.
	networkErrorMarkers = []string{
		"err_timed_out", "err_tunnel_connection_failed", "err_proxy_connection_failed",
		"err_connection", "err_name_not_resolved", "err_internet_disconnected",
		"err_address_unreachable", "err_network_changed", "net::", "timeout",
	}
)

// Options tune a search run.
type Options struct {
	// MaxNameVariants caps how many spellings of each name part are tried.
	MaxNameVariants int
	// MaxResultScrolls bounds the scrolling of one query's results.
	MaxResultScrolls int
	ScrollPause      time.Duration
	// SurnameFirst says how the incoming names are written. They come from the
	// company registry, which puts the surname first.
	SurnameFirst bool
	// ProfileTimeout bounds one profile.
	ProfileTimeout time.Duration
	Progress       func(status string, fields map[string]any)
}

func (o Options) withDefaults() Options {
	if o.MaxNameVariants <= 0 {
		o.MaxNameVariants = 8
	}
	if o.MaxResultScrolls <= 0 {
		o.MaxResultScrolls = 12
	}
	if o.ScrollPause <= 0 {
		o.ScrollPause = 600 * time.Millisecond
	}
	if o.ProfileTimeout <= 0 {
		o.ProfileTimeout = 5 * time.Minute
	}
	if o.Progress == nil {
		o.Progress = func(string, map[string]any) {}
	}
	return o
}

// Searcher holds an open Instagram tab across several profiles.
type Searcher struct {
	page *browser.Page
	opts Options
}

// Open takes the app's Instagram window.
func Open(ctx context.Context, page *browser.Page, opts Options) (*Searcher, error) {
	opts = opts.withDefaults()
	s := &Searcher{page: page, opts: opts}

	current, _ := page.URL(ctx)
	if !strings.Contains(current, "instagram.com") {
		if err := page.Navigate(ctx, "https://www.instagram.com/"); err != nil {
			return nil, fmt.Errorf("не открыть Instagram: %w", err)
		}
		_ = browser.Sleep(ctx, 2*time.Second)
	}
	if status, detail := s.pageState(ctx); status == "login_required" {
		return nil, fmt.Errorf("в Instagram не выполнен вход — откройте окно Instagram и войдите (%s)", detail)
	}
	return s, nil
}

func (s *Searcher) Close() { s.page.Close() }

// Search looks for each of the named people among one profile's followers.
func (s *Searcher) Search(ctx context.Context, profileURL string, people []string) Result {
	ctx, cancel := context.WithTimeout(ctx, s.opts.ProfileTimeout)
	defer cancel()

	resolved := NormalizeProfileURL(profileURL)
	if resolved == "" {
		return Result{Profile: profileURL, Status: "unsupported_url",
			Error: "ссылка не похожа на профиль Instagram"}
	}

	if err := s.page.Navigate(ctx, resolved); err != nil {
		status, detail := classifyNavError(err.Error())
		return Result{Profile: profileURL, Status: status, Error: detail}
	}
	if browser.Sleep(ctx, 1500*time.Millisecond) != nil {
		return Result{Profile: profileURL, Status: "cancelled"}
	}

	if status, detail := s.pageState(ctx); status != "" {
		return Result{Profile: profileURL, Status: status, Error: detail}
	}
	if err := s.openFollowers(ctx); err != nil {
		// A checkpoint can appear right after navigation; look again before
		// blaming the dialog.
		if status, detail := s.pageState(ctx); status != "" {
			return Result{Profile: profileURL, Status: status, Error: detail}
		}
		return Result{Profile: profileURL, Status: "followers_dialog", Error: err.Error()}
	}

	seen := map[string]Match{}
	var order []string
	for _, person := range people {
		queries := names.SearchQueries(person, s.opts.SurnameFirst, s.opts.MaxNameVariants)
		if len(queries) == 0 {
			continue
		}
		s.opts.Progress("ig_searching", map[string]any{"profile": profileURL, "person": person})
		for _, query := range queries {
			if ctx.Err() != nil {
				return Result{Profile: profileURL, Matches: ordered(seen, order), Status: "cancelled"}
			}
			for _, match := range s.searchOne(ctx, person, query) {
				key := person + "|" + match.ProfileURL
				// Keep the best evidence for a person: a later query may find
				// the same profile with a weaker query than an earlier one did.
				if existing, dup := seen[key]; dup {
					if rank(match) <= rank(existing) {
						continue
					}
				} else {
					order = append(order, key)
				}
				seen[key] = match
			}
		}
	}
	return Result{Profile: profileURL, Matches: ordered(seen, order), Status: "ok"}
}

func ordered(seen map[string]Match, order []string) []Match {
	out := make([]Match, 0, len(order))
	for _, key := range order {
		out = append(out, seen[key])
	}
	return out
}

// rank orders two findings for the same person and profile: confidence first,
// then how the text matched.
func rank(m Match) int {
	confidence := map[string]int{"high": 3, "medium": 2, "low": 1}[m.Confidence]
	return confidence*1000 + m.Score
}

// searchOne types one query and reads what comes back.
func (s *Searcher) searchOne(ctx context.Context, person, query string) []Match {
	var typed bool
	if err := s.page.Eval(ctx, jsTypeSearch, query, &typed); err != nil || !typed {
		return nil
	}
	// Instagram searches as you type, with a debounce; there is no event to
	// wait on, so this is the pause its own UI takes to answer.
	if browser.Sleep(ctx, 1200*time.Millisecond) != nil {
		return nil
	}

	found := map[string]Match{}
	var order []string
	stable := 0
	for range s.opts.MaxResultScrolls {
		if ctx.Err() != nil {
			break
		}
		var profiles []struct {
			Username   string `json:"username"`
			FullName   string `json:"full_name"`
			ProfileURL string `json:"profile_url"`
			RowText    string `json:"row_text"`
		}
		if err := s.page.EvalExpr(ctx, "("+jsExtractProfiles+")()", &profiles); err != nil {
			break
		}

		before := len(found)
		for _, p := range profiles {
			score := names.MatchScore(query, p.RowText, p.Username, p.FullName)
			if score <= 0 {
				continue
			}
			profileURL := NormalizeProfileURL(p.ProfileURL)
			if profileURL == "" {
				profileURL = p.ProfileURL
			}
			if _, dup := found[profileURL]; dup {
				continue
			}
			order = append(order, profileURL)
			found[profileURL] = Match{
				Person:      person,
				Query:       query,
				Username:    p.Username,
				FullName:    p.FullName,
				ProfileURL:  profileURL,
				MatchedText: p.RowText,
				Score:       score,
				Confidence: names.Confidence(person, s.opts.SurnameFirst, s.opts.MaxNameVariants,
					p.Username, p.FullName, p.RowText),
			}
		}

		// Two rounds that add nothing means the results have settled; scrolling
		// further only spends time and requests.
		if len(found) == before {
			stable++
		} else {
			stable = 0
		}
		if stable >= 2 {
			break
		}

		var moved bool
		if err := s.page.EvalExpr(ctx, "("+jsScrollDialog+")()", &moved); err != nil || !moved {
			break
		}
		if browser.Sleep(ctx, s.opts.ScrollPause) != nil {
			break
		}
	}
	return ordered(found, order)
}

// openFollowers gets the followers dialog onto the screen and ready to search.
func (s *Searcher) openFollowers(ctx context.Context) error {
	if s.searchInputReady(ctx) {
		return nil // already open, from the previous profile
	}
	var clicked bool
	if err := s.page.EvalExpr(ctx, "("+jsClickFollowers+")()", &clicked); err != nil {
		return err
	}
	if !clicked {
		return fmt.Errorf("не найден счётчик подписчиков")
	}
	// The dialog appears before its contents; the search box is the real signal.
	if !browser.WaitUntil(ctx, 12*time.Second, 300*time.Millisecond, func() bool {
		return s.searchInputReady(ctx)
	}) {
		return fmt.Errorf("окно подписчиков не открылось")
	}
	return nil
}

func (s *Searcher) searchInputReady(ctx context.Context) bool {
	var ready bool
	if err := s.page.EvalExpr(ctx, "("+jsFindSearchInput+")()", &ready); err != nil {
		return false
	}
	return ready
}

// pageState classifies what the page is showing. Empty status means it looks
// like an ordinary profile.
func (s *Searcher) pageState(ctx context.Context) (status, detail string) {
	var info struct {
		URL          string `json:"url"`
		Text         string `json:"text"`
		CaptchaFrame bool   `json:"captcha_frame"`
		Login        bool   `json:"login"`
	}
	if err := s.page.EvalExpr(ctx, "("+jsPageState+")()", &info); err != nil {
		return "", ""
	}

	onChallengeURL := containsAny(info.URL, challengeURLMarkers)
	if marker := firstMarker(info.Text, captchaMarkers); onChallengeURL || info.CaptchaFrame || marker != "" {
		switch {
		case onChallengeURL:
			detail = "страница проверки Instagram"
		case info.CaptchaFrame:
			detail = "капча на странице"
		default:
			detail = "на странице: «" + marker + "»"
		}
		return "captcha_or_challenge", detail
	}
	if marker := firstMarker(info.Text, rateLimitMarkers); marker != "" {
		return "rate_limited", "на странице: «" + marker + "»"
	}
	if marker := firstMarker(info.Text, unavailableMarkers); marker != "" {
		return "page_unavailable", "на странице: «" + marker + "» (закрытый профиль, блокировка или регион)"
	}
	if info.Login || strings.Contains(info.URL, "/accounts/login") {
		return "login_required", "требуется вход"
	}
	return "", ""
}

// classifyNavError separates a network failure — usually a VPN, proxy or geo
// problem, and worth saying so — from any other navigation error.
func classifyNavError(message string) (string, string) {
	if containsAny(strings.ToLower(message), networkErrorMarkers) {
		return "network_error", "страница не загрузилась (возможны VPN, прокси, регион или нет связи): " + truncate(message, 160)
	}
	return "nav_error", truncate(message, 200)
}

func firstMarker(text string, markers []string) string {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return marker
		}
	}
	return ""
}

func containsAny(text string, markers []string) bool {
	return firstMarker(text, markers) != ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// technicalPaths are Instagram's own pages, which are not profiles.
var technicalPaths = map[string]bool{
	"about": true, "accounts": true, "api": true, "ar": true, "blog": true,
	"business": true, "challenge": true, "developer": true, "direct": true,
	"directory": true, "explore": true, "graphql": true, "help": true,
	"invites": true, "legal": true, "oauth": true, "p": true, "privacy": true,
	"reel": true, "reels": true, "stories": true, "terms": true, "tv": true,
	"web": true,
}

var igUsernameRe = regexp.MustCompile(`^[A-Za-z0-9._]{1,30}$`)

// NormalizeProfileURL reduces any Instagram link to the canonical profile URL,
// or returns empty when it is not one. Empty is the right answer for a post, a
// reel or one of Instagram's own pages: there are no followers to search there.
func NormalizeProfileURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
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
	if host != "instagram.com" && host != "instagr.am" {
		return ""
	}
	parts := strings.FieldsFunc(parsed.Path, func(r rune) bool { return r == '/' })
	if len(parts) == 0 {
		return ""
	}
	username := strings.TrimPrefix(strings.TrimSpace(parts[0]), "@")
	if technicalPaths[strings.ToLower(username)] || !igUsernameRe.MatchString(username) {
		return ""
	}
	return "https://www.instagram.com/" + username + "/"
}
