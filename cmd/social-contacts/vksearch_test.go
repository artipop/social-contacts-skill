package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/artipop/social-contacts-skill/cdp"
	"github.com/artipop/social-contacts-skill/record"
	"github.com/artipop/social-contacts-skill/vkweb"
)

// fakeVKWeb answers Search from a table, by person.
type fakeVKWeb struct {
	results map[string]vkweb.Result
	calls   []string
}

func (s *fakeVKWeb) Search(_ context.Context, person, city, company string) vkweb.Result {
	s.calls = append(s.calls, person+"|"+city+"|"+company)
	if res, ok := s.results[person]; ok {
		return res
	}
	return vkweb.Result{TargetPerson: person, Query: vkweb.Query{Name: person}, Status: "not_found"}
}

func (s *fakeVKWeb) Close() {}

func vkFound(person string) vkweb.Result {
	return vkweb.Result{TargetPerson: person, Status: "ok", Candidates: []vkweb.Candidate{
		{ProfileURL: "https://vk.com/id1", Name: person, Confidence: "medium", Score: 80},
	}}
}

func TestVKSearchWritesALinePerPersonOfEachRow(t *testing.T) {
	h := newHarness(t)
	h.vkweb.results["ЗАРЕЧНЫЙ ПЁТР ИЛЬИЧ"] = vkFound("ЗАРЕЧНЫЙ ПЁТР ИЛЬИЧ")
	input := writeFile(t, "people.csv", "inn,company,city,people,vk\n"+
		"7700000001,Ромашка,Москва,ЗАРЕЧНЫЙ ПЁТР ИЛЬИЧ; ЛУГОВАЯ ОЛЬГА ПЕТРОВНА,https://vk.com/romashka\n"+
		"7700000002,Лютик,,ЗАРЕЧНЫЙ ПЁТР ИЛЬИЧ,\n")
	if code := h.run(t, "vk-search", "--input", input, "--surname-first"); code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, h.stderr)
	}
	got := lines(t, h.stdout.String())
	if len(got) != 3 {
		t.Fatalf("%d lines:\n%s", len(got), h.stdout)
	}
	if got[0]["source"] != "7700000001" || got[0]["target_person"] != "ЗАРЕЧНЫЙ ПЁТР ИЛЬИЧ" || got[0]["status"] != "ok" {
		t.Errorf("first line = %v", got[0])
	}
	if got[1]["target_person"] != "ЛУГОВАЯ ОЛЬГА ПЕТРОВНА" || got[1]["status"] != "not_found" {
		t.Errorf("second line = %v", got[1])
	}
	if got[2]["source"] != "7700000002" {
		t.Errorf("third line = %v", got[2])
	}
	want := []string{"ЗАРЕЧНЫЙ ПЁТР ИЛЬИЧ|Москва|Ромашка", "ЛУГОВАЯ ОЛЬГА ПЕТРОВНА|Москва|Ромашка", "ЗАРЕЧНЫЙ ПЁТР ИЛЬИЧ||Лютик"}
	if strings.Join(h.vkweb.calls, ",") != strings.Join(want, ",") {
		t.Errorf("searched %v", h.vkweb.calls)
	}
	if p := h.browser.pages[cdp.VK.URL]; p == nil || len(h.browser.activated) != 1 {
		t.Errorf("the VK tab was not brought forward: %v", h.browser.activated)
	}
}

func TestVKSearchFlagsApplyToEveryRow(t *testing.T) {
	h := newHarness(t)
	input := writeFile(t, "people.jsonl", `{"names":["Пётр Заречный"],"город":"Казань"}`+"\n")
	if code := h.run(t, "vk-search", "--input", input, "--company", "Ромашка", "--city", "Москва"); code != exitNotFound {
		t.Fatalf("exit %d; stderr:\n%s", code, h.stderr)
	}
	if len(h.vkweb.calls) != 1 || h.vkweb.calls[0] != "Пётр Заречный|Москва|Ромашка" {
		t.Errorf("searched %v", h.vkweb.calls)
	}
	if got := lines(t, h.stdout.String()); got[0]["source"] != "row 1" {
		t.Errorf("a row without a key column = %v", got[0])
	}
}

func TestVKSearchResumesPerPerson(t *testing.T) {
	h := newHarness(t)
	out := filepath.Join(t.TempDir(), "out.jsonl")
	previous := []record.VKSearch{
		{Source: "1", Result: vkFound("Пётр Заречный")},
		{Source: "1", Result: vkweb.Result{TargetPerson: "Ольга Луговая", Status: "captcha_or_challenge"}},
	}
	var data []byte
	for _, rec := range previous {
		line, _ := json.Marshal(rec)
		data = append(append(data, line...), '\n')
	}
	if err := os.WriteFile(out, data, 0o600); err != nil {
		t.Fatal(err)
	}
	input := writeFile(t, "people.csv", "id,people\n1,Пётр Заречный; Ольга Луговая\n")
	if code := h.run(t, "vk-search", "--input", input, "--out", out); code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, h.stderr)
	}
	if len(h.vkweb.calls) != 1 || !strings.HasPrefix(h.vkweb.calls[0], "Ольга Луговая|") {
		t.Errorf("searched %v; only the blocked person should be retried", h.vkweb.calls)
	}
}

func TestVKSearchStopsAtACaptcha(t *testing.T) {
	h := newHarness(t)
	h.vkweb.results["Пётр Заречный"] = vkweb.Result{TargetPerson: "Пётр Заречный", Status: "captcha_or_challenge", Error: "капча"}
	input := writeFile(t, "people.csv", "id,people\n1,Пётр Заречный; Ольга Луговая\n")
	if code := h.run(t, "vk-search", "--input", input); code != exitBlocked {
		t.Fatalf("exit %d, want %d", code, exitBlocked)
	}
	if len(h.vkweb.calls) != 1 {
		t.Errorf("searched on after a captcha: %v", h.vkweb.calls)
	}
}

func TestVKSearchLoggedOut(t *testing.T) {
	h := newHarness(t)
	h.vkweb.results["Пётр Заречный"] = vkweb.Result{TargetPerson: "Пётр Заречный", Status: "not_logged_in"}
	if code := h.run(t, "vk-search", "--person", "Пётр Заречный"); code != exitLogin {
		t.Fatalf("logged out mid-run: exit %d, want %d", code, exitLogin)
	}
	if h.stdout.Len() != 0 {
		t.Errorf("a login wall was recorded: %s", h.stdout)
	}

	h = newHarness(t)
	h.openErr = vkweb.ErrNotLoggedIn
	if code := h.run(t, "vk-search", "--person", "Пётр Заречный"); code != exitLogin {
		t.Fatalf("logged out: exit %d, want %d", code, exitLogin)
	}
	if code := h.run(t, "status", "vk-web"); code != exitLogin || !strings.Contains(h.stdout.String(), `"vk_web":{"logged_in":false`) {
		t.Fatalf("status vk-web: exit %d, %s", code, h.stdout)
	}
}

func TestVKSearchUsage(t *testing.T) {
	h := newHarness(t)
	if code := h.run(t, "vk-search", "https://vk.com/acme"); code != exitUsage {
		t.Errorf("a link: exit %d", code)
	}
	input := writeFile(t, "people.csv", "id,company\n1,Ромашка\n")
	if code := h.run(t, "vk-search", "--input", input); code != exitUsage {
		t.Errorf("no names: exit %d", code)
	}
	if code := h.run(t, "vk-search", "--input", input, "--city-column", "town"); code != exitUsage {
		t.Errorf("missing city column: exit %d", code)
	}
}
