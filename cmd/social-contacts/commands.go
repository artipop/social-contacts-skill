package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/artipop/social-contacts-skill/cdp"
	"github.com/artipop/social-contacts-skill/ig"
	"github.com/artipop/social-contacts-skill/record"
	"github.com/artipop/social-contacts-skill/tg"
	"github.com/artipop/social-contacts-skill/vk"
	"github.com/artipop/social-contacts-skill/vkweb"
)

// envVKToken holds the user's VK access token. Not a flag, so the token does
// not show up in the process list.
const envVKToken = "VK_ACCESS_TOKEN"

func (a *app) flagSet(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "%s\n\nФлаги:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parse reads flags and positional arguments in any order. The code is -1
// when the command should go on.
func (a *app) parse(fs *flag.FlagSet, args []string) ([]string, int) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, exitOK
			}
			return nil, exitUsage
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, -1
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// run is one collection command's state: what to do, what an earlier run
// already did, and where the records go.
type run struct {
	pending []item
	tally   tally
	sink    *sink
}

func (a *app) prepare(net network, positional []string, in inputFlags, out outputFlags, decode decodeLine, needPeople bool) (*run, int) {
	items, err := a.readInputs(net, positional, in)
	if err != nil {
		fmt.Fprintln(a.stderr, strings.TrimPrefix(err.Error(), "usage: "))
		return nil, exitUsage
	}
	if needPeople {
		withPeople := items[:0]
		for _, it := range items {
			if len(it.People) > 0 {
				withPeople = append(withPeople, it)
			}
		}
		if missing := len(items) - len(withPeople); missing > 0 {
			fmt.Fprintf(a.stderr, "пропущено ссылок без ФИО: %d (искать некого)\n", missing)
		}
		if len(withPeople) == 0 {
			fmt.Fprintln(a.stderr, "не переданы ФИО: укажите --person или колонку с ФИО (--people-column)")
			return nil, exitUsage
		}
		items = withPeople
	}

	settled, err := loadDone(out.path, decode, a.stderr)
	if err != nil {
		fmt.Fprintln(a.stderr, strings.TrimPrefix(err.Error(), "usage: "))
		return nil, exitUsage
	}
	r := &run{tally: tally{}}
	for _, it := range items {
		if outcome, ok := settled[it.Link]; ok {
			r.tally[outcome]++
			continue
		}
		r.pending = append(r.pending, it)
	}
	if skipped := len(items) - len(r.pending); skipped > 0 {
		fmt.Fprintf(a.stderr, "входов: %d, уже готово: %d, осталось: %d\n", len(items), skipped, len(r.pending))
	}
	if r.sink, err = a.openSink(out.path); err != nil {
		fmt.Fprintln(a.stderr, strings.TrimPrefix(err.Error(), "usage: "))
		return nil, exitUsage
	}
	return r, -1
}

func (a *app) finish(r *run) int {
	code := r.tally.exitCode()
	fmt.Fprintf(a.stderr, "итог: %s\n", r.tally)
	return code
}

func (a *app) writeFailed(err error) int {
	fmt.Fprintf(a.stderr, "не записать результат: %v\n", err)
	return exitError
}

