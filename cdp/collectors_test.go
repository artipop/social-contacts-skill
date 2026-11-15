package cdp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/artipop/social-contacts-skill/tg"
)

// The Telegram collector, end to end over this transport: tab lookup, the
// login check and the bridge ops all go through Runtime.evaluate.
func TestTelegramCollectorRunsOverCDP(t *testing.T) {
	f := newFakeChrome(t)
	f.on("Runtime.evaluate", func(c *fakeConn, req fakeRequest) {
		expr := expressionOf(t, req)
		switch {
		case expr == "location.href":
			c.reply(req, evalValue(map[string]any{"ok": true, "value": "https://web.telegram.org/k/"}))
			return
		case strings.Contains(expr, "chatlist-container"): // the login check
			c.reply(req, evalValue(map[string]any{"ok": true, "value": true}))
			return
		}
		// The bridge: the argument is the JSON after the function's closing
		// parenthesis.
		open := strings.LastIndex(expr, ")({")
		var params map[string]any
		if open < 0 || json.Unmarshal([]byte(expr[open+2:len(expr)-1]), &params) != nil {
			t.Errorf("не разобрать вызов моста: %.80s…", expr[max(0, len(expr)-80):])
			c.reply(req, evalValue(map[string]any{"ok": false, "error": "bad call"}))
			return
		}
		var value map[string]any
		switch params["op"] {
		case "whoami":
			value = map[string]any{"ok": true, "my_id": "1"}
		case "resolve":
			value = map[string]any{"ok": true, "id": "100", "is_megagroup": true, "username": "somegroup"}
		case "participants":
			value = map[string]any{"ok": true, "count": 1, "returned": 1, "members": []any{
				map[string]any{"user_id": "7", "name": "Анна", "username": "anna_example"},
			}}
		}
		c.reply(req, evalValue(map[string]any{"ok": true, "value": value}))
	})
	f.tabs = []Tab{{ID: "P", Type: "page", URL: "https://web.telegram.org/k/"}}
	page, err := f.connect(t).Page(context.Background(), Telegram)
	if err != nil {
		t.Fatal(err)
	}

	collector, err := tg.Open(context.Background(), page, tg.Options{})
	if err != nil {
		t.Fatal(err)
	}
	res := collector.Collect(context.Background(), "https://t.me/somegroup")
	if res.Status != "ok" || len(res.Members) != 1 || res.Members[0].PublicURL() != "https://t.me/anna_example" {
		t.Fatalf("результат = %+v", res)
	}
}
