package vkweb

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// fakePage is a VK tab: the page state and the results come from fixtures,
// and every address it is sent to is recorded.
type fakePage struct {
	t       *testing.T
	state   map[string]any
	navErr  error
	visited []string
	// results answers jsResults: before the filters are applied, and after.
	results, filtered string
	applied           []map[string]string
	navTimes          []time.Time
}

func newFakePage(t *testing.T, results, filtered string) *fakePage {
	return &fakePage{
		t:       t,
		state:   map[string]any{"url": "https://vk.ru/feed", "user_id": 1},
		results: results, filtered: filtered,
	}
}

func (p *fakePage) URL(context.Context) (string, error) { return "https://vk.ru/feed", nil }
func (p *fakePage) Close()                              {}

func (p *fakePage) Navigate(_ context.Context, url string) error {
	p.visited = append(p.visited, url)
	p.navTimes = append(p.navTimes, time.Now())
	p.applied = nil
	return p.navErr
}

func (p *fakePage) Eval(_ context.Context, fn string, arg any, out any) error {
	if fn != jsApplyFilters {
		return errors.New("unexpected function")
	}
	filters := arg.(map[string]string)
	p.applied = append(p.applied, filters)
	return remarshal(map[string]any{"ok": true}, out)
}

func (p *fakePage) EvalExpr(_ context.Context, expression string, out any) error {
	switch expression {
	case "(" + jsPageState + ")()":
		return remarshal(p.state, out)
	case "(" + jsResults + ")()":
		name := p.results
		if len(p.applied) > 0 {
			name = p.filtered
		}
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			p.t.Fatal(err)
		}
		return json.Unmarshal(data, out)
	}
	return errors.New("unexpected expression")
}

func remarshal(v, out any) error {
	data, _ := json.Marshal(v)
	return json.Unmarshal(data, out)
}

func quick() Options {
	return Options{Pause: time.Millisecond, Jitter: -1, ResultsTimeout: 50 * time.Millisecond}
}

func TestOnlyFirstNameAndSurnameTogetherAreCandidates(t *testing.T) {
	page := newFakePage(t, "results.json", "results.json")
	s, err := Open(context.Background(), page, quick())
	if err != nil {
		t.Fatal(err)
	}
	res := s.Search(context.Background(), "Пётр Заречный", "", "Ромашка")
	// Not Пётр Семёнов (first name only), not Олег Заречный (surname only),
	// and the repeated row once.
	want := map[string]string{
		"https://vk.com/zarechny.p":  "high",
		"https://vk.com/id100000001": "medium",
		"https://vk.com/id100000003": "medium",
		"https://vk.com/id100000004": "medium",
	}
	if res.Status != "ok" || len(res.Candidates) != len(want) {
		t.Fatalf("result = %+v", res)
	}
	if res.Candidates[0].ProfileURL != "https://vk.com/zarechny.p" {
		t.Errorf("the company match is not first: %+v", res.Candidates)
	}
	for _, c := range res.Candidates {
		if want[c.ProfileURL] != c.Confidence {
			t.Errorf("%s: confidence %q, want %q", c.ProfileURL, c.Confidence, want[c.ProfileURL])
		}
	}
	if got := res.Candidates[0].Snippet; got != "ООО «Ромашка», 1 602 подписчика" {
		t.Errorf("snippet = %q", got)
	}
	if res.Query != (Query{Name: "Пётр Заречный", Company: "Ромашка"}) {
		t.Errorf("query = %+v", res.Query)
	}
}

func TestSurnameFirstNamesAreReadAsRegistryNames(t *testing.T) {
	page := newFakePage(t, "results.json", "results.json")
	s, _ := Open(context.Background(), page, Options{SurnameFirst: true, Pause: time.Millisecond, Jitter: -1})
	res := s.Search(context.Background(), "ЗАРЕЧНЫЙ ПЁТР ИЛЬИЧ", "Москва", "")
	if res.Query.Name != "Пётр Заречный" {
		t.Errorf("query name = %q", res.Query.Name)
	}
	if len(res.Candidates) == 0 || res.Candidates[0].ProfileURL != "https://vk.com/id100000003" ||
		res.Candidates[0].Confidence != "high" {
		t.Errorf("the city match is not first and high: %+v", res.Candidates)
	}
}

func TestCompanyFirstThenCity(t *testing.T) {
	page := newFakePage(t, "results.json", "empty.json")
	s, _ := Open(context.Background(), page, quick())
	res := s.Search(context.Background(), "Пётр Заречный", "Москва", "Ромашка")
	if len(page.visited) != 2 {
		t.Fatalf("visited %v", page.visited)
	}
	if res.Status != "not_found" || res.Query.City != "Москва" || res.Query.Company != "" {
		t.Errorf("result = %+v", res)
	}
}

func TestTheNameGoesWithSpacesNotPluses(t *testing.T) {
	page := newFakePage(t, "results.json", "results.json")
	s, _ := Open(context.Background(), page, quick())
	s.Search(context.Background(), "Пётр Заречный", "", "Ромашка и сыновья")
	if len(page.visited) != 1 || len(page.applied) != 1 {
		t.Fatalf("visited %v, filters %v", page.visited, page.applied)
	}
	if u := page.visited[0]; strings.Contains(u, "+") || !strings.Contains(u, "%20") {
		t.Errorf("search address %q: VK reads '+' as a plus sign", u)
	}
	if page.applied[0]["company"] != "Ромашка и сыновья" {
		t.Errorf("filters = %v", page.applied[0])
	}
	if got := SearchPageURL("A+B C"); got != SearchURL+"?q=A%2BB%20C" {
		t.Errorf("SearchPageURL = %q", got)
	}
}

