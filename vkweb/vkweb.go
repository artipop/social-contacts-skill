// Package vkweb finds named people through VK's own people search, in the
// user's logged-in browser.
//
// The VK API's users.search is closed to service keys and VK ID tokens, so a
// name can only be looked up the way a person does it: on
// vk.com/search/people, with the place of work and the city as the page's own
// filters. Only the search results are read; no profile is opened.
//
// Like Instagram this reads the DOM, so it reports why it found nothing — a
// captcha, a rate limit and an empty result look the same otherwise.
//
// Everything found stays on the user's machine.
package vkweb

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/artipop/social-contacts-skill/browser"
	"github.com/artipop/social-contacts-skill/names"
)

// SearchURL is the people search page.
const SearchURL = "https://vk.com/search/people"

// Query is what one search asked VK.
type Query struct {
	Name    string `json:"name"`
	City    string `json:"city,omitempty"`
	Company string `json:"company,omitempty"`
}

// Candidate is one person in the search results whose name matches.
type Candidate struct {
	ProfileURL string `json:"profile_url"`
	Name       string `json:"name"`
	// Snippet is the line VK shows under the name: age, city, place of work
	// or a community, as shown.
	Snippet    string `json:"snippet,omitempty"`
	Score      int    `json:"score"`
	Confidence string `json:"confidence"`
}