func (a *app) telegramMembers(ctx context.Context, args []string) int {
	fs := a.flagSet("tg-members", `social-contacts tg-members <ссылка> | --input FILE [флаги]

Участники Telegram-каналов и групп: имена, @юзернеймы и телефоны тех, кто их
не скрыл. Ссылка вида https://t.me/name или @name. У канала-вещателя подписчики
скрыты — тогда берётся его группа обсуждений, если она есть. Telegram ограничивает
частоту запросов и сам говорит, сколько ждать; это соблюдается, поэтому большой
канал считается минутами. Нужен вход в Telegram Web в браузере.`)
	var in inputFlags
	in.register(fs, false)
	var out outputFlags
	out.register(fs)
	var bf browserFlags
	bf.register(fs, a.getenv)
	limit := fs.Int("limit", 0, "сколько участников максимум на канал; 0 — без ограничения")
	deep := fs.Bool("deep", false, "перебрать алфавит, чтобы обойти лимит ~200 недавних участников; намного дольше")
	bios := fs.Bool("bios", false, "дособрать описания профилей; по запросу на человека, заметно дольше")
	maxBios := fs.Int("max-bios", 0, "сколько описаний максимум на канал; 0 — все")
	timeout := fs.Duration("channel-timeout", 30*time.Minute, "предел на один канал")
	positional, code := a.parse(fs, args)
	if code >= 0 {
		return code
	}
	r, code := a.prepare(telegramNet, positional, in, out, decodeTelegram, false)
	if r == nil {
		return code
	}
	defer r.sink.close()
	if len(r.pending) == 0 {
		return a.finish(r)
	}

	s, code := a.connectBrowser(ctx, bf)
	if s == nil {
		return code
	}
	defer s.Close()
	pg, code := a.openPage(ctx, s, cdp.Telegram)
	if pg == nil {
		return code
	}
	collector, err := a.openTelegram(ctx, pg, tg.Options{
		MaxMembers: *limit, DeepSearch: *deep, CollectBios: *bios, MaxBios: *maxBios,
		ChannelTimeout: *timeout, Progress: a.progress,
	})
	if err != nil {
		code := a.collectorOpenFailed(ctx, pg, err)
		pg.Close()
		return code
	}
	defer collector.Close()

	for i, it := range r.pending {
		fmt.Fprintf(a.stderr, "[%d/%d] %s\n", i+1, len(r.pending), it.Link)
		var res tg.Result
		if tg.Collectable(it.Link) {
			res = collector.Collect(ctx, it.Link)
		} else {
			res = tg.Result{Channel: it.Link, Status: "unsupported_url", Error: "это бот — участников у него нет"}
		}
		if ctx.Err() != nil {
			return exitInterrupted
		}
		if a.lostBrowser(pg) {
			return exitBrowser
		}
		rec := record.Telegram{Source: it.Link, Result: res}
		if err := r.sink.write(rec); err != nil {
			return a.writeFailed(err)
		}
		r.tally[rec.Outcome()]++
		fmt.Fprintf(a.stderr, "  %s: %s, участников %d из %d%s\n",
			rec.Outcome(), res.Status, len(res.Members), res.Total, detail(res.Error))
	}
	return a.finish(r)
}

func (a *app) instagramFind(ctx context.Context, args []string) int {
	fs := a.flagSet("ig-find", `social-contacts ig-find <ссылка> --person ФИО | --input FILE [флаги]

Найти конкретных людей среди подписчиков Instagram-профиля по ФИО. Подписчики не
выгружаются целиком: используется поиск внутри диалога подписчиков, а русское
имя примеряется во всех правдоподобных латинских написаниях. Совпадением
считается только фамилия вместе с именем. Нужен вход в Instagram в браузере.`)
	var in inputFlags
	in.register(fs, true)
	var out outputFlags
	out.register(fs)
	var bf browserFlags
	bf.register(fs, a.getenv)
	surnameFirst := fs.Bool("surname-first", false, "ФИО записаны фамилией вперёд, как в реестрах")
	timeout := fs.Duration("profile-timeout", 5*time.Minute, "предел на один профиль")
	positional, code := a.parse(fs, args)
	if code >= 0 {
		return code
	}
	r, code := a.prepare(instagramNet, positional, in, out, decodeInstagram, true)
	if r == nil {
		return code
	}
	defer r.sink.close()
	if len(r.pending) == 0 {
		return a.finish(r)
	}

	s, code := a.connectBrowser(ctx, bf)
	if s == nil {
		return code
	}
	defer s.Close()
	pg, code := a.openPage(ctx, s, cdp.Instagram)
	if pg == nil {
		return code
	}
	searcher, err := a.openInstagram(ctx, pg, ig.Options{
		SurnameFirst: *surnameFirst, ProfileTimeout: *timeout, Progress: a.progress,
	})
	if err != nil {
		code := a.collectorOpenFailed(ctx, pg, err)
		pg.Close()
		return code
	}
	defer searcher.Close()

	for i, it := range r.pending {
		fmt.Fprintf(a.stderr, "[%d/%d] %s\n", i+1, len(r.pending), it.Link)
		res := searcher.Search(ctx, it.Link, it.People)
		if ctx.Err() != nil {
			return exitInterrupted
		}
		if a.lostBrowser(pg) {
			return exitBrowser
		}
		rec := record.Instagram{Source: it.Link, Result: res}
		if rec.Outcome() == record.NeedsLogin {
			fmt.Fprintf(a.stderr, "%v\n", ig.ErrNotLoggedIn)
			return exitLogin
		}
		if err := r.sink.write(rec); err != nil {
			return a.writeFailed(err)
		}
		r.tally[rec.Outcome()]++
		fmt.Fprintf(a.stderr, "  %s: %s, совпадений %d%s\n", rec.Outcome(), res.Status, len(res.Matches), detail(res.Error))
	}
	return a.finish(r)
}

