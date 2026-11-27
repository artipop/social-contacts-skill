// Package vk searches a VK community's subscribers for named people, and
// reads a community's public contacts.
//
// The odd one out among the collectors: Telegram and Instagram have no public
// member list, so those are read out of a logged-in browser. VK documents
// `groups.getMembers` and answers it for any open community given a token, so
// this is an HTTP client and nothing more — no window, no login gate, no DOM.
// A user token and an app's service key both do: groups.getById,
// groups.getMembers and users.get are open to service keys.
//
// Matching is easier too. VK returns `first_name`/`last_name` as structured
// Cyrillic, so a registry name is compared directly and the names package —
// which exists to guess how «Иванов» might be spelled in latin — is not needed
// here.
package vk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	APIVersion = "5.199"
	apiBase    = "https://api.vk.com/method"
	// pageSize is VK's own maximum for groups.getMembers.
	pageSize     = 1000
	memberFields = "first_name,last_name,screen_name,city,domain"
)

// BlockStatuses are the outcomes that mean we never saw a member list at all —
// a bad token, a closed community, VK throttling us. Kept apart from "no match"
// because the two look identical in the results and mean opposite things: one is
// worth retrying, the other is an answer.
var BlockStatuses = map[string]bool{
	"invalid_token": true,
	"access_denied": true,
	"rate_limited":  true,
	"network_error": true,
	"not_a_group":   true,
}

// statusByCode maps VK's error codes onto those outcomes.
var statusByCode = map[int]string{
	5:   "invalid_token", // user authorization failed
	15:  "access_denied", // closed or private community
	18:  "access_denied", // profile deleted or banned
	203: "access_denied", // access to the community denied
	6:   "rate_limited",  // too many requests per second
	9:   "rate_limited",  // flood control: the key's daily quota for the method
	29:  "rate_limited",  // rate limit reached
}

// retryCodes are the short rate limits worth waiting out. Flood control (9)
// is not one: it lasts until the next day.
var retryCodes = map[int]bool{6: true, 29: true}

// Error is a VK API failure carrying the outcome it maps to.
type Error struct {
	Status  string
	Message string
	Code    int
}

func (e *Error) Error() string { return e.Message }

// Config is one search: a community, and the people to look for in it.
type Config struct {
	GroupURL    string
	People      []string
	AccessToken string
	APIVersion  string
	// RequestDelay paces the paging loop under VK's per-token rate limit.
	RequestDelay time.Duration
	// RetryDelay is the first wait after a short rate limit (errors 6 and 29);
	// each further retry waits longer.
	RetryDelay time.Duration
	Timeout    time.Duration
	HTTPClient *http.Client
	// BaseURL overrides the API host; tests point it at an httptest server.
	BaseURL string
}

func (c Config) apiVersion() string {
	if c.APIVersion != "" {
		return c.APIVersion
	}
	return APIVersion
}

func (c Config) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return apiBase
}

func (c Config) delay() time.Duration {
	if c.RequestDelay > 0 {
		return c.RequestDelay
	}
	return 350 * time.Millisecond // ~3 req/s
}

func (c Config) retryDelay() time.Duration {
	if c.RetryDelay > 0 {
		return c.RetryDelay
	}
	return time.Second
}

func (c Config) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// Member is a subscriber as we need them.
type Member struct {
	ID         string
	FirstName  string
	LastName   string
	ScreenName string
	Domain     string
	City       string
}

// ProfileURL is where the member can be found.
func (m Member) ProfileURL() string {
	handle := m.ScreenName
	if handle == "" {
		handle = m.Domain
	}
	if handle == "" {
		handle = "id" + m.ID
	}
	return "https://vk.com/" + handle
}

// Row is one target person's outcome in one community.
type Row struct {
	SourceVKURL     string `json:"source_vk_url"`
	ResolvedGroupID string `json:"resolved_group_id"`
	TargetPerson    string `json:"target_person"`
	FoundUserID     string `json:"found_user_id"`
	FoundFirstName  string `json:"found_first_name"`
	FoundLastName   string `json:"found_last_name"`
	FoundProfileURL string `json:"found_profile_url"`
	Status          string `json:"status"`
	Confidence      string `json:"confidence"`
	Error           string `json:"error"`
	RowKey          string `json:"row_key,omitempty"`
}

