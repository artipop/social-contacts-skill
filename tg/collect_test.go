package tg

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// scriptedPage stands in for a Telegram Web tab: it answers the bridge's ops
// from a function instead of running the JavaScript.
type scriptedPage struct {
	mu     sync.Mutex
	answer func(params map[string]any) map[string]any
	calls  []map[string]any
}

func (p *scriptedPage) URL(context.Context) (string, error)         { return telegramWebURL, nil }
func (p *scriptedPage) Navigate(context.Context, string) error      { return nil }
func (p *scriptedPage) EvalExpr(context.Context, string, any) error { return nil }
func (p *scriptedPage) Close()                                      {}

func (p *scriptedPage) Eval(_ context.Context, fn string, arg any, out any) error {
	var value any = true // jsIsLoggedIn
	if fn == jsBridge {
		params := arg.(map[string]any)
		p.mu.Lock()
		p.calls = append(p.calls, params)
		p.mu.Unlock()
		if params["op"] == "whoami" {
			value = map[string]any{"ok": true, "my_id": "1"}
		} else {
			value = p.answer(params)
		}
	}
	data, _ := json.Marshal(value)
	return json.Unmarshal(data, out)
}

func member(id, name string) map[string]any {
	return map[string]any{"user_id": id, "name": name, "username": "u" + id}
}

func TestFloodWaitIsHonouredAndTheSamePageRetried(t *testing.T) {
	floods := 0
	page := &scriptedPage{answer: func(params map[string]any) map[string]any {
		switch params["op"] {
		case "resolve":
			return map[string]any{"ok": true, "id": "100", "is_megagroup": true}
		case "participants":
			if floods == 0 {
				floods++
				return map[string]any{"ok": false, "error": "participants_failed: FLOOD_WAIT_1", "flood_wait": 1}
			}
			return map[string]any{"ok": true, "count": 2, "returned": 2,
				"members": []any{member("1", "Анна"), member("2", "Борис")}}
		}
		return map[string]any{"ok": false, "error": "unknown_op"}
	}}

	var events []map[string]any
	c, err := Open(context.Background(), page, Options{Progress: func(status string, fields map[string]any) {
		if status == "tg_flood_wait" {
			events = append(events, fields)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	res := c.Collect(context.Background(), "https://t.me/somegroup")
	if res.Status != "ok" || len(res.Members) != 2 {
		t.Fatalf("результат = %+v", res)
	}
	if len(events) != 1 || events[0]["seconds"] != 1 || events[0]["honored"] != true {
		t.Fatalf("о паузе не сообщили как надо: %+v", events)
	}
	offsets := 0
	for _, call := range page.calls {
		if call["op"] == "participants" {
			if call["offset"] != 0 {
				t.Errorf("после паузы запрошен offset %v", call["offset"])
			}
			offsets++
		}
	}
	if offsets != 2 {
		t.Errorf("страница участников запрошена %d раз, ожидалось 2", offsets)
	}
}

func TestDeepSearchSweepsTheAlphabetAndDeduplicates(t *testing.T) {
	page := &scriptedPage{answer: func(params map[string]any) map[string]any {
		switch params["op"] {
		case "resolve":
			return map[string]any{"ok": true, "id": "100", "is_megagroup": true}
		case "participants":
			// Every query returns the same person plus one of its own.
			q, _ := params["q"].(string)
			return map[string]any{"ok": true, "count": 500, "returned": 2,
				"members": []any{member("1", "Анна"), member("q"+q, "Человек "+q)}}
		}
		return map[string]any{"ok": false}
	}}
	c, err := Open(context.Background(), page, Options{DeepSearch: true})
	if err != nil {
		t.Fatal(err)
	}
	res := c.Collect(context.Background(), "@somegroup")

	var queries []string
	for _, call := range page.calls {
		if call["op"] == "participants" {
			queries = append(queries, call["q"].(string))
		}
	}
	if len(queries) != len(searchQueries) {
		t.Fatalf("запросов %d, ожидалось %d", len(queries), len(searchQueries))
	}
	for i, q := range searchQueries {
		if queries[i] != q {
			t.Fatalf("запрос %d = %q, ожидалось %q", i, queries[i], q)
		}
	}
	// One shared member plus one per query, each counted once.
	if len(res.Members) != 1+len(searchQueries) || res.Total != 500 {
		t.Fatalf("собрано %d (всего %d)", len(res.Members), res.Total)
	}
}
