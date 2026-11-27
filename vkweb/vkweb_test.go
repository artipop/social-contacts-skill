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

func withCompanyFilter() Options {
	o := quick()
	o.CompanyFilter = true
	return o
}

func TestTheCompanyFilterIsOptIn(t *testing.T) {
	page := newFakePage(t, "results.json", "empty.json")
	s, _ := Open(context.Background(), page, quick())
	res := s.Search(context.Background(), "Пётр Заречный", "", "ООО «Ромашка»")
	if len(page.visited) != 1 || len(page.applied) != 0 {
		t.Fatalf("visited %v, filters %v: without CompanyFilter only the name is searched", page.visited, page.applied)
	}
	if res.Query != (Query{Name: "Пётр Заречный"}) || len(res.Candidates) == 0 || res.Candidates[0].Confidence != "high" {
		t.Errorf("the place of work still counts from the snippet: %+v", res)
	}
}

func quick() Options {
	return Options{Pause: time.Millisecond, Jitter: -1, ResultsTimeout: 50 * time.Millisecond}
}

func TestOnlyFirstNameAndSurnameTogetherAreCandidates(t *testing.T) {
	page := newFakePage(t, "results.json", "results.json")
	s, err := Open(context.Background(), page, withCompanyFilter())
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
	// The city filter puts the city on every row, so it is no evidence, and
	// four namesakes are four leads, not the person.
	if len(res.Candidates) != 4 {
		t.Fatalf("candidates = %+v", res.Candidates)
	}
	for _, c := range res.Candidates {
		if c.Confidence != "low" || !strings.Contains(c.Reason, "4 тёзок") {
			t.Errorf("%s: confidence %q (%s) for one of several namesakes", c.ProfileURL, c.Confidence, c.Reason)
		}
	}
}

func TestOnlyThePlaceOfWorkRaisesConfidence(t *testing.T) {
	rows := []Row{
		{Path: "/id1", Name: "Пётр Заречный", Lines: []string{"Пётр Заречный", "36 лет, Москва"}},
		{Path: "/id2", Name: "Пётр Заречный", Lines: []string{"Пётр Заречный", "Ромашка, Москва"}},
	}
	got := Candidates("Пётр Заречный", false, "Ромашка", Query{Name: "Пётр Заречный", City: "Москва"}, rows, 10)
	if len(got) != 2 || got[0].ProfileURL != "https://vk.com/id2" || got[0].Confidence != "high" || got[1].Confidence != "low" {
		t.Errorf("candidates = %+v", got)
	}
	if got[0].Reason == "" || got[1].Reason == "" {
		t.Errorf("a candidate without a reason: %+v", got)
	}
}

func TestConfidenceByWhatTheSearchFound(t *testing.T) {
	row := func(path, snippet string) Row {
		return Row{Path: path, Name: "Пётр Заречный", Lines: []string{"Пётр Заречный", snippet}}
	}
	inCity := Query{Name: "Пётр Заречный", City: "Москва"}
	byCompany := Query{Name: "Пётр Заречный", Company: "Инноватика"}
	cases := []struct {
		name, company string
		q             Query
		rows          []Row
		want          []string
	}{
		{"the only namesake in the city", "Клиника Инноватика", inCity,
			[]Row{row("/id1", "Москва")}, []string{"medium"}},
		{"several namesakes in the city", "", inCity,
			[]Row{row("/id1", "Москва"), row("/id2", "Москва")}, []string{"low", "low"}},
		{"the place of work in Latin", "ООО «КЛИНИКА ИННОВАТИКА»", inCity,
			[]Row{row("/id1", "INNOVATICA, Москва"), row("/id2", "Москва")}, []string{"high", "low"}},
		{"found by the place-of-work filter", "КЛИНИКА ИННОВАТИКА", byCompany,
			[]Row{row("/id1", "Москва")}, []string{"medium"}},
		{"by name alone", "", Query{Name: "Пётр Заречный"},
			[]Row{row("/id1", "")}, []string{"low"}},
		{"a generic word is not the place of work", "Медицинский центр Здоровье", inCity,
			[]Row{row("/id1", "Медицинский центр Альфа"), row("/id2", "Москва")}, []string{"low", "low"}},
	}
	for _, c := range cases {
		got := Candidates("Пётр Заречный", false, c.company, c.q, c.rows, 10)
		var confidences []string
		for _, cand := range got {
			confidences = append(confidences, cand.Confidence)
			if cand.Reason == "" {
				t.Errorf("%s: no reason for %s", c.name, cand.ProfileURL)
			}
		}
		if strings.Join(confidences, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: confidences %v, want %v", c.name, confidences, c.want)
		}
	}
}

