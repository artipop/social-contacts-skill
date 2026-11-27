package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/artipop/social-contacts-skill/cdp"
	"github.com/artipop/social-contacts-skill/ig"
	"github.com/artipop/social-contacts-skill/record"
	"github.com/artipop/social-contacts-skill/tg"
	"github.com/artipop/social-contacts-skill/vkweb"
)

// fakePage is a tab that answers Telegram's login check and bridge ops from
// a function, so the real collector can run on it.
type fakePage struct {
	mu     sync.Mutex
	id     string
	err    error
	closed bool
	bridge func(params map[string]any) map[string]any
}

func (p *fakePage) URL(context.Context) (string, error)         { return "https://web.telegram.org/k/", nil }
func (p *fakePage) Navigate(context.Context, string) error      { return nil }
func (p *fakePage) EvalExpr(context.Context, string, any) error { return nil }
func (p *fakePage) TargetID() string                            { return p.id }

func (p *fakePage) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
}

func (p *fakePage) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *fakePage) lose(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func (p *fakePage) Eval(_ context.Context, _ string, arg any, out any) error {
	var value any = true // the login check
	if params, ok := arg.(map[string]any); ok {
		if params["op"] == "whoami" {
			value = map[string]any{"ok": true, "my_id": "1"}
		} else {
			value = p.bridge(params)
		}
	}
	data, _ := json.Marshal(value)
	return json.Unmarshal(data, out)
}

// fakeBrowser is the browser connection: it counts connections and hands out
// one page per site.
type fakeBrowser struct {
	mu        sync.Mutex
	connects  int
	connErr   error
	pages     map[string]*fakePage
	activated []string
	closed    bool
}

func (b *fakeBrowser) Version(context.Context) (cdp.Version, error) {
	return cdp.Version{Product: "Chrome/144.0.0.0"}, nil
}

func (b *fakeBrowser) Page(_ context.Context, site cdp.Site) (page, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p, ok := b.pages[site.URL]; ok {
		return p, nil
	}
	p := &fakePage{id: fmt.Sprintf("T%d", len(b.pages)+1)}
	b.pages[site.URL] = p
	return p, nil
}

func (b *fakeBrowser) Activate(_ context.Context, id string) error {
	b.activated = append(b.activated, id)
	return nil
}

func (b *fakeBrowser) Close() error {
	b.closed = true
	return nil
}

// fakeTelegram answers Collect from a table, by link.
type fakeTelegram struct {
	results map[string]tg.Result
	calls   []string
	during  func(link string) // runs inside Collect, to break things mid-run
}

func (c *fakeTelegram) Collect(_ context.Context, link string) tg.Result {
	c.calls = append(c.calls, link)
	if c.during != nil {
		c.during(link)
	}
	if res, ok := c.results[link]; ok {
		return res
	}
	return tg.Result{Channel: link, Status: "members_not_found"}
}

func (c *fakeTelegram) Close() {}

type fakeInstagram struct {
	results map[string]ig.Result
	calls   []string
}

func (s *fakeInstagram) Search(_ context.Context, link string, people []string) ig.Result {
	s.calls = append(s.calls, link+" "+strings.Join(people, "+"))
	if res, ok := s.results[link]; ok {
		return res
	}
	return ig.Result{Profile: link, Status: "ok"}
}

func (s *fakeInstagram) Close() {}

type harness struct {
	app            *app
	stdout, stderr *bytes.Buffer
	browser        *fakeBrowser
	telegram       *fakeTelegram
	instagram      *fakeInstagram
	vkweb          *fakeVKWeb
	vkwebOpts      vkwebOptions
	env            map[string]string
	openErr        error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		browser:   &fakeBrowser{pages: map[string]*fakePage{}},
		telegram:  &fakeTelegram{results: map[string]tg.Result{}},
		instagram: &fakeInstagram{results: map[string]ig.Result{}},
		vkweb:     &fakeVKWeb{results: map[string]vkweb.Result{}},
		env:       map[string]string{},
	}
	h.app = &app{
		stdin: strings.NewReader(""), stdout: h.stdout, stderr: h.stderr,
		getenv: func(k string) string { return h.env[k] },
		connect: func(_ context.Context, _ browserFlags, _ func()) (session, error) {
			h.browser.mu.Lock()
			defer h.browser.mu.Unlock()
			h.browser.connects++
			if h.browser.connErr != nil {
				return nil, h.browser.connErr
			}
			return h.browser, nil
		},
		openTelegram: func(context.Context, page, tgOptions) (telegramCollector, error) {
			if h.openErr != nil {
				return nil, h.openErr
			}
			return h.telegram, nil
		},
		openInstagram: func(context.Context, page, igOptions) (instagramSearcher, error) {
			if h.openErr != nil {
				return nil, h.openErr
			}
			return h.instagram, nil
		},
		openVKWeb: func(_ context.Context, _ page, opts vkwebOptions) (vkWebSearcher, error) {
			h.vkwebOpts = opts
			if h.openErr != nil {
				return nil, h.openErr
			}
			return h.vkweb, nil
		},
	}
	return h
}