func (a *app) vkFind(ctx context.Context, args []string) int {
	fs := a.flagSet("vk-find", `social-contacts vk-find <ссылка> --person ФИО | --input FILE [флаги]

Найти конкретных людей среди подписчиков сообщества VK по ФИО. Браузер не нужен:
работает через API VK с токеном пользователя из переменной `+envVKToken+`
(права groups). Закрытое сообщество ответит отказом в доступе. Совпадением
считается только фамилия вместе с именем.`)
	var in inputFlags
	in.register(fs, true)
	var out outputFlags
	out.register(fs)
	positional, code := a.parse(fs, args)
	if code >= 0 {
		return code
	}
	r, code := a.prepare(vkNet, positional, in, out, decodeVK, true)
	if r == nil {
		return code
	}
	defer r.sink.close()
	if len(r.pending) == 0 {
		return a.finish(r)
	}
	token := strings.TrimSpace(a.getenv(envVKToken))
	if token == "" {
		fmt.Fprintf(a.stderr, "нет токена VK: задайте переменную окружения %s\n", envVKToken)
		return exitLogin
	}

	for i, it := range r.pending {
		fmt.Fprintf(a.stderr, "[%d/%d] %s\n", i+1, len(r.pending), it.Link)
		rows := vk.SearchPeople(ctx, vk.Config{
			GroupURL: it.Link, People: it.People, AccessToken: token, BaseURL: a.vkBaseURL,
		}, a.progress)
		if ctx.Err() != nil {
			return exitInterrupted
		}
		var outcome record.Outcome
		found := 0
		for _, row := range rows {
			rec := record.VK{Source: it.Link, Row: row}
			if rec.Outcome() == record.NeedsLogin {
				fmt.Fprintf(a.stderr, "токен VK не принят (%s): получите новый и задайте %s\n", row.Error, envVKToken)
				return exitLogin
			}
			outcome = merge(outcome, rec.Outcome())
			if rec.Outcome() == record.Found {
				found++
			}
		}
		for _, row := range rows {
			if err := r.sink.write(record.VK{Source: it.Link, Row: row}); err != nil {
				return a.writeFailed(err)
			}
		}
		r.tally[outcome]++
		errText := ""
		if len(rows) > 0 && found == 0 {
			errText = rows[0].Error
		}
		fmt.Fprintf(a.stderr, "  %s: найдено %d из %d%s\n", outcome, found, len(it.People), detail(errText))
	}
	return a.finish(r)
}

func detail(text string) string {
	if text == "" {
		return ""
	}
	return " — " + text
}

// statusReport is what status prints.
type statusReport struct {
	Browser   *browserStatus `json:"browser,omitempty"`
	Telegram  *loginStatus   `json:"telegram,omitempty"`
	Instagram *loginStatus   `json:"instagram,omitempty"`
	VKWeb     *loginStatus   `json:"vk_web,omitempty"`
	VK        *tokenStatus   `json:"vk,omitempty"`
}

type browserStatus struct {
	Connected bool   `json:"connected"`
	Product   string `json:"product,omitempty"`
	Error     string `json:"error,omitempty"`
}

type loginStatus struct {
	LoggedIn bool   `json:"logged_in"`
	Error    string `json:"error,omitempty"`
}

type tokenStatus struct {
	Token bool `json:"token"`
}

func (a *app) status(ctx context.Context, args []string) int {
	fs := a.flagSet("status", `social-contacts status [telegram|instagram|vk|vk-web] [флаги]

Проверить, можно ли собирать: браузер доступен, вход в Telegram Web и Instagram
выполнен (без аргумента — оба), задан ли токен VK. "status vk-web" проверяет
вход в VK в браузере (для vk-search). Если вкладки нет, она открывается. Печатает JSON; код выхода 0, 3 (браузер) или 4
(нужен вход). "status vk" проверяет только токен VK и браузер не трогает.`)
	var bf browserFlags
	bf.register(fs, a.getenv)
	positional, code := a.parse(fs, args)
	if code >= 0 {
		return code
	}
	checkTG, checkIG, checkVK, checkVKWeb := true, true, false, false
	switch {
	case len(positional) > 1:
		fs.Usage()
		return exitUsage
	case len(positional) == 1:
		checkTG, checkIG, checkVK = false, false, false
		switch strings.ToLower(positional[0]) {
		case "telegram", "tg":
			checkTG = true
		case "instagram", "ig":
			checkIG = true
		case "vk":
			checkVK = true
		case "vk-web":
			checkVKWeb = true
		default:
			fmt.Fprintf(a.stderr, "неизвестная сеть %q: telegram, instagram, vk или vk-web\n", positional[0])
			return exitUsage
		}
	}

	report := statusReport{VK: &tokenStatus{Token: strings.TrimSpace(a.getenv(envVKToken)) != ""}}
	code = exitOK
	if checkVK && !report.VK.Token {
		fmt.Fprintf(a.stderr, "нет токена VK: задайте переменную окружения %s\n", envVKToken)
		code = exitLogin
	}
	if checkTG || checkIG || checkVKWeb {
		code = a.checkBrowser(ctx, bf, &report, checkTG, checkIG, checkVKWeb)
	}
	a.printJSON(a.stdout, report)
	return code
}

