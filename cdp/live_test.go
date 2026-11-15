package cdp

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveChrome connects to the Chrome running on this machine. It is skipped
// unless CDP_LIVE=1, because it needs remote debugging switched on at
// chrome://inspect/#remote-debugging and a click on «Allow» in Chrome:
//
//	CDP_LIVE=1 go test ./cdp -run TestLiveChrome -v
//
// It opens one blank tab, evaluates in it and closes it; other tabs are not
// touched.
func TestLiveChrome(t *testing.T) {
	if os.Getenv("CDP_LIVE") != "1" {
		t.Skip("CDP_LIVE=1 включает проверку на настоящем Chrome")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	b, err := Connect(ctx, Options{
		Channel:            Channel(os.Getenv("CDP_CHANNEL")),
		OnAwaitingApproval: func() { t.Log("ждём «Разрешить» в окне Chrome…") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	t.Logf("подключились к %s", b.Endpoint())

	v, err := b.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Chrome: %s, протокол %s", v.Product, v.ProtocolVersion)

	page, err := b.Open(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = b.Conn().Call(context.Background(), "", "Target.closeTarget", map[string]any{"targetId": page.TargetID()}, nil)
	}()

	var sum int
	if err := page.Eval(ctx, "async (n) => n + 1", 41, &sum); err != nil || sum != 42 {
		t.Fatalf("evaluate = %d, %v", sum, err)
	}
	var agent string
	if err := page.EvalExpr(ctx, "navigator.userAgent", &agent); err != nil {
		t.Fatal(err)
	}
	t.Logf("evaluate: 41+1 = %d, userAgent = %s", sum, agent)
}