func (h *harness) run(t *testing.T, args ...string) int {
	t.Helper()
	return h.app.run(context.Background(), args)
}

func lines(t *testing.T, data string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not a JSON line: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func found(link string, n int) tg.Result {
	members := make([]tg.Member, n)
	for i := range members {
		members[i] = tg.Member{UserID: fmt.Sprint(i + 1), Name: "Человек"}
	}
	return tg.Result{Channel: link, Members: members, Total: n, Status: "ok"}
}

func TestEachLineIsTaggedWithItsSourceOverOneConnection(t *testing.T) {
	h := newHarness(t)
	h.telegram.results["https://t.me/acme"] = found("https://t.me/acme", 2)
	input := writeFile(t, "leads.csv", "company,telegram\nAcme,https://t.me/acme\nBeta,@beta_group\nBot,@helper_bot\n")

	if code := h.run(t, "tg-members", "--input", input); code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, h.stderr)
	}
	got := lines(t, h.stdout.String())
	if len(got) != 3 {
		t.Fatalf("%d lines:\n%s", len(got), h.stdout)
	}
	if got[0]["source"] != "https://t.me/acme" || got[0]["status"] != "ok" || len(got[0]["members"].([]any)) != 2 {
		t.Errorf("first line = %v", got[0])
	}
	if got[1]["source"] != "@beta_group" || got[1]["status"] != "members_not_found" {
		t.Errorf("second line = %v", got[1])
	}
	if got[2]["source"] != "@helper_bot" || got[2]["status"] != "unsupported_url" {
		t.Errorf("a bot is not collected: %v", got[2])
	}
	if h.browser.connects != 1 || !h.browser.closed {
		t.Errorf("connections = %d, closed %v; want one, closed", h.browser.connects, h.browser.closed)
	}
	if len(h.telegram.calls) != 2 {
		t.Errorf("collected %v", h.telegram.calls)
	}
}