// Result is what searching for one person yielded.
type Result struct {
	TargetPerson string      `json:"target_person"`
	Query        Query       `json:"query"`
	Candidates   []Candidate `json:"candidates,omitempty"`
	// Status is "ok" with candidates, "not_found", or why the search could
	// not be done.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Blocked reports that the search never got to look.
func (r Result) Blocked() bool { return blockStatuses[r.Status] }

var blockStatuses = map[string]bool{
	"captcha_or_challenge": true,
	"rate_limited":         true,
	"network_error":        true,
	"nav_error":            true,
	"not_logged_in":        true,
}

var (
	captchaMarkers = []string{
		"я не робот", "не робот", "подтвердите, что вы", "введите код с картинки",
		"captcha", "капча", "i'm not a robot", "confirm you're human",
	}
	rateLimitMarkers = []string{
		"слишком много запросов", "too many requests", "однотипной страницы",
		"попробуйте позже", "try again later", "превышен лимит",
	}
	networkErrorMarkers = []string{
		"err_timed_out", "err_tunnel_connection_failed", "err_proxy_connection_failed",
		"err_connection", "err_name_not_resolved", "err_internet_disconnected",
		"err_address_unreachable", "err_network_changed", "net::", "timeout",
	}
	// actionLabels are the buttons of a result row, not part of its snippet.
	actionLabels = map[string]bool{
		"написать сообщение": true, "добавить": true, "добавить в друзья": true,
		"подписаться": true, "отписаться": true, "подробнее": true,
		"заявка отправлена": true, "вы подписаны": true, "у вас в друзьях": true,
		"убрать из друзей": true, "отменить заявку": true,
		"message": true, "add": true, "follow": true, "more": true,
	}
)

// Options tune a search run.
type Options struct {
	// SurnameFirst says the names are written surname first, as in company
	// registries.
	SurnameFirst bool
	// MaxCandidates caps the candidates reported per person.
	MaxCandidates int
	// Pause and Jitter space the searches: each one waits Pause plus up to
	// Jitter after the previous.
	Pause, Jitter time.Duration
	// ResultsTimeout bounds the wait for one results page.
	ResultsTimeout time.Duration
	Progress       func(status string, fields map[string]any)
}

func (o Options) withDefaults() Options {
	if o.MaxCandidates <= 0 {
		o.MaxCandidates = 10
	}
	if o.Pause <= 0 {
		o.Pause = 6 * time.Second
	}
	if o.Jitter < 0 {
		o.Jitter = 0
	} else if o.Jitter == 0 {
		o.Jitter = 4 * time.Second
	}
	if o.ResultsTimeout <= 0 {
		o.ResultsTimeout = 15 * time.Second
	}
	if o.Progress == nil {
		o.Progress = func(string, map[string]any) {}
	}
	return o
}

// ErrNotLoggedIn is what Open returns when VK is logged out: only the user
// can log in.
var ErrNotLoggedIn = errors.New("в VK не выполнен вход — откройте vk.com в браузере и войдите")

// Searcher holds an open VK tab across several searches.
type Searcher struct {
	page browser.Page
	opts Options
	last time.Time
}

// Open takes a VK page and checks that the user is logged in.
func Open(ctx context.Context, page browser.Page, opts Options) (*Searcher, error) {
	s := &Searcher{page: page, opts: opts.withDefaults()}
	current, _ := page.URL(ctx)
	if !IsVKURL(current) {
		if err := page.Navigate(ctx, "https://vk.com/feed"); err != nil {
			return nil, fmt.Errorf("не открыть VK: %w", err)
		}
		_ = browser.Sleep(ctx, 2*time.Second)
	}
	if status, detail := s.pageState(ctx); status == "not_logged_in" {
		return nil, fmt.Errorf("%w (%s)", ErrNotLoggedIn, detail)
	}
	return s, nil
}

func (s *Searcher) Close() { s.page.Close() }

// Search looks for one person: by name and place of work first, then by name
// and city if that found nobody. Without either, by name alone.
func (s *Searcher) Search(ctx context.Context, person, city, company string) Result {
	person, city, company = strings.TrimSpace(person), strings.TrimSpace(city), strings.TrimSpace(company)
	parsed, ok := names.Split(person, s.opts.SurnameFirst)
	if !ok {
		return Result{TargetPerson: person, Status: "invalid_name", Error: "в ФИО нужны фамилия и имя"}
	}
	name := titleCase(parsed.First) + " " + titleCase(parsed.Last)

	var steps []Query
	if company != "" {
		steps = append(steps, Query{Name: name, Company: company})
	}
	if city != "" {
		steps = append(steps, Query{Name: name, City: city})
	}
	if len(steps) == 0 {
		steps = append(steps, Query{Name: name})
	}

	var res Result
	for _, q := range steps {
		s.opts.Progress("vk_searching", map[string]any{"person": person, "city": q.City, "company": q.Company})
		res = s.searchOne(ctx, person, q, city, company)
		if res.Status != "not_found" {
			return res
		}
	}
	return res
}

// searchOne runs one query and reads its first results page: the name is
// searched, then the filters are applied on the page.
func (s *Searcher) searchOne(ctx context.Context, person string, q Query, city, company string) Result {
	res := Result{TargetPerson: person, Query: q}
	fail := func(status, detail string) Result {
		res.Status, res.Error = status, detail
		return res
	}
	if status, detail := s.load(ctx, SearchPageURL(q.Name)); status != "" {
		return fail(status, detail)
	}
	page, status, detail := s.results(ctx)
	if status != "" {
		return fail(status, detail)
	}
	if (q.City != "" || q.Company != "") && !page.Empty {
		var applied struct {
			OK          bool   `json:"ok"`
			UnknownCity bool   `json:"unknown_city"`
			Error       string `json:"error"`
		}
		err := s.page.Eval(ctx, jsApplyFilters, map[string]string{"city": q.City, "company": q.Company}, &applied)
		if err != nil {
			return fail("nav_error", "фильтры поиска: "+truncate(err.Error(), 160))
		}
		if status, detail := s.pageState(ctx); status != "" {
			return fail(status, detail)
		}
		if applied.UnknownCity {
			return fail("not_found", applied.Error)
		}
		if !applied.OK {
			return fail("nav_error", "фильтры поиска: "+applied.Error)
		}
		if page, status, detail = s.results(ctx); status != "" {
			return fail(status, detail)
		}
	}
	res.Candidates = Candidates(person, s.opts.SurnameFirst, city, company, page.Rows, s.opts.MaxCandidates)
	res.Status = "not_found"
	if len(res.Candidates) > 0 {
		res.Status = "ok"
	}
	return res
}

// SearchPageURL is the search page for a name. Spaces go as %20: VK reads a
// '+' in the address as a plus sign, not a space.
func SearchPageURL(name string) string {
	return SearchURL + "?q=" + strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
}

// load paces and navigates, then checks the page is a usable one.
func (s *Searcher) load(ctx context.Context, target string) (string, string) {
	if !s.last.IsZero() {
		wait := s.opts.Pause
		if s.opts.Jitter > 0 {
			wait += time.Duration(rand.Int64N(int64(s.opts.Jitter)))
		}
		if wait -= time.Since(s.last); wait > 0 {
			if browser.Sleep(ctx, wait) != nil {
				return "cancelled", ""
			}
		}
	}
	s.last = time.Now()
	if err := s.page.Navigate(ctx, target); err != nil {
		return classifyNavError(err.Error())
	}
	return s.pageState(ctx)
}

// Row is one person on the results page, as the page shows them.
type Row struct {
	Path  string   `json:"path"`
	Name  string   `json:"name"`
	Lines []string `json:"lines"`
}

type resultsPage struct {
	Rows  []Row `json:"rows"`
	Empty bool  `json:"empty"`
}

// results waits for the results to render, or for VK to say there are none.
func (s *Searcher) results(ctx context.Context) (resultsPage, string, string) {
	var page resultsPage
	ready := browser.WaitUntil(ctx, s.opts.ResultsTimeout, 500*time.Millisecond, func() bool {
		page = resultsPage{}
		if err := s.page.EvalExpr(ctx, "("+jsResults+")()", &page); err != nil {
			return false
		}
		return len(page.Rows) > 0 || page.Empty
	})
	if ready {
		return page, "", ""
	}
	if ctx.Err() != nil {
		return page, "cancelled", ""
	}
	if status, detail := s.pageState(ctx); status != "" {
		return page, status, detail
	}
	return page, "nav_error", "результаты поиска не загрузились"
}

// pageState classifies the page. Empty status means it looks usable.
func (s *Searcher) pageState(ctx context.Context) (status, detail string) {
	var info struct {
		URL          string `json:"url"`
		Text         string `json:"text"`
		UserID       int64  `json:"user_id"`
		CaptchaFrame bool   `json:"captcha_frame"`
	}
	if err := s.page.EvalExpr(ctx, "("+jsPageState+")()", &info); err != nil {
		return "", ""
	}
	return classifyPage(info.URL, info.Text, info.UserID, info.CaptchaFrame)
}

func classifyPage(pageURL, text string, userID int64, captchaFrame bool) (string, string) {
	if marker := firstMarker(text, captchaMarkers); captchaFrame || marker != "" {
		if captchaFrame {
			return "captcha_or_challenge", "капча на странице"
		}
		return "captcha_or_challenge", "на странице: «" + marker + "»"
	}
	if marker := firstMarker(text, rateLimitMarkers); marker != "" {
		return "rate_limited", "на странице: «" + marker + "»"
	}
	host := hostOf(pageURL)
	if userID == 0 || strings.HasPrefix(host, "id.vk.") || strings.Contains(pageURL, "/login") {
		return "not_logged_in", "требуется вход"
	}
	return "", ""
}

// Candidates picks the rows whose name is the person's — surname and first
// name both — and grades them: high when the snippet also shows the city or
// the company, medium on the name alone.
func Candidates(person string, surnameFirst bool, city, company string, rows []Row, limit int) []Candidate {
	var out []Candidate
	seen := map[string]bool{}
	for _, row := range rows {
		profile := ProfileURL(row.Path)
		if profile == "" || seen[profile] {
			continue
		}
		if names.Confidence(person, surnameFirst, 8, "", row.Name, "") != "high" {
			continue
		}
		seen[profile] = true
		snippet := Snippet(row)
		c := Candidate{ProfileURL: profile, Name: row.Name, Snippet: snippet, Score: 80, Confidence: "medium"}
		switch {
		case company != "" && companyMatches(company, snippet):
			c.Score, c.Confidence = 100, "high"
		case city != "" && cityMatches(city, snippet):
			c.Score, c.Confidence = 90, "high"
		}
		out = append(out, c)
	}
	// High before medium, page order within each.
	ordered := make([]Candidate, 0, len(out))
	for _, want := range []string{"high", "medium"} {
		for _, c := range out {
			if c.Confidence == want {
				ordered = append(ordered, c)
			}
		}
	}
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	return ordered
}

// Snippet is a row's text without its name and buttons.
func Snippet(row Row) string {
	var parts []string
	for _, line := range row.Lines {
		line = strings.TrimSpace(line)
		if line == "" || line == row.Name || actionLabels[strings.ToLower(line)] {
			continue
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, " · ")
}

// ProfileURL is the canonical address of a result's profile path, or empty
// when the path is not a profile.
func ProfileURL(path string) string {
	handle := strings.Trim(path, "/")
	if handle == "" || strings.Contains(handle, "/") {
		return ""
	}
	return "https://vk.com/" + handle
}

// legalForms are words of a registered company name that its people do not
// write when they name their workplace.
var legalForms = map[string]bool{
	"ооо": true, "ао": true, "пао": true, "зао": true, "оао": true, "нао": true,
	"ип": true, "ано": true, "нко": true, "фгбу": true, "гбу": true, "гбуз": true,
	"гуп": true, "муп": true, "фгуп": true, "llc": true, "ltd": true, "inc": true,
	"группа": true, "компаний": true, "гк": true,
}

func companyMatches(company, snippet string) bool {
	text := " " + names.Normalize(snippet) + " "
	var words []string
	for _, w := range names.NameTokens(company) {
		if !legalForms[w] {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return false
	}
	return strings.Contains(text, " "+strings.Join(words, " ")+" ")
}

func cityMatches(city, snippet string) bool {
	want := names.Normalize(city)
	return want != "" && strings.Contains(" "+names.Normalize(snippet)+" ", " "+want+" ")
}

// IsVKURL reports whether a page is on VK, under either of its domains.
func IsVKURL(raw string) bool {
	host := hostOf(raw)
	return host == "vk.com" || host == "vk.ru" || host == "m.vk.com" || host == "m.vk.ru"
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

func classifyNavError(message string) (string, string) {
	if firstMarker(strings.ToLower(message), networkErrorMarkers) != "" {
		return "network_error", "страница не загрузилась (возможны VPN, прокси, регион или нет связи): " + truncate(message, 160)
	}
	return "nav_error", truncate(message, 200)
}

func firstMarker(text string, markers []string) string {
	text = strings.ToLower(text)
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return marker
		}
	}
	return ""
}

// titleCase writes a registry's «ИВАН» as «Иван».
func titleCase(word string) string {
	r := []rune(strings.ToLower(word))
	if len(r) > 0 {
		r[0] = unicode.ToUpper(r[0])
	}
	return string(r)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
