package vk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Contact is one entry of a community's «Контакты» block: a VK user the
// community names, or just a phone or email, with the role it gives them.
type Contact struct {
	Name       string `json:"name"`
	ProfileURL string `json:"profile_url"`
	Desc       string `json:"desc"`
	Phone      string `json:"phone"`
	Email      string `json:"email"`
	// MatchedPerson is the input person whose surname and first name both
	// match the contact's name; Confidence is then "high".
	MatchedPerson string `json:"matched_person"`
	Confidence    string `json:"confidence"`
}

// ContactsResult is one community's contacts.
type ContactsResult struct {
	Group    string    `json:"group"`
	GroupID  int       `json:"group_id,omitempty"`
	Contacts []Contact `json:"contacts"`
	// Status is "ok" (with or without contacts), or why the contacts could not
	// be read: invalid_url, not_a_group, access_denied, invalid_token,
	// rate_limited, network_error.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Blocked reports whether the community was never read.
func (r ContactsResult) Blocked() bool { return BlockStatuses[r.Status] }

// GroupContacts reads the public contacts of the community a link points at,
// names the VK users among them, and marks those who are one of people.
func GroupContacts(ctx context.Context, cfg Config, link string, people []string) ContactsResult {
	cfg.AccessToken = strings.TrimSpace(cfg.AccessToken)
	path := NormalizeGroupURL(link)
	res := ContactsResult{Group: path, Contacts: []Contact{}}
	fail := func(status, message string) ContactsResult {
		res.Status, res.Error = status, message
		return res
	}
	if cfg.AccessToken == "" {
		return fail("invalid_token", "нет токена VK")
	}
	if path == "" {
		return fail("invalid_url", fmt.Sprintf("Не похоже на ссылку на сообщество VK: %s", link))
	}
	group, err := GetGroup(ctx, cfg, path, "contacts")
	if err != nil {
		return fail(statusOf(err), err.Error())
	}
	res.GroupID = group.ID
	if group.ScreenName != "" {
		res.Group = group.ScreenName
	}

	var ids []string
	for _, c := range group.Contacts {
		if c.UserID > 0 {
			ids = append(ids, strconv.Itoa(c.UserID))
		}
	}
	users := map[int]Member{}
	if len(ids) > 0 {
		found, err := getUsers(ctx, cfg, ids)
		if err != nil {
			return fail(statusOf(err), err.Error())
		}
		for _, u := range found {
			id, _ := strconv.Atoi(u.ID)
			users[id] = u
		}
	}

	for _, c := range group.Contacts {
		contact := Contact{Desc: c.Desc, Phone: c.Phone, Email: c.Email}
		if u, ok := users[c.UserID]; ok {
			contact.Name = strings.TrimSpace(u.FirstName + " " + u.LastName)
			contact.ProfileURL = u.ProfileURL()
			contact.MatchedPerson, contact.Confidence = MatchContact(people, u.FirstName, u.LastName)
		} else if c.UserID > 0 {
			contact.ProfileURL = "https://vk.com/id" + strconv.Itoa(c.UserID)
		}
		res.Contacts = append(res.Contacts, contact)
	}
	res.Status = "ok"
	return res
}

// MatchContact names the person among people whose surname and first name
// both match a contact's, and the match's confidence; empty when none does.
func MatchContact(people []string, firstName, lastName string) (string, string) {
	for _, person := range people {
		if confidence, _ := MatchPerson(person, firstName, lastName); confidence != "" {
			return strings.TrimSpace(person), confidence
		}
	}
	return "", ""
}

// getUsers names VK users by id.
func getUsers(ctx context.Context, cfg Config, ids []string) ([]Member, error) {
	raw, err := call(ctx, cfg, "users.get", url.Values{
		"user_ids": {strings.Join(ids, ",")},
		"fields":   {"screen_name,domain"},
	})
	if err != nil {
		return nil, err
	}
	var items []struct {
		ID         int    `json:"id"`
		FirstName  string `json:"first_name"`
		LastName   string `json:"last_name"`
		ScreenName string `json:"screen_name"`
		Domain     string `json:"domain"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, &Error{Status: "network_error", Message: fmt.Sprintf("непонятный ответ VK: %v", err)}
	}
	out := make([]Member, 0, len(items))
	for _, it := range items {
		out = append(out, Member{
			ID: strconv.Itoa(it.ID), FirstName: it.FirstName, LastName: it.LastName,
			ScreenName: it.ScreenName, Domain: it.Domain,
		})
	}
	return out, nil
}