func TestResumeSkipsSettledInputsAndRetriesTheRest(t *testing.T) {
	h := newHarness(t)
	out := filepath.Join(t.TempDir(), "out.jsonl")
	// Done: one found, one with no members. Not done: a flood-wait and a
	// torn last line from a killed run.
	previous := []any{
		record.Telegram{Source: "@done_found", Result: found("@done_found", 1)},
		record.Telegram{Source: "@done_empty", Result: tg.Result{Channel: "@done_empty", Status: "members_not_found"}},
		record.Telegram{Source: "@flooded", Result: tg.Result{Channel: "@flooded", Status: "members_not_found", Error: "FLOOD_WAIT_900"}},
	}
	var b strings.Builder
	for _, rec := range previous {
		data, _ := json.Marshal(rec)
		b.Write(data)
		b.WriteByte('\n')
	}
	b.WriteString(`{"source":"@torn","channel":"@to`)
	if err := os.WriteFile(out, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	h.telegram.results["@flooded"] = found("@flooded", 3)
	h.telegram.results["@torn"] = found("@torn", 1)
	input := writeFile(t, "in.jsonl", `"@done_found"`+"\n"+`"@done_empty"`+"\n"+`"@flooded"`+"\n"+`"@torn"`+"\n")

	if code := h.run(t, "tg-members", "--input", input, "--out", out); code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, h.stderr)
	}
	if strings.Join(h.telegram.calls, ",") != "@flooded,@torn" {
		t.Fatalf("collected %v, want only the unsettled inputs", h.telegram.calls)
	}
	if h.stdout.Len() != 0 {
		t.Errorf("records went to stdout as well: %s", h.stdout)
	}
	content := readFile(t, out)
	var sources []string
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			sources = append(sources, m["source"].(string))
		}
	}
	if strings.Join(sources, ",") != "@done_found,@done_empty,@flooded,@flooded,@torn" {
		t.Errorf("sources in the file = %v", sources)
	}

	// A third run has nothing left to do and does not touch the browser.
	h2 := newHarness(t)
	if code := h2.run(t, "tg-members", "--input", input, "--out", out); code != exitOK {
		t.Fatalf("exit %d on a finished file; stderr:\n%s", code, h2.stderr)
	}
	if h2.browser.connects != 0 || len(h2.telegram.calls) != 0 {
		t.Errorf("a finished run connected %d times and collected %v", h2.browser.connects, h2.telegram.calls)
	}
}

func TestExitCodesSummarizeTheRun(t *testing.T) {
	cases := []struct {
		name    string
		results map[string]tg.Result
		want    int
	}{
		{"something found", map[string]tg.Result{"@a_group": found("@a_group", 1)}, exitOK},
		{"nobody found", map[string]tg.Result{}, exitNotFound},
		{"blocked", map[string]tg.Result{
			"@a_group": found("@a_group", 1),
			"@b_group": {Channel: "@b_group", Status: "no_linked_discussion"},
		}, exitBlocked},
		{"failed", map[string]tg.Result{
			"@a_group": found("@a_group", 1),
			"@b_group": {Channel: "@b_group", Status: "resolve_failed", Error: "managers_not_ready"},
		}, exitError},
	}
	for _, c := range cases {
		h := newHarness(t)
		h.telegram.results = c.results
		input := writeFile(t, "in.csv", "telegram\n@a_group\n@b_group\n")
		if code := h.run(t, "tg-members", "--input", input); code != c.want {
			t.Errorf("%s: exit %d, want %d; stderr:\n%s", c.name, code, c.want, h.stderr)
		}
	}
}

func TestBrowserUnavailable(t *testing.T) {
	for _, err := range []error{cdp.ErrNotEnabled, cdp.ErrDenied, cdp.ErrApprovalPending, cdp.ErrProfileUnreadable} {
		h := newHarness(t)
		h.browser.connErr = fmt.Errorf("%w (HTTP 403)", err)
		if code := h.run(t, "tg-members", "@a_group"); code != exitBrowser {
			t.Errorf("%v: exit %d, want %d", err, code, exitBrowser)
		}
		if h.stdout.Len() != 0 {
			t.Errorf("%v: records written: %s", err, h.stdout)
		}
	}
}

func TestLoggedOutBeforeCollecting(t *testing.T) {
	h := newHarness(t)
	h.openErr = tg.ErrNotLoggedIn
	if code := h.run(t, "tg-members", "@a_group"); code != exitLogin {
		t.Fatalf("exit %d, want %d", code, exitLogin)
	}
	h = newHarness(t)
	h.openErr = fmt.Errorf("%w (требуется вход)", ig.ErrNotLoggedIn)
	if code := h.run(t, "ig-find", "instagram.com/acme", "--person", "Иванов Иван"); code != exitLogin {
		t.Fatalf("instagram: exit %d, want %d", code, exitLogin)
	}
}

