package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/artipop/social-contacts-skill/ig"
	"github.com/artipop/social-contacts-skill/vk"
)

// network is which collector the links are for; it decides which columns hold
// them and what a link looks like.
type network int

const (
	telegramNet network = iota
	instagramNet
	vkNet
)

// item is one input: a link and, for the people searches, who to look for.
type item struct {
	Link   string
	People []string
}

// inputFlags are the input options every collection command shares.
type inputFlags struct {
	file         string
	column       string
	peopleColumn string
	people       stringList
}

func (f *inputFlags) register(fs *flag.FlagSet, withPeople bool) {
	fs.StringVar(&f.file, "input", "", "файл со ссылками: CSV или JSONL (\"-\" — stdin)")
	fs.StringVar(&f.column, "column", "", "колонка (CSV) или поле (JSONL) со ссылкой; по умолчанию угадывается по названию")
	if withPeople {
		fs.StringVar(&f.peopleColumn, "people-column", "", "колонка или поле с ФИО; по умолчанию people, names, фио, лпр…")
		fs.Var(&f.people, "person", "ФИО, кого искать (можно несколько раз); с --input добавляется к каждой строке")
	}
}

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, "; ") }
func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// errUsage marks a problem with the command line or the input file.
var errUsage = errors.New("usage")

func usageErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, args...))
}

// Column names that hold the link, most specific first. Headers are compared
// lower-cased, with spaces and dashes as underscores.
var linkColumns = map[network][]string{
	telegramNet:  {"telegram", "telegram_url", "telegram_link", "tg", "channel", "link", "url", "source", "ссылка", "телеграм"},
	instagramNet: {"instagram", "instagram_url", "instagram_link", "ig", "profile", "link", "url", "source", "ссылка", "инстаграм"},
	vkNet:        {"vk", "vk_url", "vk_link", "vkontakte", "group", "community", "link", "url", "source", "ссылка", "вк", "вконтакте"},
}

var peopleColumns = []string{"people", "persons", "person", "names", "decision_makers", "lpr", "лпр", "фио", "люди"}

func normalizeHeader(h string) string {
	h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")))
	return strings.NewReplacer(" ", "_", "-", "_").Replace(h)
}

// looksLikeLink tells a data row from a header row in a file without one.
func looksLikeLink(net network, value string) bool {
	value = strings.TrimSpace(value)
	switch net {
	case telegramNet:
		v := strings.ToLower(value)
		return strings.HasPrefix(v, "@") || strings.Contains(v, "t.me/") ||
			strings.Contains(v, "telegram.me/") || strings.Contains(v, "web.telegram.org")
	case instagramNet:
		return ig.NormalizeProfileURL(value) != ""
	case vkNet:
		return vk.NormalizeGroupURL(value) != ""
	}
	return false
}

// splitPeople reads a cell that may hold several names.
func splitPeople(cell string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(cell, func(r rune) bool {
		return r == ';' || r == ',' || r == '|' || r == '\n'
	}) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readInputs turns the command line into the list of inputs: one link, or a
// file of them. Repeated links are merged into one input, their people joined,
// so each link is collected once.
func (a *app) readInputs(net network, positional []string, f inputFlags) ([]item, error) {
	switch {
	case len(positional) > 1:
		return nil, usageErr("нужна одна ссылка, а передано %d аргументов: для нескольких используйте --input", len(positional))
	case len(positional) == 1 && f.file != "":
		return nil, usageErr("либо ссылка, либо --input, не оба")
	case len(positional) == 0 && f.file == "":
		return nil, usageErr("нужна ссылка или --input FILE")
	}

	var items []item
	if len(positional) == 1 {
		items = []item{{Link: positional[0]}}
	} else {
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
		if isJSONL(f.file, data) {
			items, err = parseJSONL(net, data, f)
		} else {
			items, err = parseCSV(net, f.file, data, f)
		}
		if err != nil {
			return nil, err
		}
	}

	merged, skipped := mergeItems(items, f.people)
	if skipped > 0 {
		fmt.Fprintf(a.stderr, "пропущено строк без ссылки: %d\n", skipped)
	}
	if len(merged) == 0 {
		return nil, usageErr("во входе нет ни одной ссылки")
	}
	return merged, nil
}

func mergeItems(items []item, extraPeople []string) ([]item, int) {
	index := map[string]int{}
	var out []item
	skipped := 0
	for _, it := range items {
		link := strings.TrimSpace(it.Link)
		if link == "" {
			skipped++
			continue
		}
		i, seen := index[link]
		if !seen {
			i = len(out)
			index[link] = i
			out = append(out, item{Link: link})
		}
		out[i].People = appendUnique(out[i].People, it.People...)
		out[i].People = appendUnique(out[i].People, extraPeople...)
	}
	return out, skipped
}

func appendUnique(list []string, values ...string) []string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		dup := false
		for _, have := range list {
			if strings.EqualFold(have, v) {
				dup = true
				break
			}
		}
		if !dup {
			list = append(list, v)
		}
	}
	return list
}

