package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readItems(t *testing.T, net network, positional []string, f inputFlags) []item {
	t.Helper()
	a := &app{stdin: strings.NewReader(""), stderr: &bytes.Buffer{}}
	items, err := a.readInputs(net, positional, f)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestASingleLinkWithPeople(t *testing.T) {
	got := readItems(t, instagramNet, []string{" instagram.com/acme "}, inputFlags{people: stringList{"Иванов Иван", "Петров Пётр"}})
	want := []item{{Link: "instagram.com/acme", People: []string{"Иванов Иван", "Петров Пётр"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %+v", got)
	}
}

func TestCSVFindsTheLinkColumnByName(t *testing.T) {
	path := writeFile(t, "leads.csv", "\uFEFFCompany,Telegram,Site\n"+
		"Acme,https://t.me/acme,acme.ru\n"+
		"Empty,,empty.ru\n"+
		"Acme again,https://t.me/acme,acme.ru\n"+
		"Beta,@beta_group,beta.ru\n")
	got := readItems(t, telegramNet, nil, inputFlags{file: path})
	want := []item{{Link: "https://t.me/acme"}, {Link: "@beta_group"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %+v", got)
	}
}

func TestCSVWithSemicolonsAndAPeopleColumn(t *testing.T) {
	path := writeFile(t, "leads.csv", "name;instagram;ФИО\n"+
		"Acme;instagram.com/acme;Иванов Иван, Петров Пётр\n"+
		"Acme;instagram.com/acme;Сидоров Сидор; иванов иван\n")
	got := readItems(t, instagramNet, nil, inputFlags{file: path})
	want := []item{{Link: "instagram.com/acme", People: []string{"Иванов Иван", "Петров Пётр", "Сидоров Сидор"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %+v", got)
	}
}

func TestCSVWithoutAHeader(t *testing.T) {
	path := writeFile(t, "links.csv", "vk.com/acme\nvk.com/beta\n")
	got := readItems(t, vkNet, nil, inputFlags{file: path, people: stringList{"Иванов Иван"}})
	want := []item{
		{Link: "vk.com/acme", People: []string{"Иванов Иван"}},
		{Link: "vk.com/beta", People: []string{"Иванов Иван"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %+v", got)
	}
}

func TestCSVExplicitColumns(t *testing.T) {
	path := writeFile(t, "leads.tsv", "Где\tКто\nvk.com/acme\tИванов Иван\n")
	got := readItems(t, vkNet, nil, inputFlags{file: path, column: "где", peopleColumn: "Кто"})
	want := []item{{Link: "vk.com/acme", People: []string{"Иванов Иван"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %+v", got)
	}
}

func TestJSONL(t *testing.T) {
	path := writeFile(t, "leads.jsonl", `{"id": 1, "vk_url": "vk.com/acme", "people": ["Иванов Иван", "Петров Пётр"]}`+"\n\n"+
		`"vk.com/beta"`+"\n"+
		`{"url": "vk.com/gamma", "names": "Сидоров Сидор; Козлов Иван"}`+"\n")
	got := readItems(t, vkNet, nil, inputFlags{file: path})
	want := []item{
		{Link: "vk.com/acme", People: []string{"Иванов Иван", "Петров Пётр"}},
		{Link: "vk.com/beta"},
		{Link: "vk.com/gamma", People: []string{"Сидоров Сидор", "Козлов Иван"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %+v", got)
	}
}

func TestJSONLFromStdinIsSniffed(t *testing.T) {
	a := &app{stdin: strings.NewReader(`{"channel":"@one_group"}` + "\n" + `{"channel":"@two_group"}`), stderr: &bytes.Buffer{}}
	got, err := a.readInputs(telegramNet, nil, inputFlags{file: "-"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Link != "@two_group" {
		t.Fatalf("items = %+v", got)
	}
}

func TestBadInputIsAUsageError(t *testing.T) {
	cases := map[string]struct {
		positional []string
		flags      inputFlags
	}{
		"nothing":            {nil, inputFlags{}},
		"two links":          {[]string{"@a_group", "@b_group"}, inputFlags{}},
		"link and file":      {[]string{"@a_group"}, inputFlags{file: "x.csv"}},
		"missing file":       {nil, inputFlags{file: filepath.Join(t.TempDir(), "none.csv")}},
		"no link column":     {nil, inputFlags{file: writeFile(t, "a.csv", "company,site\nAcme,acme.ru\n")}},
		"unknown column":     {nil, inputFlags{file: writeFile(t, "b.csv", "telegram\n@a_group\n"), column: "tg_link"}},
		"broken JSONL":       {nil, inputFlags{file: writeFile(t, "c.jsonl", `{"telegram":"@a_group"}`+"\n{oops\n")}},
		"JSONL without link": {nil, inputFlags{file: writeFile(t, "d.jsonl", `{"company":"Acme"}`+"\n")}},
		"only empty links":   {nil, inputFlags{file: writeFile(t, "e.csv", "telegram,company\n,Acme\n")}},
	}
	for name, c := range cases {
		a := &app{stdin: strings.NewReader(""), stderr: &bytes.Buffer{}}
		if _, err := a.readInputs(telegramNet, c.positional, c.flags); !errors.Is(err, errUsage) {
			t.Errorf("%s: err = %v, want a usage error", name, err)
		}
	}
}
