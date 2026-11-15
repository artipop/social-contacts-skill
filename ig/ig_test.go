package ig

import "testing"

func TestNormalizeProfileURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://www.instagram.com/nasa/", "https://www.instagram.com/nasa/"},
		{"instagram.com/nasa", "https://www.instagram.com/nasa/"},
		{"https://instagram.com/nasa?hl=ru", "https://www.instagram.com/nasa/"},
		{"https://instagr.am/nasa", "https://www.instagram.com/nasa/"},
		{"https://www.instagram.com/@nasa", "https://www.instagram.com/nasa/"},

		// A post or a reel is not a profile: there are no followers to search
		// there, and treating one as a profile would search the wrong account.
		{"https://www.instagram.com/p/Cabc123/", ""},
		{"https://www.instagram.com/nasa/reel/Cabc123/", "https://www.instagram.com/nasa/"},
		// Instagram's own pages.
		{"https://www.instagram.com/explore/", ""},
		{"https://www.instagram.com/accounts/login/", ""},
		// Not Instagram at all.
		{"https://vk.com/nasa", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeProfileURL(c.in); got != c.want {
			t.Errorf("NormalizeProfileURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBlockedTellsApartWhatLooksTheSame(t *testing.T) {
	// "Found nobody" and "was never allowed to look" look identical in the
	// results unless they are distinguished here.
	for _, status := range []string{"captcha_or_challenge", "rate_limited", "login_required", "network_error"} {
		if !(Result{Status: status}).Blocked() {
			t.Errorf("status %q is not treated as blocked", status)
		}
	}
	if (Result{Status: "ok"}).Blocked() {
		t.Error("a successful search counted as blocked")
	}
}

func TestNavErrorsNameTheLikelyCause(t *testing.T) {
	// A tunnel or DNS failure is almost always the user's VPN or region, and
	// saying so saves them retrying into the same wall.
	status, detail := classifyNavError("net::ERR_TUNNEL_CONNECTION_FAILED at https://instagram.com")
	if status != "network_error" {
		t.Fatalf("network error = %q", status)
	}
	if detail == "" {
		t.Error("network error without an explanation")
	}
	if status, _ := classifyNavError("something else went wrong"); status != "nav_error" {
		t.Errorf("ordinary navigation error = %q", status)
	}
}

func TestBetterEvidenceForTheSamePersonWins(t *testing.T) {
	// The same profile can be found by several queries; the finding kept should
	// be the one the user can trust most.
	strong := Match{Confidence: "high", Score: 80}
	weak := Match{Confidence: "low", Score: 100}
	if rank(strong) <= rank(weak) {
		t.Error("confidence lost to a bare match score")
	}
	better := Match{Confidence: "high", Score: 90}
	if rank(better) <= rank(strong) {
		t.Error("with equal confidence the better match did not win")
	}
}

func TestFollowersCountIsNotTheFollowingCount(t *testing.T) {
	page := func(texts ...string) []candidate {
		out := make([]candidate, len(texts))
		for i, text := range texts {
			out[i] = candidate{Text: text, Href: "#"}
		}
		return out
	}
	cases := []struct {
		name       string
		candidates []candidate
		want       int
	}{
		{"russian thousands", page("Подписаться", "11 подписок", "113 тыс. подписчиков"), 2},
		{"russian millions", page("1,2 млн подписчиков", "300 подписок"), 0},
		{"russian plain", page("Сообщения", "523 подписчика"), 1},
		{"english", page("11 following", "12.5K followers"), 1},
		{"english one", page("1 follower", "2 following"), 0},
		{"only following", page("11 подписок", "Подписаться"), -1},
		{"link wins", []candidate{{Text: "11 подписок", Href: "#"}, {Text: "x", Href: "/name/followers/"}}, 1},
	}
	for _, c := range cases {
		if got := followersCount(c.candidates); got != c.want {
			t.Errorf("%s: followersCount = %d, want %d", c.name, got, c.want)
		}
	}
}
