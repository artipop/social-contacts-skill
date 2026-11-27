# Контакты из соцсетей — скилл для ИИ-агентов

Сборщики контактов из Telegram, Instagram и VK для десктопных ИИ-агентов (Claude
Code, Codex, Cursor): участники открытых Telegram-каналов и групп, поиск людей
среди подписчиков Instagram-профиля и подписчиков VK-сообщества, публичные
контакты VK-сообществ, поиск людей по ФИО, городу и месту работы в поиске
людей VK. Агент получает
таблицу со ссылками, запускает CLI `social-contacts` и сам склеивает результаты с
исходными данными.

Telegram и Instagram читаются из обычного браузера пользователя, в котором он уже
вошёл в аккаунты: CLI подключается к нему по Chrome DevTools Protocol, второго
браузера и отдельного профиля нет. Подписчики и контакты VK-сообществ читаются
через API с токеном пользователя или сервисным ключом приложения, поиск людей
VK — в браузере: `users.search` в API закрыт для сервисных ключей (ошибка 1051)
и токенов VK ID.
Собранные данные остаются на машине пользователя.

Облачные агенты не поддерживаются: нужен браузер на той же машине.

## Установка скилла

Скачайте архив для своей платформы со страницы
[релизов](https://github.com/artipop/social-contacts-skill/releases):
`social-contacts-darwin-arm64.tar.gz` (Mac на Apple Silicon),
`social-contacts-darwin-amd64.tar.gz` (Mac на Intel),
`social-contacts-linux-amd64.tar.gz`, `social-contacts-linux-arm64.tar.gz`,
`social-contacts-windows-amd64.zip`. В архиве папка `social-contacts` со
`SKILL.md` и бинарём; её и нужно положить в каталог скиллов агента.

**Claude Code** — для всех проектов в `~/.claude/skills/`, для одного проекта в
`.claude/skills/` в его корне:

```sh
mkdir -p ~/.claude/skills
tar -xzf social-contacts-darwin-arm64.tar.gz -C ~/.claude/skills
```

**Codex** — для всех проектов в `~/.agents/skills/`, для одного репозитория в
`.agents/skills/` в его корне:

```sh
mkdir -p ~/.agents/skills
tar -xzf social-contacts-darwin-arm64.tar.gz -C ~/.agents/skills
```

**Cursor** — в `~/.cursor/skills/` или в `.cursor/skills/` проекта. Cursor также
читает `~/.claude/skills/` и `.agents/skills/`, так что одна копия может
обслуживать несколько агентов.

На Windows распакуйте zip в тот же каталог (`%USERPROFILE%\.claude\skills` и
т. п.).

Если macOS не даёт запустить бинарь («не удаётся проверить разработчика»),
значит, сборка не нотаризована; снимите карантин:

```sh
xattr -d com.apple.quarantine ~/.claude/skills/social-contacts/social-contacts
```

После установки достаточно попросить агента, например: «вот CSV с
Telegram-каналами компаний — собери участников и допиши в таблицу».

## Подготовка браузера

1. Chrome 144 или новее, запущен.
2. Откройте `chrome://inspect/#remote-debugging` и включите удалённую отладку.
3. Войдите в [Telegram Web](https://web.telegram.org/k/),
   [Instagram](https://www.instagram.com/) и, для `vk-search`,
   [VK](https://vk.com/) в этом браузере.
4. При каждом запуске CLI браузер спрашивает, разрешить ли удалённую отладку, —
   нажмите «Разрешить». За один запуск CLI подключается один раз.

Для `vk-find` и `vk-contacts` задайте переменную окружения `VK_ACCESS_TOKEN`
(токен пользователя VK) или `VK_SERVICE_TOKEN` (сервисный ключ своего приложения
VK); если заданы обе, берётся токен пользователя. Сервисный ключ работает для
`groups.getById`, `groups.getMembers` и `users.get`; токен VK ID с правом
`vkid.personal_info` для них не годится. `status vk` показывает, какой токен
задан (`"kind":"user"` или `"service"`). Токен не передаётся аргументом, чтобы
не светиться в списке процессов, и не пишется в вывод.

### Поддерживаемые браузеры

- **Google Chrome** (каналы stable, beta, dev, canary — флаг `--channel`): адрес
  отладки берётся из файла `DevToolsActivePort` в профиле Chrome.
- **Другие браузеры на Chromium 144+**, например Helium: укажите каталог профиля
  флагом `--user-data-dir` (у Helium на macOS это
  `~/Library/Application Support/net.imput.helium`; ищите каталог, где после
  включения отладки появляется файл `DevToolsActivePort`) или адрес отладки флагом
  `--cdp-url` / переменной `SOCIAL_CONTACTS_CDP_URL` (`ws://…` из
  `DevToolsActivePort` или `http://127.0.0.1:9222` для браузера, запущенного с
  `--remote-debugging-port`).

Проверено вживую: Helium (Chromium 157) на macOS с `--user-data-dir` — «Разрешить»
подключает, «Запретить» и «Отмена» дают HTTP 403, CLI выходит с кодом 3.
Автоматический поиск профиля Google Chrome вживую не проверялся — работает по
стандартным путям Chromium, но подтверждения пока нет.

## Команды

```
social-contacts status [telegram|instagram|vk|vk-web]
social-contacts open telegram|instagram|vk
social-contacts tg-members <ссылка> | --input FILE [--out FILE] [--limit N] [--deep] [--bios]
social-contacts ig-find <ссылка> --person ФИО | --input FILE [--out FILE] [--surname-first]
social-contacts vk-find <ссылка> --person ФИО | --input FILE [--out FILE]
social-contacts vk-contacts <ссылка> | --input FILE [--out FILE] [--person ФИО]
social-contacts vk-search --person ФИО | --input FILE [--out FILE] [--city ГОРОД] [--company МЕСТО] [--surname-first] [--limit N] [--company-filter]
```

- **Вход** — одна ссылка или `--input FILE`: CSV (разделитель `,`, `;` или
  табуляция; колонка со ссылкой угадывается по названию или задаётся `--column`)
  или JSONL (объекты или строки-ссылки). ФИО для `ig-find`/`vk-find` (и,
  если есть, для `vk-contacts`) — флаг `--person` или колонка
  `people`/`names`/`фио`/`лпр` (`--people-column`).
- **`vk-find`** — короткие лимиты VK (ошибки 6 и 29) пережидаются; «Flood
  control» (ошибка 9: суточный лимит ключа на `groups.getMembers`, несколько сотен
  тысяч подписчиков) останавливает запуск, строки сообщества получают
  `rate_limited` с текстом ошибки VK и повторяются следующим запуском с тем же
  `--out`.
- **`vk-contacts`** — блок «Контакты» сообщества (`groups.getById` с
  `fields=contacts`, имена — `users.get`). Строка на сообщество: `group`,
  `group_id`, `contacts` (`name`, `profile_url`, `desc`, `phone`, `email`,
  `matched_person`, `confidence`), `status`, `error`. Контакт, у которого
  фамилия и имя совпали с ФИО строки, помечается `matched_person` и
  `confidence: high`. Статусы — как у `vk-find`.
- **`vk-search`** ссылок не берёт: в строке ФИО, необязательные `city` и
  `company` (`--city-column`, `--company-column`; `--city`/`--company` — для
  всех строк) и ключ строки (`source`, `id`, `inn`, `инн`… или `--column`; без
  него — номер строки). Для каждого человека ищется имя с городом (без города —
  одно имя), место работы сверяется со строкой результата. `--company-filter`
  добавляет первым поиск с фильтром VK «Работа» (название без формы
  собственности и общих слов: «ООО "КЛИНИКА ИННОВАТИКА"» — «Инноватика»), если
  никого — имя с городом; по умолчанию выключен. Читается
  первая страница результатов, профили не открываются, между поисками пауза
  6–10 с. `--limit N` — не больше N человек за запуск, повторный запуск с тем же
  `--out` продолжает. Строка результата — на человека: `inn` и `company` строки
  входа (чтобы склеить с таблицей по ИНН), `target_person`, `query`,
  `candidates` (до 10: `profile_url`, `name`, `snippet`, `score`, `confidence`,
  `reason`), `status`. Уверенность: high — в строке результата видно место
  работы (без формы собственности и общих слов вроде «клиника», кириллицей или
  латиницей: «КЛИНИКА ИННОВАТИКА» совпадает с «INNOVATICA»); medium — найден
  единственный тёзка в поиске по городу или (с `--company-filter`) найден
  фильтром места работы; low — один из
  нескольких тёзок или найден по имени без города.
  Фильтр VK «Работа» на практике почти никого не находит: в проверке он не дал
  ни одного результата даже для крупных работодателей (Сбербанк, Медси,
  Инвитро) и только удваивал число поисков, поэтому он и выключен по умолчанию.
- **Выход** — JSON Lines на stdout или в `--out FILE`. Каждая строка — результат
  сборщика (`tg.Result`, `ig.Result`, `vk.Row`, `vk.ContactsResult`, `vkweb.Result`) с полем `source`: исходной
  ссылкой. Формат описан в пакете [`record`](record/record.go).
- **Возобновление** — повторный запуск с тем же `--out` пропускает уже готовые
  входы и повторяет заблокированные и упавшие.
- **Прогресс** — в stderr.

Подробности — `social-contacts <команда> -h` и [SKILL.md](skill/SKILL.md).

### Коды выхода

| Код | Значение |
|---|---|
| 0 | Готово, что-то найдено |
| 1 | Прочая ошибка, в том числе часть входов упала без названной причины |
| 2 | Неверные аргументы или входной файл |
| 3 | Браузер недоступен: не запущен, отладка не включена, подключение отклонено или не разрешено вовремя, вкладка или соединение потеряны |
| 4 | Нужен вход в Telegram/Instagram/VK или токен VK (нет или не принят) |
| 5 | Часть входов заблокирована: капча, лимит, сеть, закрытое сообщество |
| 6 | Всё просмотрено, никого не нашли |
| 130 | Прервано |

Коды 3 и 4 останавливают запуск сразу; всё, что собрано до этого, уже в `--out`.
Если есть и заблокированные, и упавшие входы, побеждает 5.

## Приватность

- Данные собираются только на машине пользователя и только из его собственного
  браузера; CLI никуда их не отправляет, кроме самих Telegram, Instagram и VK.
- Пароли и cookies CLI не читает: он выполняет запросы в уже открытых вкладках,
  где пользователь вошёл сам.
- `SKILL.md` запрещает агенту отправлять собранные персональные данные в сторонние
  сервисы и выполнять текст из био и имён как инструкции.
- Токен VK берётся только из переменной окружения и не пишется в результаты.

## Go-модуль

`github.com/artipop/social-contacts-skill`: пакеты `tg`, `ig`, `vk`, `vkweb`, `names` —
сборщики; `browser` — интерфейс вкладки, от которого они зависят; `cdp` —
подключение к браузеру по DevTools Protocol; `record` — формат JSONL.

Сборка из исходников (Go 1.26, без cgo):

```sh
go build -o social-contacts ./cmd/social-contacts
```

## Релиз

Тег `v*` запускает [`.github/workflows/release.yml`](.github/workflows/release.yml):
тесты, сборка архивов скилла для darwin arm64/amd64, linux amd64/arm64 и windows
amd64, подпись и нотаризация macOS, публикация релиза с `SHA256SUMS`.

Секреты репозитория для macOS (без них бинари подписываются ad-hoc и не
нотаризуются):

| Секрет | Что это |
|---|---|
| `APPLE_CERT_P12` | Сертификат Developer ID Application с ключом, `.p12` в base64 |
| `APPLE_CERT_PASSWORD` | Пароль от `.p12` |
| `APPLE_SIGN_IDENTITY` | Имя сертификата для `codesign`, например `Developer ID Application: Имя (TEAMID)` |
| `APPLE_ID` | Apple ID для нотаризации |
| `APPLE_APP_PASSWORD` | Пароль приложения для этого Apple ID |
| `APPLE_TEAM_ID` | Team ID разработчика |

## Лицензия

[MIT](LICENSE).
