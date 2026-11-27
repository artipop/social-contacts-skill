// Command social-contacts collects contacts from Telegram, Instagram and VK
// for a desktop AI agent, as a skill: the agent passes a link or a file of
// links, runs the command (in the background for long jobs), and joins the
// JSON Lines it prints back onto its own data.
//
// Telegram and Instagram run in the user's own Chrome over the DevTools
// Protocol, in tabs where the user is already logged in; VK communities are
// read through its API with the user's token or an app's service key, and VK's
// people search in the browser. Everything collected stays on the user's
// machine.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is set at release build time.
var version = "dev"

// Exit codes. Collection commands summarize the whole run: per-input detail is
// in the records.
const (
	exitOK          = 0
	exitError       = 1 // anything else, including inputs that failed for no named reason
	exitUsage       = 2 // bad command line or unreadable input
	exitBrowser     = 3 // browser unreachable: not running, debugging off, connection denied or not allowed in time, tab or connection lost
	exitLogin       = 4 // Telegram, Instagram or VK web logged out, VK token missing or rejected
	exitBlocked     = 5 // some inputs were never looked at: captcha, rate limit, network, closed community
	exitNotFound    = 6 // every input was looked at and nobody was found
	exitInterrupted = 130
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := newApp(os.Stdin, os.Stdout, os.Stderr).run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// app is the command line with its dependencies, so tests can replace the
// browser, the collectors and the VK API.
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string

	connect       func(ctx context.Context, opts browserFlags, onAwaiting func()) (session, error)
	openTelegram  func(ctx context.Context, page page, opts tgOptions) (telegramCollector, error)
	openInstagram func(ctx context.Context, page page, opts igOptions) (instagramSearcher, error)
	openVKWeb     func(ctx context.Context, page page, opts vkwebOptions) (vkWebSearcher, error)
	vkBaseURL     string
	vkRetryDelay  time.Duration
}

func newApp(stdin io.Reader, stdout, stderr io.Writer) *app {
	return &app{
		stdin: stdin, stdout: stdout, stderr: stderr,
		getenv:        os.Getenv,
		connect:       connectCDP,
		openTelegram:  openTelegram,
		openInstagram: openInstagram,
		openVKWeb:     openVKWeb,
	}
}

func (a *app) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.usage(a.stderr)
		return exitUsage
	}
	command, rest := args[0], args[1:]
	switch command {
	case "status":
		return a.status(ctx, rest)
	case "open":
		return a.open(ctx, rest)
	case "tg-members":
		return a.telegramMembers(ctx, rest)
	case "ig-find":
		return a.instagramFind(ctx, rest)
	case "vk-find":
		return a.vkFind(ctx, rest)
	case "vk-contacts":
		return a.vkContacts(ctx, rest)
	case "vk-search":
		return a.vkSearch(ctx, rest)
	case "version", "--version", "-v":
		fmt.Fprintln(a.stdout, version)
		return exitOK
	case "help", "--help", "-h":
		a.usage(a.stdout)
		return exitOK
	}
	fmt.Fprintf(a.stderr, "неизвестная команда %q\n\n", command)
	a.usage(a.stderr)
	return exitUsage
}

func (a *app) usage(w io.Writer) {
	fmt.Fprint(w, `social-contacts — контакты из Telegram, Instagram и VK для ИИ-агента.

Команды:
  status [telegram|instagram|vk|vk-web]
                                   браузер доступен, вход в Telegram/Instagram/VK выполнен, токен VK задан
  open telegram|instagram|vk       открыть вкладку, чтобы пользователь вошёл
  tg-members <ссылка> | --input F  участники Telegram-каналов и групп
  ig-find <ссылка> --person ФИО    найти людей среди подписчиков Instagram-профилей
  vk-find <ссылка> --person ФИО    найти людей среди подписчиков сообществ VK
                                   (нужен VK_ACCESS_TOKEN или VK_SERVICE_TOKEN)
  vk-contacts <ссылка> | --input F публичные контакты сообществ VK (тот же токен)
  vk-search --input F | --person ФИО
                                   найти людей в поиске людей VK по ФИО, городу и месту работы
  version                          версия

Вход: одна ссылка или --input FILE (CSV или JSONL, "-" — stdin).
Выход: JSONL на stdout или в --out FILE; с --out повторный запуск пропускает
готовые входы. Прогресс — в stderr. Подробности: social-contacts <команда> -h.

Коды выхода:
  0  готово, что-то найдено
  1  прочая ошибка
  2  неверные аргументы или входной файл
  3  браузер недоступен: не запущен, отладка не включена, подключение отклонено
     или не разрешено вовремя, вкладка или соединение потеряны
  4  нужен вход в Telegram/Instagram/VK или токен VK
  5  часть входов заблокирована: капча, лимит, сеть, закрытое сообщество
  6  всё просмотрено, никого не нашли
  130 прервано
`)
}