func TestALoginWallMidRunStopsWithoutARecord(t *testing.T) {
	h := newHarness(t)
	h.instagram.results["instagram.com/acme"] = ig.Result{Profile: "instagram.com/acme", Status: "ok",
		Matches: []ig.Match{{Person: "Иванов Иван", Username: "ivanov"}}}
	h.instagram.results["instagram.com/beta"] = ig.Result{Profile: "instagram.com/beta", Status: "login_required"}
	input := writeFile(t, "in.csv", "instagram,people\ninstagram.com/acme,Иванов Иван\ninstagram.com/beta,Петров Пётр\ninstagram.com/gamma,Сидоров Сидор\n")
	if code := h.run(t, "ig-find", "--input", input); code != exitLogin {
		t.Fatalf("exit %d, want %d", code, exitLogin)
	}
	got := lines(t, h.stdout.String())
	if len(got) != 1 || got[0]["source"] != "instagram.com/acme" {
		t.Fatalf("records = %v", got)
	}
	if len(h.instagram.calls) != 2 || h.instagram.calls[0] != "instagram.com/acme Иванов Иван" {
		t.Errorf("searched %v", h.instagram.calls)
	}
}

func TestInstagramBlockedIsNotNothingFound(t *testing.T) {
	h := newHarness(t)
	h.instagram.results["instagram.com/acme"] = ig.Result{Profile: "instagram.com/acme", Status: "captcha_or_challenge", Error: "капча"}
	if code := h.run(t, "ig-find", "instagram.com/acme", "--person", "Иванов Иван"); code != exitBlocked {
		t.Fatalf("exit %d, want %d", code, exitBlocked)
	}
	h = newHarness(t)
	if code := h.run(t, "ig-find", "instagram.com/acme", "--person", "Иванов Иван"); code != exitNotFound {
		t.Fatalf("exit %d, want %d", code, exitNotFound)
	}
}

func TestPeopleAreRequiredForASearch(t *testing.T) {
	h := newHarness(t)
	if code := h.run(t, "ig-find", "instagram.com/acme"); code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
	if h.browser.connects != 0 {
		t.Error("connected without anyone to search for")
	}
}

func TestLosingTheBrowserMidRunStops(t *testing.T) {
	h := newHarness(t)
	h.telegram.results["@a_group"] = found("@a_group", 1)
	h.telegram.during = func(link string) {
		if link == "@b_group" {
			h.browser.pages[cdp.Telegram.URL].lose(cdp.ErrDisconnected)
		}
	}
	input := writeFile(t, "in.csv", "telegram\n@a_group\n@b_group\n@c_group\n")
	if code := h.run(t, "tg-members", "--input", input); code != exitBrowser {
		t.Fatalf("exit %d, want %d", code, exitBrowser)
	}
	if got := lines(t, h.stdout.String()); len(got) != 1 {
		t.Fatalf("records = %v; the channel cut off must not be written", got)
	}
}