// Blocked reports whether this row means "we never got in" rather than "nobody
// matched".
func (r Row) Blocked() bool { return BlockStatuses[r.Status] }

// Progress reports paging as it happens; a long community is otherwise silent
// for minutes.
type Progress func(status string, fields map[string]any)

func (p Progress) emit(status string, fields map[string]any) {
	if p != nil {
		p(status, fields)
	}
}

var (
	groupPathRe = regexp.MustCompile(`^(?:club|public)(\d+)$`)
	eventPathRe = regexp.MustCompile(`^event(\d+)$`)
)

// NormalizeGroupURL extracts the vk.com path segment a community link points
// at — a screen name, or club<id>/public<id>/event<id>. "" if it is not a VK
// link at all.
func NormalizeGroupURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	host = strings.TrimPrefix(host, "www.")
	switch host {
	case "vk.com", "vkontakte.ru", "m.vk.com":
	default:
		return ""
	}
	for _, part := range strings.Split(strings.Trim(parsed.Path, "/"), "/") {
		if part == "" {
			continue
		}
		return strings.TrimPrefix(strings.TrimSpace(part), "@")
	}
	return ""
}

// maxRetries is how many times a short rate limit is waited out before the
// call reports rate_limited.
const maxRetries = 3

// call performs one VK API method and unwraps its `response`, waiting out
// VK's short rate limits.
func call(ctx context.Context, cfg Config, method string, params url.Values) (json.RawMessage, error) {
	wait := cfg.retryDelay()
	for attempt := 0; ; attempt++ {
		raw, err := callOnce(ctx, cfg, method, params)
		apiErr, ok := err.(*Error)
		if !ok || !retryCodes[apiErr.Code] || attempt >= maxRetries {
			return raw, err
		}
		if sleep(ctx, wait) != nil {
			return nil, err
		}
		wait *= 2
	}
}

