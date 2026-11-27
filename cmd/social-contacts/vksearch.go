package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/artipop/social-contacts-skill/cdp"
	"github.com/artipop/social-contacts-skill/names"
	"github.com/artipop/social-contacts-skill/record"
	"github.com/artipop/social-contacts-skill/vkweb"
)

// searchUnit is one person of one input row: what vk-search looks up and
// writes one line for.
type searchUnit struct {
	Source, Person, City, Company string
}

func (u searchUnit) key() string { return unitKey(u.Source, u.Person) }

func unitKey(source, person string) string { return source + "\x1f" + strings.TrimSpace(person) }

type searchFlags struct {
	file, column, peopleColumn, cityColumn, companyColumn string
	people                                                stringList
	city, company                                         string
}

var (
	sourceColumns  = []string{"source", "id", "inn", "инн", "ogrn", "огрн", "key", "link", "url", "ссылка"}
	cityColumns    = []string{"city", "город", "town"}
	companyColumns = []string{"company", "company_name", "organization", "organisation", "employer", "work", "компания", "организация", "работа", "место_работы"}
)

// readSearchUnits turns the input into the people to look up: one unit per
// person of each row, keyed by the row's source column, or by its number when
// there is none.
func (a *app) readSearchUnits(f searchFlags) ([]searchUnit, error) {
	var units []searchUnit
	if f.file == "" {
		if len(f.people) == 0 {
			return nil, usageErr("нужен --input FILE или --person ФИО")
		}
		for _, p := range f.people {
			units = append(units, searchUnit{Source: strings.TrimSpace(p), Person: p, City: f.city, Company: f.company})
		}
		return dedupeUnits(units), nil
	}

	var data []byte
	var err error
	if f.file == "-" {
		data, err = io.ReadAll(a.stdin)
	} else {
		data, err = os.ReadFile(f.file)
	}
	if err != nil {
		return nil, usageErr("не прочитать %s: %v", f.file, err)
	}
	var rows []map[string]any
	if isJSONL(f.file, data) {
		rows, err = jsonlObjects(data)
	} else {
		rows, err = csvObjects(f.file, data)
	}
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, usageErr("во входе нет ни одной строки")
	}

	headers := map[string]bool{}
	for _, row := range rows {
		for k := range row {
			headers[k] = true
		}
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	pick := func(explicit string, known []string, what string) (string, error) {
		if i, ok := findColumn(keys, explicit, known); ok {
			return keys[i], nil
		}
		if explicit != "" {
			return "", usageErr("во входе нет колонки %q (%s)", explicit, what)
		}
		return "", nil
	}
	sourceCol, err := pick(f.column, sourceColumns, "источник")
	if err != nil {
		return nil, err
	}
	peopleCol, err := pick(f.peopleColumn, peopleColumns, "ФИО")
	if err != nil {
		return nil, err
	}
	cityCol, err := pick(f.cityColumn, cityColumns, "город")
	if err != nil {
		return nil, err
	}
	companyCol, err := pick(f.companyColumn, companyColumns, "место работы")
	if err != nil {
		return nil, err
	}

	for i, row := range rows {
		source := cellString(row[sourceCol])
		if sourceCol == "" || source == "" {
			source = "row " + strconv.Itoa(i+1)
		}
		city, company := cellString(row[cityCol]), cellString(row[companyCol])
		if f.city != "" {
			city = f.city
		}
		if f.company != "" {
			company = f.company
		}
		var people []string
		switch v := row[peopleCol].(type) {
		case string:
			people = splitPeople(v)
		case []any:
			for _, p := range v {
				if s, ok := p.(string); ok {
					people = append(people, s)
				}
			}
		}
		people = appendUnique(people, f.people...)
		for _, p := range people {
			units = append(units, searchUnit{Source: source, Person: p, City: city, Company: company})
		}
	}
	units = dedupeUnits(units)
	if len(units) == 0 {
		return nil, usageErr("во входе нет ФИО: укажите колонку (--people-column) или --person")
	}
	return units, nil
}

func cellString(v any) string {
	switch v := v.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func dedupeUnits(units []searchUnit) []searchUnit {
	seen := map[string]bool{}
	out := units[:0]
	for _, u := range units {
		u.Person = strings.TrimSpace(u.Person)
		if u.Person == "" || seen[u.key()] {
			continue
		}
		seen[u.key()] = true
		out = append(out, u)
	}
	return out
}

// csvObjects reads a CSV with a header row into one map per data row.
func csvObjects(name string, data []byte) ([]map[string]any, error) {
	data = bytes.TrimPrefix(data, []byte("\uFEFF"))
	r := csv.NewReader(bytes.NewReader(data))
	r.Comma = delimiter(name, data)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	rows, err := r.ReadAll()
	if err != nil {
		return nil, usageErr("CSV не читается: %v", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	header := rows[0]
	var out []map[string]any
	for _, row := range rows[1:] {
		obj := map[string]any{}
		for i, h := range header {
			if i < len(row) {
				obj[h] = row[i]
			}
		}
		out = append(out, obj)
	}
	return out, nil
}

func jsonlObjects(data []byte) ([]map[string]any, error) {
	var out []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(bytes.TrimPrefix(data, []byte("\uFEFF"))))
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)
	line := 0
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(text, &obj); err != nil {
			return nil, usageErr("JSONL, строка %d: нужен объект: %v", line, err)
		}
		out = append(out, obj)
	}
	if err := scanner.Err(); err != nil {
		return nil, usageErr("JSONL не читается: %v", err)
	}
	return out, nil
}

func decodeVKSearch(line []byte) (string, classified, error) {
	var r record.VKSearch
	err := json.Unmarshal(line, &r)
	if r.Source == "" {
		return "", r, err
	}
	return unitKey(r.Source, r.TargetPerson), r, err
}

