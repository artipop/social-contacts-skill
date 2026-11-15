package vk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalizeGroupURL(t *testing.T) {
	cases := map[string]string{
		"https://vk.com/club123":            "club123",
		"vk.com/mycommunity":                "mycommunity",
		"https://m.vk.com/public42/":        "public42",
		"https://www.vk.com/@editorial":     "editorial",
		"https://vk.com/event7?w=wall-1_2":  "event7",
		"https://t.me/somechannel":          "",
		"":                                  "",
		"https://vkontakte.ru/durov":        "durov",
		"https://vk.com/":                   "",
		"https://instagram.com/vk.com/oops": "",
	}
	for input, want := range cases {
		if got := NormalizeGroupURL(input); got != want {
			t.Errorf("NormalizeGroupURL(%q) = %q, want %q", input, got, want)
		}
	}
}

// A shared first name must not confirm a person: every community holds an Иван,
// and naming the wrong one as the company's owner is worse than finding nobody.
func TestMatchPersonNeedsTwoTokens(t *testing.T) {
	cases := []struct {
		target, first, last, want string
	}{
		{"Иванов Иван Иванович", "Иван", "Иванов", "high"},
		{"Иван Петров", "Иван", "Петров", "high"},
		{"Иванов Иван Иванович", "Иван", "Сидоров", ""},
		{"Иванов Иван Иванович", "Пётр", "Иванов", ""},
		{"Пётр Королёв", "Петр", "Королев", "high"}, // ё is folded to е
		{"Иванов И. И.", "Иван", "Иванов", ""},      // initials are dropped
		{"", "Иван", "Иванов", ""},
	}
	for _, c := range cases {
		got, _ := MatchPerson(c.target, c.first, c.last)
		if got != c.want {
			t.Errorf("MatchPerson(%q, %q %q) = %q, want %q", c.target, c.first, c.last, got, c.want)
		}
	}
}

// fakeVK answers the two methods the collector calls.
func fakeVK(t *testing.T, members []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/utils.resolveScreenName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"response": map[string]any{"type": "group", "object_id": 777},
			})
		case "/groups.getMembers":
			offset := 0
			fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
			items := []map[string]any{}
			if offset < len(members) {
				items = members[offset:]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"response": map[string]any{"count": len(members), "items": items},
			})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
}

func TestSearchPeopleFindsAndMisses(t *testing.T) {
	server := fakeVK(t, []map[string]any{
		{"id": 1, "first_name": "Иван", "last_name": "Иванов", "screen_name": "ivanov"},
		{"id": 2, "first_name": "Мария", "last_name": "Сидорова"},
	})
	defer server.Close()

	rows := SearchPeople(context.Background(), Config{
		GroupURL: "https://vk.com/mycommunity", AccessToken: "t",
		People:  []string{"Иванов Иван Иванович", "Петров Пётр Петрович"},
		BaseURL: server.URL, RequestDelay: time.Millisecond,
	}, nil)

	if len(rows) != 2 {
		t.Fatalf("want a row per target, got %d", len(rows))
	}
	if rows[0].Status != "ok" || rows[0].Confidence != "high" {
		t.Errorf("Иванов should be found: %+v", rows[0])
	}
	if rows[0].FoundProfileURL != "https://vk.com/ivanov" {
		t.Errorf("profile url = %q", rows[0].FoundProfileURL)
	}
	if rows[0].ResolvedGroupID != "777" {
		t.Errorf("group id = %q, want 777", rows[0].ResolvedGroupID)
	}
	if rows[1].Status != "not_found" {
		t.Errorf("Петров should be absent: %+v", rows[1])
	}
	// "not found" is an answer, not a failure — the caller must not retry it.
	if rows[1].Blocked() {
		t.Error("not_found must not read as blocked")
	}
}

// A bad token and an empty community look the same to the caller unless the
// failure is reported distinctly.
func TestSearchPeopleReportsBlocked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"error_code": 5, "error_msg": "User authorization failed"},
		})
	}))
	defer server.Close()

	rows := SearchPeople(context.Background(), Config{
		GroupURL: "https://vk.com/club1", AccessToken: "stale",
		People:  []string{"Иванов Иван"},
		BaseURL: server.URL,
	}, nil)

	if len(rows) != 1 || rows[0].Status != "invalid_token" {
		t.Fatalf("want invalid_token, got %+v", rows)
	}
	if !rows[0].Blocked() {
		t.Error("invalid_token must read as blocked")
	}
}

func TestSearchPeopleWithoutTokenDoesNotCallVK(t *testing.T) {
	rows := SearchPeople(context.Background(), Config{
		GroupURL: "https://vk.com/club1", People: []string{"Иванов Иван"},
		BaseURL: "http://127.0.0.1:1", // would fail if it were reached
	}, nil)
	if len(rows) != 1 || rows[0].Status != "invalid_token" {
		t.Fatalf("want invalid_token without a call, got %+v", rows)
	}
}

// A Stop mid-walk keeps whoever was already found rather than discarding the
// page it had.
func TestFetchMembersStopsOnCancel(t *testing.T) {
	server := fakeVK(t, []map[string]any{{"id": 1, "first_name": "Иван", "last_name": "Иванов"}})
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	members, err := FetchMembers(ctx, Config{AccessToken: "t", BaseURL: server.URL}, 777, nil)
	if err != nil {
		t.Fatalf("cancel is not an error: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("cancelled before the first page, got %d", len(members))
	}
}