func callOnce(ctx context.Context, cfg Config, method string, params url.Values) (json.RawMessage, error) {
	params.Set("access_token", cfg.AccessToken)
	params.Set("v", cfg.apiVersion())

	endpoint := cfg.base() + "/" + method + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &Error{Status: "network_error", Message: err.Error()}
	}
	resp, err := cfg.client().Do(req)
	if err != nil {
		return nil, &Error{Status: "network_error", Message: fmt.Sprintf("VK API недоступен: %v", err)}
	}
	defer resp.Body.Close()

	var envelope struct {
		Response json.RawMessage `json:"response"`
		Error    *struct {
			Code    int    `json:"error_code"`
			Message string `json:"error_msg"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, &Error{Status: "network_error", Message: fmt.Sprintf("VK API вернул не JSON: %v", err)}
	}
	if envelope.Error != nil {
		status, known := statusByCode[envelope.Error.Code]
		if !known {
			status = "vk_error"
		}
		message := fmt.Sprintf("VK API error %d", envelope.Error.Code)
		if envelope.Error.Message != "" {
			message += ": " + envelope.Error.Message
		}
		return nil, &Error{Status: status, Message: message, Code: envelope.Error.Code}
	}
	return envelope.Response, nil
}

// Group is a community as groups.getById describes it.
type Group struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	ScreenName  string `json:"screen_name"`
	Deactivated string `json:"deactivated"`
	Contacts    []struct {
		UserID int    `json:"user_id"`
		Desc   string `json:"desc"`
		Phone  string `json:"phone"`
		Email  string `json:"email"`
	} `json:"contacts"`
}

// GetGroup looks a community up by the path segment of its link: a screen
// name, or club<id>/public<id>/event<id>. fields are extra fields to ask for,
// such as "contacts".
func GetGroup(ctx context.Context, cfg Config, path, fields string) (Group, error) {
	id := path
	if m := groupPathRe.FindStringSubmatch(path); m != nil {
		id = m[1]
	} else if m := eventPathRe.FindStringSubmatch(path); m != nil {
		id = m[1]
	}
	params := url.Values{"group_id": {id}}
	if fields != "" {
		params.Set("fields", fields)
	}
	notGroup := &Error{Status: "not_a_group", Message: fmt.Sprintf("%q — не сообщество VK", path)}
	raw, err := call(ctx, cfg, "groups.getById", params)
	if err != nil {
		// 100: the screen name is not a community's (a person's, or nobody's).
		if apiErr, ok := err.(*Error); ok && apiErr.Code == 100 {
			return Group{}, notGroup
		}
		return Group{}, err
	}
	// Since API 5.194 the groups come wrapped in an object; before, bare.
	var groups []Group
	var wrapped struct {
		Groups []Group `json:"groups"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Groups != nil {
		groups = wrapped.Groups
	} else if err := json.Unmarshal(raw, &groups); err != nil {
		return Group{}, &Error{Status: "network_error", Message: fmt.Sprintf("непонятный ответ VK: %v", err)}
	}
	if len(groups) == 0 || groups[0].ID == 0 {
		return Group{}, notGroup
	}
	if groups[0].Deactivated != "" {
		return groups[0], &Error{Status: "not_a_group", Message: fmt.Sprintf("сообщество %q удалено или заблокировано (%s)", path, groups[0].Deactivated)}
	}
	return groups[0], nil
}

// ResolveGroupID turns a path segment into a numeric community id.
func ResolveGroupID(ctx context.Context, cfg Config, path string) (int, error) {
	g, err := GetGroup(ctx, cfg, path, "")
	return g.ID, err
}

// FetchMembers pages through a community's subscriber list.
//
// Cancellation returns what was collected so far rather than an error: a
// half-walked community still answers "is this person in it" for everyone
// found, and the caller writes those.
func FetchMembers(ctx context.Context, cfg Config, groupID int, progress Progress) ([]Member, error) {
	var members []Member
	offset, total := 0, -1

	for total < 0 || offset < total {
		if ctx.Err() != nil {
			return members, nil
		}
		raw, err := call(ctx, cfg, "groups.getMembers", url.Values{
			"group_id": {strconv.Itoa(groupID)},
			"offset":   {strconv.Itoa(offset)},
			"count":    {strconv.Itoa(pageSize)},
			"fields":   {memberFields},
			"sort":     {"id_asc"},
		})
		if err != nil {
			return members, err
		}
		var page struct {
			Count int `json:"count"`
			Items []struct {
				ID         int    `json:"id"`
				FirstName  string `json:"first_name"`
				LastName   string `json:"last_name"`
				ScreenName string `json:"screen_name"`
				Domain     string `json:"domain"`
				City       *struct {
					Title string `json:"title"`
				} `json:"city"`
			} `json:"items"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return members, &Error{Status: "network_error", Message: fmt.Sprintf("непонятный ответ VK: %v", err)}
		}
		total = page.Count
		for _, item := range page.Items {
			member := Member{
				ID:         strconv.Itoa(item.ID),
				FirstName:  item.FirstName,
				LastName:   item.LastName,
				ScreenName: item.ScreenName,
				Domain:     item.Domain,
			}
			if item.City != nil {
				member.City = item.City.Title
			}
			members = append(members, member)
		}
		progress.emit("vk_paging", map[string]any{
			"group_id": groupID, "collected": len(members), "total": total,
		})
		if len(page.Items) == 0 {
			break
		}
		offset += len(page.Items)
		if err := sleep(ctx, cfg.delay()); err != nil {
			return members, nil
		}
	}
	return members, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var nonNameRe = regexp.MustCompile(`[^a-zа-я\s-]`)
var spacesRe = regexp.MustCompile(`\s+`)

func normalizeName(value string) string {
	text := strings.ToLower(strings.NewReplacer("ё", "е", "Ё", "Е").Replace(value))
	text = nonNameRe.ReplaceAllString(text, " ")
	return strings.TrimSpace(spacesRe.ReplaceAllString(text, " "))
}

func tokens(value string) map[string]bool {
	out := map[string]bool{}
	for _, token := range strings.Split(normalizeName(value), " ") {
		// Drop one-character initials and particles; keep short real names
		// («Ян», «Ли»).
		if len([]rune(token)) >= 2 {
			out[token] = true
		}
	}
	return out
}

// MatchPerson compares a registry name against a subscriber's structured name.
//
// Two tokens have to line up, never one. A registry ФИО is three tokens and a
// VK member carries two, so demanding all of them would never confirm anybody;
// accepting one would confirm everybody, because every community holds an Иван
// and naming the wrong person as a company's owner is worse than finding none.
func MatchPerson(target, firstName, lastName string) (confidence string, score int) {
	targetTokens := tokens(target)
	memberTokens := tokens(firstName + " " + lastName)
	overlap := 0
	for token := range targetTokens {
		if memberTokens[token] {
			overlap++
		}
	}
	if overlap >= 2 {
		return "high", 100
	}
	return "", 0
}

// SearchPeople walks one community and reports each target person's outcome.
//
// Always returns a row per person, including for the failures — a caller that
// only saw an empty slice could not tell a closed community from an absent one.
func SearchPeople(ctx context.Context, cfg Config, progress Progress) []Row {
	token := strings.TrimSpace(cfg.AccessToken)
	if token == "" {
		return statusRows(cfg.GroupURL, "", cfg.People, "invalid_token",
			"Нет токена VK — укажите его в настройках.")
	}
	cfg.AccessToken = token

	path := NormalizeGroupURL(cfg.GroupURL)
	if path == "" {
		return statusRows(cfg.GroupURL, "", cfg.People, "invalid_url",
			fmt.Sprintf("Не похоже на ссылку на сообщество VK: %s", cfg.GroupURL))
	}

	groupID, err := ResolveGroupID(ctx, cfg, path)
	if err != nil {
		return statusRows(cfg.GroupURL, "", cfg.People, statusOf(err), err.Error())
	}

	members, err := FetchMembers(ctx, cfg, groupID, progress)
	if err != nil && len(members) == 0 {
		return statusRows(cfg.GroupURL, strconv.Itoa(groupID), cfg.People, statusOf(err), err.Error())
	}
	// A walk cut short answers nobody's absence: every row carries the
	// failure, so the community is walked again on the next run, and those
	// already found keep what was found.
	partialStatus, partialError := "", ""
	if err != nil {
		partialStatus = statusOf(err)
		partialError = fmt.Sprintf("просмотрено подписчиков: %d, дальше — %s", len(members), err.Error())
	}

	rows := make([]Row, 0, len(cfg.People))
	for _, person := range cfg.People {
		row := Row{
			SourceVKURL:     cfg.GroupURL,
			ResolvedGroupID: strconv.Itoa(groupID),
			TargetPerson:    person,
			Status:          "not_found",
		}
		best := 0
		for _, member := range members {
			confidence, score := MatchPerson(person, member.FirstName, member.LastName)
			if confidence == "" || score <= best {
				continue
			}
			best = score
			row.FoundUserID = member.ID
			row.FoundFirstName = member.FirstName
			row.FoundLastName = member.LastName
			row.FoundProfileURL = member.ProfileURL()
			row.Status = "ok"
			row.Confidence = confidence
		}
		if partialStatus != "" {
			row.Status, row.Error = partialStatus, partialError
		}
		rows = append(rows, row)
	}
	return rows
}

func statusOf(err error) string {
	var apiErr *Error
	if ok := asError(err, &apiErr); ok {
		return apiErr.Status
	}
	return "vk_error"
}

func asError(err error, target **Error) bool {
	if e, ok := err.(*Error); ok {
		*target = e
		return true
	}
	return false
}

// statusRows renders one failure as a row per target, so the caller sees the
// same shape whether the search ran or not.
func statusRows(sourceURL, groupID string, people []string, status, message string) []Row {
	targets := people
	if len(targets) == 0 {
		targets = []string{""}
	}
	rows := make([]Row, 0, len(targets))
	for _, person := range targets {
		rows = append(rows, Row{
			SourceVKURL:     sourceURL,
			ResolvedGroupID: groupID,
			TargetPerson:    person,
			Status:          status,
			Error:           message,
		})
	}
	return rows
}