func TestCompanyFirstThenCity(t *testing.T) {
	page := newFakePage(t, "results.json", "empty.json")
	s, _ := Open(context.Background(), page, withCompanyFilter())
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
	s, _ := Open(context.Background(), page, withCompanyFilter())
	s.Search(context.Background(), "Пётр Заречный", "", "Ромашка и сыновья")
	if len(page.visited) != 1 || len(page.applied) != 1 {
		t.Fatalf("visited %v, filters %v", page.visited, page.applied)
	}
	if u := page.visited[0]; strings.Contains(u, "+") || !strings.Contains(u, "%20") {
		t.Errorf("search address %q: VK reads '+' as a plus sign", u)
	}
	if page.applied[0]["company"] != "Ромашка Сыновья" {
		t.Errorf("filters = %v", page.applied[0])
	}
	if got := SearchPageURL("A+B C"); got != SearchURL+"?q=A%2BB%20C" {
		t.Errorf("SearchPageURL = %q", got)
	}
}

func TestNothingFoundSkipsTheFilters(t *testing.T) {
	page := newFakePage(t, "empty.json", "results.json")
	s, _ := Open(context.Background(), page, withCompanyFilter())
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
	if got := Candidates("Пётр Заречный", false, "", Query{}, rows, 10); len(got) != 10 {
		t.Errorf("candidates = %d", len(got))
	}
}

func TestCompanyMatching(t *testing.T) {
	cases := []struct {
		company, snippet string
		want             bool
	}{
		{"ООО «Ромашка»", "Ромашка, Москва", true},
		{"Нв-медика", "НВ Медика", true},
		{"Медси", "Медсити", false},
		{"ООО", "ООО Лютик", false},
		{"КЛИНИКА ИННОВАТИКА", "INNOVATICA", true},
		{"ООО \"КЛИНИКА ИННОВАТИКА\"", "Клиника «Инноватика», Москва", true},
		{"Общество с ограниченной ответственностью «Смарт Клиник»", "Smart Clinic", true},
		{"Стоматология Улыбка", "Улыбка, Москва", true},
		{"Стоматология Улыбка", "Стоматология Жемчуг", false},
		{"Медицинский центр Здоровье", "Медицинский центр, Москва", false},
		{"Семейный доктор", "Семейный доктор", true},
		{"Мед Свисс", "MedSwiss", true},
		{"Инноватика", "работает в Инноватике", true},
		{"Инноватика", "Москва, 36 лет", false},
	}
	for _, c := range cases {
		if got := CompanyMatches(c.company, c.snippet); got != c.want {
			t.Errorf("CompanyMatches(%q, %q) = %v", c.company, c.snippet, got)
		}
	}
}

func TestCompanyQueryDropsLegalFormAndGenericWords(t *testing.T) {
	cases := map[string]string{
		`ООО "КЛИНИКА ИННОВАТИКА"`: "Инноватика",
		"ООО «Ромашка»":            "Ромашка",
		"Медицинский центр":        "Медицинский Центр",
		"ООО":                      "",
		"АО «Медси 2»":             "Медси 2",
	}
	for company, want := range cases {
		if got := CompanyQuery(company); got != want {
			t.Errorf("CompanyQuery(%q) = %q, want %q", company, got, want)
		}
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