func (a *app) vkSearch(ctx context.Context, args []string) int {
	fs := a.flagSet("vk-search", `social-contacts vk-search --input FILE | --person ФИО [флаги]

Найти людей по ФИО в поиске людей VK (vk.com/search/people) в браузере
пользователя, где он вошёл в VK. Сначала ищется имя с местом работы, если никого
— имя с городом. Совпадением считается только фамилия вместе с именем;
уверенность high, если в строке результата видны город или место работы, иначе
medium. Профили не открываются. Между поисками — пауза в несколько секунд; на
капче поиск останавливается, капчи не решаются.`)
	var f searchFlags
	fs.StringVar(&f.file, "input", "", "файл со строками: CSV с заголовком или JSONL (\"-\" — stdin)")
	fs.StringVar(&f.column, "column", "", "колонка-ключ строки для склейки (по умолчанию source, id, inn, инн…; без неё — номер строки)")
	fs.StringVar(&f.peopleColumn, "people-column", "", "колонка с ФИО; по умолчанию people, names, фио, лпр…")
	fs.StringVar(&f.cityColumn, "city-column", "", "колонка с городом; по умолчанию city, город")
	fs.StringVar(&f.companyColumn, "company-column", "", "колонка с местом работы; по умолчанию company, компания, организация…")
	fs.Var(&f.people, "person", "ФИО, кого искать (можно несколько раз); с --input добавляется к каждой строке")
	fs.StringVar(&f.city, "city", "", "город для всех строк, например «Москва»")
	fs.StringVar(&f.company, "company", "", "место работы для всех строк")
	var out outputFlags
	out.register(fs)
	var bf browserFlags
	bf.register(fs, a.getenv)
	surnameFirst := fs.Bool("surname-first", false, "ФИО записаны фамилией вперёд, как в реестрах")
	pause := fs.Duration("pause", 6*time.Second, "пауза между поисками; к ней добавляется случайно до 4 с")
	positional, code := a.parse(fs, args)
	if code >= 0 {
		return code
	}
	if len(positional) > 0 {
		fmt.Fprintln(a.stderr, "vk-search не принимает ссылок: --input FILE или --person ФИО")
		return exitUsage
	}
	units, err := a.readSearchUnits(f)
	if err != nil {
		fmt.Fprintln(a.stderr, strings.TrimPrefix(err.Error(), "usage: "))
		return exitUsage
	}
	settled, err := loadDone(out.path, decodeVKSearch, a.stderr)
	if err != nil {
		fmt.Fprintln(a.stderr, strings.TrimPrefix(err.Error(), "usage: "))
		return exitUsage
	}
	r := &run{tally: tally{}}
	var pending []searchUnit
	for _, u := range units {
		if outcome, ok := settled[u.key()]; ok {
			r.tally[outcome]++
			continue
		}
		pending = append(pending, u)
	}
	if skipped := len(units) - len(pending); skipped > 0 {
		fmt.Fprintf(a.stderr, "людей: %d, уже готово: %d, осталось: %d\n", len(units), skipped, len(pending))
	}
	if r.sink, err = a.openSink(out.path); err != nil {
		fmt.Fprintln(a.stderr, strings.TrimPrefix(err.Error(), "usage: "))
		return exitUsage
	}
	defer r.sink.close()
	if len(pending) == 0 {
		return a.finish(r)
	}

	s, code := a.connectBrowser(ctx, bf)
	if s == nil {
		return code
	}
	defer s.Close()
	pg, code := a.openPage(ctx, s, cdp.VK)
	if pg == nil {
		return code
	}
	// VK renders the results only in a visible tab.
	if err := s.Activate(ctx, pg.TargetID()); err != nil {
		fmt.Fprintf(a.stderr, "вкладка VK не выведена вперёд: %v\n", err)
	}
	searcher, err := a.openVKWeb(ctx, pg, vkweb.Options{
		SurnameFirst: *surnameFirst, Pause: *pause, Progress: a.progress,
	})
	if err != nil {
		code := a.collectorOpenFailed(ctx, pg, err)
		pg.Close()
		return code
	}
	defer searcher.Close()

	cache := map[string]vkweb.Result{}
	for i, u := range pending {
		fmt.Fprintf(a.stderr, "[%d/%d] %s: %s\n", i+1, len(pending), u.Source, u.Person)
		same := strings.Join([]string{names.Normalize(u.Person), names.Normalize(u.City), names.Normalize(u.Company)}, "|")
		res, ok := cache[same]
		if !ok {
			res = searcher.Search(ctx, u.Person, u.City, u.Company)
		}
		if ctx.Err() != nil {
			return exitInterrupted
		}
		if a.lostBrowser(pg) {
			return exitBrowser
		}
		res.TargetPerson = u.Person
		rec := record.VKSearch{Source: u.Source, Result: res}
		if rec.Outcome() == record.NeedsLogin {
			fmt.Fprintf(a.stderr, "%v\n", vkweb.ErrNotLoggedIn)
			return exitLogin
		}
		if rec.Final() {
			cache[same] = res
		}
		if err := r.sink.write(rec); err != nil {
			return a.writeFailed(err)
		}
		r.tally[rec.Outcome()]++
		fmt.Fprintf(a.stderr, "  %s: %s, кандидатов %d%s\n", rec.Outcome(), res.Status, len(res.Candidates), detail(res.Error))
		if res.Status == "captcha_or_challenge" || res.Status == "rate_limited" {
			fmt.Fprintln(a.stderr, "VK показал капчу или ограничил поиск: запуск остановлен, повторите позже с тем же --out")
			return a.finish(r)
		}
	}
	return a.finish(r)
}