func TestInterruptedRunWritesNothingHalfDone(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.telegram.during = func(string) { cancel() }
	if code := h.app.run(ctx, []string{"tg-members", "@a_group"}); code != exitInterrupted {
		t.Fatalf("exit %d, want %d", code, exitInterrupted)
	}
	if h.stdout.Len() != 0 {
		t.Errorf("records written: %s", h.stdout)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"nope"},
		{"tg-members"},
		{"tg-members", "--bogus"},
		{"open"},
		{"open", "myspace"},
		{"status", "myspace"},
	} {
		h := newHarness(t)
		if code := h.run(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
	h := newHarness(t)
	if code := h.run(t, "tg-members", "-h"); code != exitOK {
		t.Errorf("help: exit %d", code)
	}
}

// The real Telegram collector, driven through a fake tab.
func TestTelegramCollectorThroughAFakeTab(t *testing.T) {
	h := newHarness(t)
	h.app.openTelegram = openTelegram
	h.browser.pages[cdp.Telegram.URL] = &fakePage{id: "P", bridge: func(params map[string]any) map[string]any {
		switch params["op"] {
		case "resolve":
			return map[string]any{"ok": true, "id": "100", "is_megagroup": true, "username": "somegroup"}
		case "participants":
			return map[string]any{"ok": true, "count": 1, "returned": 1, "members": []any{
				map[string]any{"user_id": "7", "name": "Анна", "username": "anna_example"},
			}}
		}
		return map[string]any{"ok": false, "error": "unknown_op"}
	}}
	if code := h.run(t, "tg-members", "https://t.me/somegroup"); code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, h.stderr)
	}
	var rec record.Telegram
	if err := json.Unmarshal(bytes.TrimSpace(h.stdout.Bytes()), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Source != "https://t.me/somegroup" || len(rec.Members) != 1 || rec.Members[0].Username != "anna_example" {
		t.Fatalf("record = %+v", rec)
	}
	if !h.browser.pages[cdp.Telegram.URL].closed {
		t.Error("the tab was not released")
	}
}

