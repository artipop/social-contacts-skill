package ig

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// statePage stands in for an Instagram tab that only ever shows one state.
type statePage struct {
	state   map[string]any
	navErr  error
	visited []string
}

func (p *statePage) URL(context.Context) (string, error) { return "https://www.instagram.com/", nil }
func (p *statePage) Navigate(_ context.Context, url string) error {
	p.visited = append(p.visited, url)
	return p.navErr
}
func (p *statePage) Eval(context.Context, string, any, any) error { return nil }
func (p *statePage) Close()                                       {}

func (p *statePage) EvalExpr(_ context.Context, expression string, out any) error {
	if expression != "("+jsPageState+")()" {
		return errors.New("не ожидали " + expression[:20])
	}
	data, _ := json.Marshal(p.state)
	return json.Unmarshal(data, out)
}

func TestACaptchaIsReportedAsBlockedNotAsNobody(t *testing.T) {
	page := &statePage{state: map[string]any{"url": "https://www.instagram.com/nasa/", "text": "help us confirm it's you"}}
	s, err := Open(context.Background(), page, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res := s.Search(context.Background(), "instagram.com/nasa", []string{"Иванов Иван"})
	if res.Status != "captcha_or_challenge" || !res.Blocked() {
		t.Fatalf("капча = %+v", res)
	}
	if len(page.visited) != 1 || page.visited[0] != "https://www.instagram.com/nasa/" {
		t.Errorf("открыто %v", page.visited)
	}
}

func TestALoginWallStopsBeforeSearching(t *testing.T) {
	page := &statePage{state: map[string]any{"url": "https://www.instagram.com/accounts/login/", "login": true}}
	_, err := Open(context.Background(), page, Options{})
	if err == nil || !strings.Contains(err.Error(), "не выполнен вход") {
		t.Fatalf("без входа = %v", err)
	}
}

func TestANetworkFailureNamesTheLikelyCause(t *testing.T) {
	page := &statePage{state: map[string]any{}, navErr: errors.New("net::ERR_TUNNEL_CONNECTION_FAILED at https://www.instagram.com/nasa/")}
	s, err := Open(context.Background(), page, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res := s.Search(context.Background(), "https://www.instagram.com/nasa/", []string{"Иванов Иван"})
	if res.Status != "network_error" || !res.Blocked() {
		t.Fatalf("сетевая ошибка = %+v", res)
	}
}
