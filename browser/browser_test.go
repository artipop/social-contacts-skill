package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakePage stands in for a window: instead of running the script it hands it to
// a function that answers the way a page would, by posting to the bridge.
func fakePage(t *testing.T, answer func(id, expression string)) *Page {
	t.Helper()
	bridge, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)

	return NewPage(Window{
		ExecJS: func(js string) {
			id, expression := readScript(t, js)
			go answer(id, expression)
		},
		SetURL: func(string) {},
	}, bridge)
}

var (
	idRe   = regexp.MustCompile(`id: "([0-9a-f]+)"`)
	exprRe = regexp.MustCompile(`(?s)value: await \((.*?)\) \};`)
)

// readScript pulls the request id and the expression back out of the wrapper,
// which is how the fake knows what it was asked.
func readScript(t *testing.T, js string) (id, expression string) {
	t.Helper()
	m := idRe.FindStringSubmatch(js)
	if m == nil {
		t.Fatalf("в скрипте нет id:\n%s", js)
	}
	e := exprRe.FindStringSubmatch(js)
	if e == nil {
		t.Fatalf("в скрипте нет выражения:\n%s", js)
	}
	return m[1], e[1]
}

// post answers a call the way the page's fetch would.
func post(t *testing.T, endpoint string, payload map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Error(err)
		return
	}
	resp, err := http.Post(endpoint, "text/plain", strings.NewReader(string(body)))
	if err != nil {
		t.Error(err)
		return
	}
	resp.Body.Close()
}

func TestEvalCarriesTheArgumentAndReturnsTheValue(t *testing.T) {
	var page *Page
	page = fakePage(t, func(id, expression string) {
		// The argument travels inside the expression, encoded as JSON — the
		// page is never handed a separate parameter to bind.
		if !strings.Contains(expression, `{"op":"whoami"}`) {
			t.Errorf("аргумент не доехал: %s", expression)
		}
		post(t, page.bridge.url, map[string]any{
			"id": id, "ok": true, "value": map[string]any{"ok": true, "my_id": "42"},
		})
	})

	var got struct {
		OK   bool   `json:"ok"`
		MyID string `json:"my_id"`
	}
	if err := page.Eval(context.Background(), "async (p) => p", map[string]any{"op": "whoami"}, &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.MyID != "42" {
		t.Fatalf("вернулось %+v", got)
	}
}

func TestAThrownErrorComesBackAsAnError(t *testing.T) {
	// A page that throws must still report, or the caller waits out the whole
	// timeout for an answer that already exists.
	var page *Page
	page = fakePage(t, func(id, _ string) {
		post(t, page.bridge.url, map[string]any{
			"id": id, "ok": false, "error": "managers_not_ready",
		})
	})

	err := page.EvalExpr(context.Background(), "boom()", nil)
	if err == nil || !strings.Contains(err.Error(), "managers_not_ready") {
		t.Fatalf("ошибка страницы = %v", err)
	}
}

func TestAnAnswerForAnotherCallIsIgnored(t *testing.T) {
	// Results are addressed by an id we generated and are waiting on; anything
	// else is dropped rather than resolving somebody's call with a stranger's
	// data.
	var page *Page
	page = fakePage(t, func(id, _ string) {
		post(t, page.bridge.url, map[string]any{"id": "не-наш-id", "ok": true, "value": 1})
		post(t, page.bridge.url, map[string]any{"id": id, "ok": true, "value": 7})
	})

	var got int
	if err := page.EvalExpr(context.Background(), "7", &got); err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("вернулось %d", got)
	}
}

func TestCancellingStopsWaiting(t *testing.T) {
	// A turn that is stopped must not sit here until the eval timeout.
	page := fakePage(t, func(string, string) { /* the page never answers */ })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := page.EvalExpr(ctx, "neverResolves()", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена вернула %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("отмена заняла %s", elapsed)
	}
}

func TestScriptSurvivesAwkwardExpressions(t *testing.T) {
	// The wrapper embeds the expression as source, so anything that could end a
	// string or a comment early would break it. The collectors' JavaScript is
	// full of both.
	js := script("abc123", `(() => { /* "quotes" and `+"`backticks`"+` */ return "\n"; })()`,
		"http://127.0.0.1:1/result")
	if !strings.Contains(js, `id: "abc123"`) {
		t.Fatalf("id не встроился:\n%s", js)
	}
	if strings.Count(js, "await fetch(") != 1 {
		t.Fatalf("обёртка повреждена:\n%s", js)
	}
}
