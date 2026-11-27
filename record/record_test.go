package record

import (
	"encoding/json"
	"testing"

	"github.com/artipop/social-contacts-skill/ig"
	"github.com/artipop/social-contacts-skill/tg"
	"github.com/artipop/social-contacts-skill/vk"
	"github.com/artipop/social-contacts-skill/vkweb"
)

func TestALineIsTheResultPlusItsSource(t *testing.T) {
	cases := []struct {
		name string
		rec  any
		want string
	}{
		{"telegram", Telegram{Source: "@somegroup", Result: tg.Result{
			Channel: "@somegroup", Total: 10, Status: "ok",
			Members: []tg.Member{{UserID: "7", Name: "Анна", Username: "anna_example"}},
		}}, `{"source":"@somegroup","channel":"@somegroup","members":[{"user_id":"7","name":"Анна","username":"anna_example","phone":"","is_bot":false,"is_premium":false,"is_admin":false,"role":"","bio":""}],"total":10,"status":"ok"}`},
		{"telegram blocked", Telegram{Source: "t.me/x", Result: tg.Result{
			Channel: "t.me/x", Status: "no_linked_discussion", Error: "closed",
		}}, `{"source":"t.me/x","channel":"t.me/x","total":0,"status":"no_linked_discussion","error":"closed"}`},
		{"instagram", Instagram{Source: "instagram.com/acme", Result: ig.Result{
			Profile: "instagram.com/acme", Status: "ok",
			Matches: []ig.Match{{Person: "Иванов Иван", Username: "ivan", Confidence: "high"}},
		}}, `{"source":"instagram.com/acme","profile":"instagram.com/acme","matches":[{"person":"Иванов Иван","query":"","username":"ivan","full_name":"","profile_url":"","matched_text":"","score":0,"confidence":"high"}],"status":"ok"}`},
		{"vk", VK{Source: "vk.com/acme", Row: vk.Row{
			SourceVKURL: "vk.com/acme", TargetPerson: "Иванов Иван", Status: "not_found",
		}}, `{"source":"vk.com/acme","source_vk_url":"vk.com/acme","resolved_group_id":"","target_person":"Иванов Иван","found_user_id":"","found_first_name":"","found_last_name":"","found_profile_url":"","status":"not_found","confidence":"","error":""}`},
		{"vk contacts", VKContacts{Source: "vk.com/acme", ContactsResult: vk.ContactsResult{
			Group: "acme", GroupID: 1, Status: "ok",
			Contacts: []vk.Contact{{Name: "Иван Иванов", ProfileURL: "https://vk.com/id5", Desc: "Директор", MatchedPerson: "Иванов Иван", Confidence: "high"}},
		}}, `{"source":"vk.com/acme","group":"acme","group_id":1,"contacts":[{"name":"Иван Иванов","profile_url":"https://vk.com/id5","desc":"Директор","phone":"","email":"","matched_person":"Иванов Иван","confidence":"high"}],"status":"ok"}`},
		{"vk search", VKSearch{Source: "7700000001", INN: "7700000001", Company: "Ромашка", Result: vkweb.Result{
			TargetPerson: "Иванов Иван", Query: vkweb.Query{Name: "Иван Иванов", City: "Москва"}, Status: "not_found",
		}}, `{"source":"7700000001","inn":"7700000001","company":"Ромашка","target_person":"Иванов Иван","query":{"name":"Иван Иванов","city":"Москва"},"status":"not_found"}`},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.rec)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

func TestOutcomes(t *testing.T) {
	telegram := []struct {
		status, err string
		want        Outcome
		final       bool
	}{
		{"ok", "", Found, true},
		{"members_not_found", "", NotFound, true},
		{"members_not_found", "participants_failed: FLOOD_WAIT_900", Blocked, false},
		{"members_not_found", "participants_failed: CHAT_ADMIN_REQUIRED", Blocked, true},
		{"members_not_found", "вкладка закрыта", Failed, false},
		{"no_linked_discussion", "", Blocked, true},
		{"unsupported_url", "", Invalid, true},
		{"is_user", "", Invalid, true},
		{"resolve_failed", "not_found", Invalid, true},
		{"resolve_failed", "resolve_failed: USERNAME_NOT_OCCUPIED", Invalid, true},
		{"resolve_failed", "resolve_failed: CHANNEL_PRIVATE", Blocked, true},
		{"resolve_failed", "resolve_failed: FLOOD_WAIT_30", Blocked, false},
		{"resolve_failed", "managers_not_ready", Failed, false},
	}
	for _, c := range telegram {
		r := Telegram{Result: tg.Result{Status: c.status, Error: c.err}}
		if r.Outcome() != c.want || r.Final() != c.final {
			t.Errorf("telegram %s %q = %s/%v, want %s/%v", c.status, c.err, r.Outcome(), r.Final(), c.want, c.final)
		}
	}

	instagram := []struct {
		status  string
		matches int
		want    Outcome
		final   bool
	}{
		{"ok", 1, Found, true},
		{"ok", 0, NotFound, true},
		{"unsupported_url", 0, Invalid, true},
		{"login_required", 0, NeedsLogin, false},
		{"captcha_or_challenge", 0, Blocked, false},
		{"rate_limited", 0, Blocked, false},
		{"network_error", 0, Blocked, false},
		{"cancelled", 0, Failed, false},
	}
	for _, c := range instagram {
		r := Instagram{Result: ig.Result{Status: c.status, Matches: make([]ig.Match, c.matches)}}
		if r.Outcome() != c.want || r.Final() != c.final {
			t.Errorf("instagram %s = %s/%v, want %s/%v", c.status, r.Outcome(), r.Final(), c.want, c.final)
		}
	}

	vkCases := []struct {
		status string
		want   Outcome
		final  bool
	}{
		{"ok", Found, true},
		{"not_found", NotFound, true},
		{"invalid_url", Invalid, true},
		{"not_a_group", Invalid, true},
		{"access_denied", Blocked, true},
		{"rate_limited", Blocked, false},
		{"network_error", Blocked, false},
		{"invalid_token", NeedsLogin, false},
		{"vk_error", Failed, false},
	}
	for _, c := range vkCases {
		r := VK{Row: vk.Row{Status: c.status}}
		if r.Outcome() != c.want || r.Final() != c.final {
			t.Errorf("vk %s = %s/%v, want %s/%v", c.status, r.Outcome(), r.Final(), c.want, c.final)
		}
	}
}

func TestVKSearchOutcomes(t *testing.T) {
	cases := []struct {
		status string
		want   Outcome
		final  bool
	}{
		{"ok", Found, true},
		{"not_found", NotFound, true},
		{"invalid_name", Invalid, true},
		{"not_logged_in", NeedsLogin, false},
		{"captcha_or_challenge", Blocked, false},
		{"rate_limited", Blocked, false},
		{"network_error", Blocked, false},
		{"nav_error", Blocked, false},
		{"cancelled", Failed, false},
	}
	for _, c := range cases {
		r := VKSearch{Result: vkweb.Result{Status: c.status}}
		if r.Outcome() != c.want || r.Final() != c.final {
			t.Errorf("%s: %s/%v, want %s/%v", c.status, r.Outcome(), r.Final(), c.want, c.final)
		}
	}
}

func TestVKContactsOutcomes(t *testing.T) {
	cases := []struct {
		status   string
		contacts int
		want     Outcome
		final    bool
	}{
		{"ok", 2, Found, true},
		{"ok", 0, NotFound, true},
		{"invalid_url", 0, Invalid, true},
		{"not_a_group", 0, Invalid, true},
		{"access_denied", 0, Blocked, true},
		{"rate_limited", 0, Blocked, false},
		{"network_error", 0, Blocked, false},
		{"invalid_token", 0, NeedsLogin, false},
	}
	for _, c := range cases {
		r := VKContacts{ContactsResult: vk.ContactsResult{Status: c.status, Contacts: make([]vk.Contact, c.contacts)}}
		if r.Outcome() != c.want || r.Final() != c.final {
			t.Errorf("%s with %d contacts: %s/%v, want %s/%v", c.status, c.contacts, r.Outcome(), r.Final(), c.want, c.final)
		}
	}
}
