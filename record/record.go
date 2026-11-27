// Package record is the JSON Lines format of collection results: one
// collector result per line, tagged with the input it answers.
//
// A line is the collector's own result type with a "source" field added, so
// whoever reads the file sees exactly what the collector returned:
//
//	{"source":"https://t.me/somegroup","channel":"https://t.me/somegroup","members":[…],"total":152,"status":"ok"}
//	{"source":"instagram.com/acme","profile":"instagram.com/acme","matches":[…],"status":"ok"}
//	{"source":"vk.com/acme","source_vk_url":"vk.com/acme","target_person":"Иванов Иван","status":"not_found",…}
//	{"source":"7700000000","target_person":"Иванов Иван","query":{"name":"Иван Иванов","company":"Ромашка"},"candidates":[…],"status":"ok"}
//
// Telegram and Instagram write one line per input; VK writes one line per
// person searched for in the community, and the VK people search one line per
// person of each input row.
package record

import (
	"strings"

	"github.com/artipop/social-contacts-skill/ig"
	"github.com/artipop/social-contacts-skill/tg"
	"github.com/artipop/social-contacts-skill/vk"
	"github.com/artipop/social-contacts-skill/vkweb"
)

// Outcome is what a result means, whatever collector produced it.
type Outcome string

const (
	// Found: the collector got in and found someone.
	Found Outcome = "found"
	// NotFound: the collector got in and found nobody. An answer, unlike
	// Blocked.
	NotFound Outcome = "not_found"
	// Blocked: the collector never got to look — a captcha, a rate limit, a
	// network failure, a closed community.
	Blocked Outcome = "blocked"
	// NeedsLogin: the account the collector runs as is logged out, or its
	// token is missing or expired.
	NeedsLogin Outcome = "needs_login"
	// Invalid: the input cannot be collected — not a channel, profile or
	// community link, or one that does not exist.
	Invalid Outcome = "invalid"
	// Failed: anything else that went wrong.
	Failed Outcome = "failed"
)

// Telegram is one channel's members.
type Telegram struct {
	Source string `json:"source"`
	tg.Result
}

// Outcome classifies the result.
func (r Telegram) Outcome() Outcome { o, _ := telegramOutcome(r.Status, r.Error); return o }

// Final reports that collecting the same input again would give the same
// answer: it has members, has none, or is closed for good.
func (r Telegram) Final() bool { _, final := telegramOutcome(r.Status, r.Error); return final }

// Instagram is the people found among one profile's followers.
type Instagram struct {
	Source string `json:"source"`
	ig.Result
}

// Outcome classifies the result.
func (r Instagram) Outcome() Outcome {
	o, _ := instagramOutcome(r.Status, len(r.Matches))
	return o
}

// Final reports that searching the same input again would give the same
// answer.
func (r Instagram) Final() bool { _, final := instagramOutcome(r.Status, len(r.Matches)); return final }

// VK is one person's outcome in one community.
type VK struct {
	Source string `json:"source"`
	vk.Row
}

// Outcome classifies the row.
func (r VK) Outcome() Outcome { o, _ := vkOutcome(r.Status); return o }

// Final reports that searching the same input again would give the same
// answer.
func (r VK) Final() bool { _, final := vkOutcome(r.Status); return final }

// VKSearch is one person looked up in VK's people search.
type VKSearch struct {
	Source string `json:"source"`
	vkweb.Result
}

// Outcome classifies the result.
func (r VKSearch) Outcome() Outcome { o, _ := vkSearchOutcome(r.Status); return o }

// Final reports that searching for the same person again would give the same
// answer.
func (r VKSearch) Final() bool { _, final := vkSearchOutcome(r.Status); return final }

// Telegram errors that mean the members are closed to this account for good.
var telegramClosed = []string{"CHANNEL_PRIVATE", "CHAT_ADMIN_REQUIRED"}

// Telegram errors that mean the username leads nowhere.
var telegramMissing = []string{"USERNAME_NOT_OCCUPIED", "USERNAME_INVALID"}

func telegramOutcome(status, errText string) (Outcome, bool) {
	switch status {
	case "ok":
		return Found, true
	case "unsupported_url", "is_user":
		return Invalid, true
	case "no_linked_discussion":
		return Blocked, true
	case "members_not_found", "resolve_failed":
		switch {
		case strings.Contains(errText, "FLOOD_WAIT"):
			return Blocked, false
		case containsAny(errText, telegramClosed):
			return Blocked, true
		case status == "resolve_failed" && (errText == "not_found" || containsAny(errText, telegramMissing)):
			return Invalid, true
		case status == "members_not_found" && errText == "":
			return NotFound, true
		}
	}
	return Failed, false
}

func instagramOutcome(status string, matches int) (Outcome, bool) {
	switch {
	case status == "ok" && matches > 0:
		return Found, true
	case status == "ok":
		return NotFound, true
	case status == "unsupported_url":
		return Invalid, true
	case status == "login_required":
		return NeedsLogin, false
	case (ig.Result{Status: status}).Blocked():
		return Blocked, false
	}
	return Failed, false
}

func vkOutcome(status string) (Outcome, bool) {
	switch status {
	case "ok":
		return Found, true
	case "not_found":
		return NotFound, true
	case "invalid_url", "not_a_group":
		return Invalid, true
	case "access_denied":
		return Blocked, true
	case "invalid_token":
		return NeedsLogin, false
	case "rate_limited", "network_error":
		return Blocked, false
	}
	return Failed, false
}

func vkSearchOutcome(status string) (Outcome, bool) {
	switch status {
	case "ok":
		return Found, true
	case "not_found":
		return NotFound, true
	case "invalid_name":
		return Invalid, true
	case "not_logged_in":
		return NeedsLogin, false
	case "captcha_or_challenge", "rate_limited", "network_error", "nav_error":
		return Blocked, false
	}
	return Failed, false
}

func containsAny(text string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}