func isJSONL(name string, data []byte) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jsonl", ".ndjson", ".json":
		return true
	case ".csv", ".tsv", ".txt":
		return false
	}
	trimmed := bytes.TrimLeft(bytes.TrimPrefix(data, []byte("\uFEFF")), " \t\r\n")
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '"')
}

// findColumn picks the column named explicitly, or the best-known name.
func findColumn(headers []string, explicit string, known []string) (int, bool) {
	normalized := make([]string, len(headers))
	for i, h := range headers {
		normalized[i] = normalizeHeader(h)
	}
	if explicit != "" {
		want := normalizeHeader(explicit)
		for i, h := range normalized {
			if h == want {
				return i, true
			}
		}
		return -1, false
	}
	for _, name := range known {
		for i, h := range normalized {
			if h == name {
				return i, true
			}
		}
	}
	return -1, false
}

func parseCSV(net network, name string, data []byte, f inputFlags) ([]item, error) {
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
	peopleCol := -1
	if f.peopleColumn != "" {
		var found bool
		if peopleCol, found = findColumn(header, f.peopleColumn, nil); !found {
			return nil, usageErr("в CSV нет колонки %q; есть: %s", f.peopleColumn, strings.Join(header, ", "))
		}
	} else {
		peopleCol, _ = findColumn(header, "", peopleColumns)
	}
	linkCol, ok := findColumn(header, f.column, linkColumns[net])
	data0 := 1
	if !ok {
		if f.column != "" {
			return nil, usageErr("в CSV нет колонки %q; есть: %s", f.column, strings.Join(header, ", "))
		}
		// No header: the first row is data if one of its cells is a link.
		for i, cell := range header {
			if looksLikeLink(net, cell) {
				linkCol, ok, data0, peopleCol = i, true, 0, -1
				break
			}
		}
		if !ok {
			return nil, usageErr("не найдена колонка со ссылкой; укажите --column, есть: %s", strings.Join(header, ", "))
		}
	}

	var items []item
	for _, row := range rows[data0:] {
		var it item
		if linkCol < len(row) {
			it.Link = row[linkCol]
		}
		if peopleCol >= 0 && peopleCol < len(row) {
			it.People = splitPeople(row[peopleCol])
		}
		items = append(items, it)
	}
	return items, nil
}

// delimiter is the separator of the first line: a comma, or the semicolon or
// tab that spreadsheet exports in some locales use instead.
func delimiter(name string, data []byte) rune {
	if strings.EqualFold(filepath.Ext(name), ".tsv") {
		return '\t'
	}
	first, _, _ := bytes.Cut(data, []byte("\n"))
	best, count := ',', bytes.Count(first, []byte(","))
	for _, d := range []rune{';', '\t'} {
		if c := bytes.Count(first, []byte(string(d))); c > count {
			best, count = d, c
		}
	}
	return best
}

func parseJSONL(net network, data []byte, f inputFlags) ([]item, error) {
	var items []item
	scanner := bufio.NewScanner(bytes.NewReader(bytes.TrimPrefix(data, []byte("\uFEFF"))))
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)
	line := 0
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var value any
		if err := json.Unmarshal(text, &value); err != nil {
			return nil, usageErr("JSONL, строка %d: %v", line, err)
		}
		switch v := value.(type) {
		case string:
			items = append(items, item{Link: v})
		case map[string]any:
			it, err := jsonItem(net, v, f)
			if err != nil {
				return nil, usageErr("JSONL, строка %d: %v", line, err)
			}
			items = append(items, it)
		default:
			return nil, usageErr("JSONL, строка %d: нужен объект или строка", line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, usageErr("JSONL не читается: %v", err)
	}
	return items, nil
}

func jsonItem(net network, obj map[string]any, f inputFlags) (item, error) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	var it item
	if i, ok := findColumn(keys, f.column, linkColumns[net]); ok {
		s, isString := obj[keys[i]].(string)
		if !isString && obj[keys[i]] != nil {
			return item{}, fmt.Errorf("поле %q — не строка", keys[i])
		}
		it.Link = s
	} else if f.column != "" {
		return item{}, fmt.Errorf("нет поля %q", f.column)
	} else {
		return item{}, fmt.Errorf("нет поля со ссылкой; укажите --column")
	}

	explicit := f.peopleColumn
	known := peopleColumns
	if explicit != "" {
		known = nil
	}
	if i, ok := findColumn(keys, explicit, known); ok {
		switch v := obj[keys[i]].(type) {
		case string:
			it.People = splitPeople(v)
		case []any:
			for _, p := range v {
				if s, ok := p.(string); ok {
					it.People = append(it.People, s)
				}
			}
		}
	}
	return it, nil
}