func vkServer(t *testing.T, tokenOK bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tokenOK {
			fmt.Fprint(w, `{"error":{"error_code":5,"error_msg":"User authorization failed"}}`)
			return
		}
		switch r.URL.Path {
		case "/groups.getById":
			switch r.URL.Query().Get("group_id") {
			case "closed":
				fmt.Fprint(w, `{"response":{"groups":[{"id":2,"screen_name":"closed"}]}}`)
			case "flood":
				fmt.Fprint(w, `{"response":{"groups":[{"id":3,"screen_name":"flood"}]}}`)
			case "durov":
				fmt.Fprint(w, `{"error":{"error_code":100,"error_msg":"One of the parameters specified was missing or invalid"}}`)
			default:
				fmt.Fprint(w, `{"response":{"groups":[{"id":1,"screen_name":"acme","contacts":[`+
					`{"user_id":5,"desc":"Главный врач","phone":"+7 900 000-00-00"},`+
					`{"user_id":6,"desc":"Администратор"},`+
					`{"desc":"Ресепшн","email":"info@example.com"}]}]}}`)
			}
		case "/groups.getMembers":
			switch r.URL.Query().Get("group_id") {
			case "2":
				fmt.Fprint(w, `{"error":{"error_code":15,"error_msg":"Access denied"}}`)
			case "3":
				fmt.Fprint(w, `{"error":{"error_code":9,"error_msg":"Flood control"}}`)
			default:
				fmt.Fprint(w, `{"response":{"count":1,"items":[{"id":5,"first_name":"Иван","last_name":"Иванов","screen_name":"ivanov"}]}}`)
			}
		case "/users.get":
			fmt.Fprint(w, `{"response":[{"id":5,"first_name":"Иван","last_name":"Иванов","screen_name":"ivanov"},{"id":6,"first_name":"Мария","last_name":"Сидорова"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestVK(t *testing.T) {
	h := newHarness(t)
	h.app.vkBaseURL = vkServer(t, true)
	h.env[envVKToken] = "token"
	input := writeFile(t, "in.jsonl", `{"vk":"vk.com/acme","people":["Иванов Иван Иванович","Петров Пётр"]}`+"\n"+`{"vk":"vk.com/closed","people":["Иванов Иван"]}`+"\n")
	if code := h.run(t, "vk-find", "--input", input); code != exitBlocked {
		t.Fatalf("exit %d, want %d; stderr:\n%s", code, exitBlocked, h.stderr)
	}
	got := lines(t, h.stdout.String())
	if len(got) != 3 {
		t.Fatalf("%d lines, want one per person:\n%s", len(got), h.stdout)
	}
	if got[0]["source"] != "vk.com/acme" || got[0]["status"] != "ok" || got[0]["found_profile_url"] != "https://vk.com/ivanov" {
		t.Errorf("match = %v", got[0])
	}
	if got[1]["status"] != "not_found" || got[2]["status"] != "access_denied" || got[2]["source"] != "vk.com/closed" {
		t.Errorf("rows = %v", got[1:])
	}
	if h.browser.connects != 0 {
		t.Error("VK connected to the browser")
	}
}

func TestVKNeedsAToken(t *testing.T) {
	h := newHarness(t)
	if code := h.run(t, "vk-find", "vk.com/acme", "--person", "Иванов Иван"); code != exitLogin {
		t.Fatalf("no token: exit %d, want %d", code, exitLogin)
	}
	h = newHarness(t)
	h.app.vkBaseURL = vkServer(t, false)
	h.env[envVKToken] = "expired"
	if code := h.run(t, "vk-find", "vk.com/acme", "--person", "Иванов Иван"); code != exitLogin {
		t.Fatalf("rejected token: exit %d, want %d", code, exitLogin)
	}
	if h.stdout.Len() != 0 {
		t.Errorf("a rejected token was recorded: %s", h.stdout)
	}
}

func TestVKTakesAServiceKey(t *testing.T) {
	h := newHarness(t)
	h.app.vkBaseURL = vkServer(t, true)
	h.env[envVKServiceToken] = "service"
	if code := h.run(t, "vk-find", "vk.com/acme", "--person", "Иванов Иван"); code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, h.stderr)
	}
	if strings.Contains(h.stdout.String()+h.stderr.String(), "service") {
		t.Errorf("the key was printed:\n%s\n%s", h.stdout, h.stderr)
	}

	h = newHarness(t)
	h.env[envVKServiceToken] = "service"
	if code := h.run(t, "status", "vk"); code != exitOK || !strings.Contains(h.stdout.String(), `"vk":{"token":true,"kind":"service"}`) {
		t.Errorf("status vk with a service key: exit %d, %s", code, h.stdout)
	}
	h = newHarness(t)
	h.env[envVKServiceToken] = "service"
	h.env[envVKToken] = "user"
	if code := h.run(t, "status", "vk"); code != exitOK || !strings.Contains(h.stdout.String(), `"kind":"user"`) {
		t.Errorf("status vk with both: exit %d, %s", code, h.stdout)
	}
}

func TestVKFloodControlStopsTheRunAndIsRetried(t *testing.T) {
	h := newHarness(t)
	h.app.vkBaseURL = vkServer(t, true)
	h.env[envVKServiceToken] = "service"
	out := filepath.Join(t.TempDir(), "out.jsonl")
	input := writeFile(t, "in.csv", "vk,people\nvk.com/flood,Иванов Иван\nvk.com/acme,Иванов Иван\n")
	if code := h.run(t, "vk-find", "--input", input, "--out", out); code != exitBlocked {
		t.Fatalf("exit %d, want %d; stderr:\n%s", code, exitBlocked, h.stderr)
	}
	data, _ := os.ReadFile(out)
	got := lines(t, string(data))
	if len(got) != 1 || got[0]["status"] != "rate_limited" || !strings.Contains(fmt.Sprint(got[0]["error"]), "Flood control") {
		t.Fatalf("lines = %v; the run should stop at flood control", got)
	}
	settled, err := loadDone(out, decodeVK, h.stderr)
	if err != nil || len(settled) != 0 {
		t.Errorf("a flood-controlled community counts as done: %v %v", settled, err)
	}
}

func TestVKContacts(t *testing.T) {
	h := newHarness(t)
	h.app.vkBaseURL = vkServer(t, true)
	h.env[envVKToken] = "token"
	input := writeFile(t, "in.csv", "inn,vk,people\n"+
		"7700000001,https://vk.com/acme,ИВАНОВ ИВАН ИВАНОВИЧ; ПЕТРОВ ПЁТР\n"+
		"7700000002,https://vk.com/durov,\n"+
		"7700000003,https://t.me/acme,\n")
	if code := h.run(t, "vk-contacts", "--input", input); code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, h.stderr)
	}
	var recs []record.VKContacts
	for _, line := range strings.Split(strings.TrimSpace(h.stdout.String()), "\n") {
		var rec record.VKContacts
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	if len(recs) != 3 {
		t.Fatalf("%d lines, want one per community:\n%s", len(recs), h.stdout)
	}
	acme := recs[0]
	if acme.Source != "https://vk.com/acme" || acme.Group != "acme" || acme.Status != "ok" || len(acme.Contacts) != 3 {
		t.Fatalf("acme = %+v", acme)
	}
	doctor, admin, desk := acme.Contacts[0], acme.Contacts[1], acme.Contacts[2]
	if doctor.Name != "Иван Иванов" || doctor.ProfileURL != "https://vk.com/ivanov" || doctor.Desc != "Главный врач" ||
		doctor.Phone == "" || doctor.MatchedPerson != "ИВАНОВ ИВАН ИВАНОВИЧ" || doctor.Confidence != "high" {
		t.Errorf("doctor = %+v", doctor)
	}
	if admin.Name != "Мария Сидорова" || admin.MatchedPerson != "" || admin.Confidence != "" {
		t.Errorf("admin = %+v", admin)
	}
	if desk.Name != "" || desk.Email != "info@example.com" {
		t.Errorf("desk = %+v", desk)
	}
	if recs[1].Status != "not_a_group" || recs[2].Status != "invalid_url" {
		t.Errorf("statuses = %s, %s", recs[1].Status, recs[2].Status)
	}
	if h.browser.connects != 0 {
		t.Error("vk-contacts connected to the browser")
	}

	h = newHarness(t)
	if code := h.run(t, "vk-contacts", "vk.com/acme"); code != exitLogin {
		t.Errorf("no token: exit %d", code)
	}
}

func TestStatus(t *testing.T) {
	h := newHarness(t)
	h.env[envVKToken] = "token"
	if code := h.run(t, "status"); code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, h.stderr)
	}
	var report statusReport
	if err := json.Unmarshal(h.stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Browser.Connected || !report.Telegram.LoggedIn || !report.Instagram.LoggedIn || !report.VK.Token {
		t.Errorf("report = %s", h.stdout)
	}
	if h.browser.connects != 1 {
		t.Errorf("connections = %d, want one", h.browser.connects)
	}

	h = newHarness(t)
	h.openErr = tg.ErrNotLoggedIn
	if code := h.run(t, "status", "telegram"); code != exitLogin {
		t.Fatalf("logged out: exit %d, want %d", code, exitLogin)
	}
	if !strings.Contains(h.stdout.String(), `"logged_in":false`) || strings.Contains(h.stdout.String(), "instagram") {
		t.Errorf("report = %s", h.stdout)
	}

	h = newHarness(t)
	h.browser.connErr = cdp.ErrNotEnabled
	if code := h.run(t, "status"); code != exitBrowser {
		t.Fatalf("no browser: exit %d, want %d", code, exitBrowser)
	}
	if !strings.Contains(h.stdout.String(), `"connected":false`) {
		t.Errorf("report = %s", h.stdout)
	}

	h = newHarness(t)
	if code := h.run(t, "status", "vk"); code != exitLogin || h.browser.connects != 0 {
		t.Fatalf("status vk without a token: exit %d, connects %d", code, h.browser.connects)
	}
}

func TestOpenBringsTheTabForward(t *testing.T) {
	h := newHarness(t)
	if code := h.run(t, "open", "instagram"); code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, h.stderr)
	}
	p := h.browser.pages[cdp.Instagram.URL]
	if p == nil || len(h.browser.activated) != 1 || h.browser.activated[0] != p.id || !p.closed {
		t.Fatalf("activated %v", h.browser.activated)
	}
	if !strings.Contains(h.stdout.String(), `"network":"instagram"`) {
		t.Errorf("output = %s", h.stdout)
	}
}
