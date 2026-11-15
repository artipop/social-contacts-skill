package tg

import "testing"

func TestUsernameFromURL(t *testing.T) {
	// The links come out of scraped websites, so every shape a company puts in
	// its footer has to survive the trip.
	cases := []struct{ in, want string }{
		{"https://t.me/durov", "durov"},
		{"http://t.me/durov", "durov"},
		{"t.me/durov", "durov"},
		{"https://www.t.me/durov/", "durov"},
		{"https://telegram.me/durov", "durov"},
		{"@durov", "durov"},
		// A link to one message in a channel still names the channel.
		{"https://t.me/durov/123", "durov"},
		// t.me/s/name is the web preview of the same channel.
		{"https://t.me/s/durov", "durov"},
		// The web client keeps the name in the fragment.
		{"https://web.telegram.org/k/#@durov", "durov"},

		// Invite links carry no resolvable name: there is nothing to ask the
		// API for, so they must be refused rather than half-parsed.
		{"https://t.me/+AbCdEfGhIjK", ""},
		{"https://t.me/joinchat/AbCdEfGhIjK", ""},
		// Too short to be a username, and not Telegram at all.
		{"https://t.me/ab", ""},
		{"https://example.com/durov", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := usernameFromURL(c.in); got != c.want {
			t.Errorf("usernameFromURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBotsAreNotWorthOpening(t *testing.T) {
	// A bot has no member list, and asking for one wastes a channel's timeout.
	for _, url := range []string{"https://t.me/some_bot", "https://t.me/SomeBot_bot", "https://t.me/x/bot"} {
		if Collectable(url) {
			t.Errorf("bot %q accepted for collection", url)
		}
	}
	for _, url := range []string{"https://t.me/durov", "https://t.me/robotics"} {
		if !Collectable(url) {
			t.Errorf("channel %q rejected as a bot", url)
		}
	}
}

func TestFloodWaitPrefersTheStructuredHint(t *testing.T) {
	// Telegram tells us exactly how long to wait; honouring that number beats
	// any backoff we could invent.
	if got := (bridgeResult{FloodWait: 42}).floodWait().Seconds(); got != 42 {
		t.Errorf("structured flood_wait = %v, want 42", got)
	}
	// Older responses only carry it in the error text.
	if got := (bridgeResult{Error: "participants_failed: FLOOD_WAIT_17"}).floodWait().Seconds(); got != 17 {
		t.Errorf("flood_wait from the text = %v, want 17", got)
	}
	if got := (bridgeResult{Error: "participants_failed: CHAT_ADMIN_REQUIRED"}).floodWait(); got != 0 {
		t.Errorf("an ordinary error taken for flood_wait: %v", got)
	}
}

func TestMemberPublicURL(t *testing.T) {
	if got := (Member{Username: "durov"}).PublicURL(); got != "https://t.me/durov" {
		t.Errorf("member link = %q", got)
	}
	// An account with no public username cannot be reached, and inventing a
	// link to it would put a dead URL in the results.
	if got := (Member{UserID: "123"}).PublicURL(); got != "" {
		t.Errorf("invented link %q for a member without @username", got)
	}
}