func (a *app) checkBrowser(ctx context.Context, bf browserFlags, report *statusReport, checkTG, checkIG, checkVKWeb bool) int {
	s, err := a.connect(ctx, bf, func() {
		fmt.Fprintln(a.stderr, "браузер спрашивает, разрешить ли подключение: нажмите «Разрешить» в его окне")
	})
	if err != nil {
		if ctx.Err() != nil {
			return exitInterrupted
		}
		report.Browser = &browserStatus{Error: err.Error()}
		fmt.Fprintf(a.stderr, "браузер недоступен: %v\n", err)
		return exitBrowser
	}
	defer s.Close()
	report.Browser = &browserStatus{Connected: true}
	if v, err := s.Version(ctx); err == nil {
		report.Browser.Product = v.Product
	}

	code := exitOK
	check := func(site cdp.Site, open func(page) (func(), error)) *loginStatus {
		pg, c := a.openPage(ctx, s, site)
		if pg == nil {
			code = max(code, c)
			return &loginStatus{Error: "вкладка не открылась"}
		}
		closeCollector, err := open(pg)
		if err == nil {
			closeCollector()
			return &loginStatus{LoggedIn: true}
		}
		defer pg.Close()
		switch {
		case ctx.Err() != nil:
			code = exitInterrupted
		case errors.Is(err, tg.ErrNotLoggedIn), errors.Is(err, ig.ErrNotLoggedIn), errors.Is(err, vkweb.ErrNotLoggedIn):
			code = max(code, exitLogin)
		case pg.Err() != nil:
			code = max(code, exitBrowser)
		default:
			code = max(code, exitError)
		}
		fmt.Fprintln(a.stderr, err)
		return &loginStatus{Error: err.Error()}
	}
	if checkTG {
		report.Telegram = check(cdp.Telegram, func(pg page) (func(), error) {
			c, err := a.openTelegram(ctx, pg, tg.Options{})
			if err != nil {
				return nil, err
			}
			return c.Close, nil
		})
	}
	if checkIG && code != exitInterrupted {
		report.Instagram = check(cdp.Instagram, func(pg page) (func(), error) {
			s, err := a.openInstagram(ctx, pg, ig.Options{})
			if err != nil {
				return nil, err
			}
			return s.Close, nil
		})
	}
	if checkVKWeb && code != exitInterrupted {
		report.VKWeb = check(cdp.VK, func(pg page) (func(), error) {
			s, err := a.openVKWeb(ctx, pg, vkweb.Options{})
			if err != nil {
				return nil, err
			}
			return s.Close, nil
		})
	}
	return code
}

func (a *app) open(ctx context.Context, args []string) int {
	fs := a.flagSet("open", `social-contacts open telegram|instagram|vk [флаги]

Открыть (или вывести вперёд) вкладку Telegram Web, Instagram или VK в браузере
пользователя, чтобы он вошёл в аккаунт. Войти за пользователя команда не может:
дождитесь, пока он скажет, что готово, и проверьте status.`)
	var bf browserFlags
	bf.register(fs, a.getenv)
	positional, code := a.parse(fs, args)
	if code >= 0 {
		return code
	}
	if len(positional) != 1 {
		fs.Usage()
		return exitUsage
	}
	site, ok := networkSite(positional[0])
	if !ok {
		fmt.Fprintf(a.stderr, "неизвестная сеть %q: telegram, instagram или vk\n", positional[0])
		return exitUsage
	}
	s, code := a.connectBrowser(ctx, bf)
	if s == nil {
		return code
	}
	defer s.Close()
	pg, code := a.openPage(ctx, s, site)
	if pg == nil {
		return code
	}
	defer pg.Close()
	if err := s.Activate(ctx, pg.TargetID()); err != nil {
		fmt.Fprintf(a.stderr, "вкладка открыта, но не выведена вперёд: %v\n", err)
	}
	fmt.Fprintln(a.stderr, "вкладка открыта: попросите пользователя войти в аккаунт и сказать, когда готово")
	a.printJSON(a.stdout, map[string]any{"network": strings.ToLower(positional[0]), "tab": pg.TargetID(), "url": site.URL})
	return exitOK
}

func (a *app) printJSON(w io.Writer, v any) {
	data, _ := json.Marshal(v)
	fmt.Fprintln(w, string(data))
}