func TestNothingFoundSkipsTheFilters(t *testing.T) {
	page := newFakePage(t, "empty.json", "results.json")
	s, _ := Open(context.Background(), page, quick())
	res := s.Search(context.Background(), "Пётр Заречный", "", "Ромашка")
	if res.Status != "not_found" || len(page.applied) != 0 {
		t.Errorf("result = %+v, filters %v", res, page.applied)
	}
}

func TestACaptchaIsBlockedNotNobody(t *testing.T) {
	page := newFakePage(t, "results.json", "results.json")
	s, _ := Open(context.Background(), page, quick())
	page.state = map[string]any{"url": "https://vk.ru/search/people", "user_id": 1, "text": "подтвердите, что вы не робот"}
	res := s.Search(context.Background(), "Пётр Заречный", "", "")
	if res.Status != "captcha_or_challenge" || !res.Blocked() {
		t.Errorf("captcha = %+v", res)
	}
}

func TestLoggedOutStopsBeforeSearching(t *testing.T) {
	page := newFakePage(t, "results.json", "results.json")
	page.state = map[string]any{"url": "https://vk.ru/", "user_id": 0}
	if _, err := Open(context.Background(), page, quick()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("logged out = %v", err)
	}
}

func TestANetworkFailureNamesTheLikelyCause(t *testing.T) {
	page := newFakePage(t, "results.json", "results.json")
	s, _ := Open(context.Background(), page, quick())
	page.navErr = errors.New("net::ERR_TUNNEL_CONNECTION_FAILED")
	if res := s.Search(context.Background(), "Пётр Заречный", "", ""); res.Status != "network_error" || !res.Blocked() {
		t.Errorf("network error = %+v", res)
	}
}

func TestResultsThatNeverRenderAreBlocked(t *testing.T) {
	page := newFakePage(t, "pending.json", "pending.json")
	s, _ := Open(context.Background(), page, quick())
	if res := s.Search(context.Background(), "Пётр Заречный", "", ""); res.Status != "nav_error" || !res.Blocked() {
		t.Errorf("never rendered = %+v", res)
	}
}

func TestOneNameIsNotSearched(t *testing.T) {
	page := newFakePage(t, "results.json", "results.json")
	s, _ := Open(context.Background(), page, quick())
	if res := s.Search(context.Background(), "Пётр", "", ""); res.Status != "invalid_name" || len(page.visited) != 0 {
		t.Errorf("one name = %+v, visited %v", res, page.visited)
	}
}

func TestSearchesArePaced(t *testing.T) {
	page := newFakePage(t, "results.json", "results.json")
	opts := quick()
	opts.Pause = 80 * time.Millisecond
	s, _ := Open(context.Background(), page, opts)
	s.Search(context.Background(), "Пётр Заречный", "", "")
	s.Search(context.Background(), "Олег Заречный", "", "")
	if gap := page.navTimes[1].Sub(page.navTimes[0]); gap < opts.Pause {
		t.Errorf("searches %v apart, want at least %v", gap, opts.Pause)
	}
}

func TestCandidatesAreCapped(t *testing.T) {
	var rows []Row
	for i := range 15 {
		path := "/id" + strings.Repeat("1", i+1)
		rows = append(rows, Row{Path: path, Name: "Пётр Заречный", Lines: []string{"Пётр Заречный"}})
	}
	if got := Candidates("Пётр Заречный", false, "", "", rows, 10); len(got) != 10 {
		t.Errorf("candidates = %d", len(got))
	}
}

func TestCompanyAndCityMatchWholeWords(t *testing.T) {
	cases := []struct {
		company, snippet string
		want             bool
	}{
		{"ООО «Ромашка»", "Ромашка, Москва", true},
		{"Нв-медика", "НВ Медика", true},
		{"Медси", "Медсити", false},
		{"ООО", "ООО Лютик", false},
	}
	for _, c := range cases {
		if got := companyMatches(c.company, c.snippet); got != c.want {
			t.Errorf("companyMatches(%q, %q) = %v", c.company, c.snippet, got)
		}
	}
	if !cityMatches("Москва", "33 года, Москва") || cityMatches("Москва", "Наша Москвария") {
		t.Error("city match")
	}
}

func TestStatesTellApartWhatLooksTheSame(t *testing.T) {
	cases := []struct {
		url, text string
		user      int64
		want      string
	}{
		{"https://vk.ru/search/people", "поиск людей", 1, ""},
		{"https://vk.ru/search/people", "слишком много запросов", 1, "rate_limited"},
		{"https://id.vk.ru/auth", "войти", 0, "not_logged_in"},
		{"https://vk.ru/", "", 0, "not_logged_in"},
	}
	for _, c := range cases {
		if got, _ := classifyPage(c.url, c.text, c.user, false); got != c.want {
			t.Errorf("classifyPage(%q, %q) = %q, want %q", c.url, c.text, got, c.want)
		}
	}
	if got, _ := classifyPage("https://vk.ru/", "", 1, true); got != "captcha_or_challenge" {
		t.Errorf("captcha frame = %q", got)
	}
	if !IsVKURL("https://vk.ru/feed") || !IsVKURL("https://vk.com/search/people") || IsVKURL("https://vk.co/") {
		t.Error("IsVKURL")
	}
}
