package names

import (
	"slices"
	"strings"
	"testing"
)

func TestSplitReadsRegistryOrder(t *testing.T) {
	// Names arrive from the company registry as SURNAME NAME PATRONYMIC.
	got, ok := Split("ИВАНОВ ИВАН ИВАНОВИЧ", true)
	if !ok || got.Last != "ИВАНОВ" || got.First != "ИВАН" {
		t.Fatalf("разбор = %+v, ok=%v", got, ok)
	}
	// A name a person typed usually is not in that order.
	got, ok = Split("Иван Иванов", false)
	if !ok || got.First != "Иван" || got.Last != "Иванов" {
		t.Fatalf("разбор = %+v, ok=%v", got, ok)
	}
	if _, ok := Split("Иванов", true); ok {
		t.Fatal("одно слово разобралось как имя и фамилия")
	}
}

func TestTransliterationCoversTheSpellingsPeopleUse(t *testing.T) {
	// There is no standard anyone follows, so the ambiguous letters have to
	// produce both readings or a real profile is missed.
	got := Transliterate("щукин", 12)
	for _, want := range []string{"shchukin", "schukin"} {
		if !slices.Contains(got, want) {
			t.Errorf("для «щукин» нет варианта %q: %v", want, got)
		}
	}
	got = Transliterate("хохлов", 12)
	if !slices.Contains(got, "khokhlov") || !slices.Contains(got, "hohlov") {
		t.Errorf("для «хохлов» не хватает вариантов х: %v", got)
	}
	// A name already written in Latin is how its owner writes it.
	if got := Transliterate("Ivanov", 8); len(got) != 1 || got[0] != "ivanov" {
		t.Errorf("латиница переписалась: %v", got)
	}
}

func TestFirstNameAliasesBeatPlainTransliteration(t *testing.T) {
	// Александр signs himself "alexander" far more often than the "aleksandr"
	// any transliteration rule produces, and the alias has to come first.
	got := Variants("Александр", true, 8)
	if len(got) == 0 || got[0] != "alexandr" {
		t.Fatalf("варианты имени = %v", got)
	}
	if !slices.Contains(got, "alexander") {
		t.Errorf("нет живого написания alexander: %v", got)
	}
	// A surname gets no aliases — there is no such table for them.
	if slices.Contains(Variants("Александров", false, 8), "alexander") {
		t.Error("к фамилии применились псевдонимы имени")
	}
}

func TestSearchQueriesLeadWithTheMostLikely(t *testing.T) {
	queries := SearchQueries("ИВАНОВ ИВАН ИВАНОВИЧ", true, 8)
	if len(queries) < 5 {
		t.Fatalf("слишком мало запросов: %v", queries)
	}
	// The name as written, then the surname alone: a search that matches
	// nothing still costs a round trip, and there are many of them.
	if queries[0] != "ИВАНОВ ИВАН ИВАНОВИЧ" || queries[1] != "ИВАНОВ" {
		t.Fatalf("порядок запросов = %v", queries[:2])
	}
	joined := strings.Join(queries, "|")
	for _, want := range []string{"ivanov", "ivan ivanov", "ivanivanov"} {
		if !strings.Contains(joined, want) {
			t.Errorf("нет запроса %q среди %v", want, queries)
		}
	}
	// Duplicates would double the work for nothing.
	seen := map[string]bool{}
	for _, q := range queries {
		if seen[q] {
			t.Errorf("запрос %q повторяется", q)
		}
		seen[q] = true
	}
}

func TestMatchScoreRanksCertaintyAboveCoincidence(t *testing.T) {
	// An exact username is certain.
	if got := MatchScore("ivanivanov", "", "ivanivanov", ""); got != 100 {
		t.Errorf("точный логин = %d", got)
	}
	// The full name inside the row is nearly so.
	if got := MatchScore("Иван Иванов", "Иван Иванов · Директор", "ivan", ""); got != 90 {
		t.Errorf("имя в тексте строки = %d", got)
	}
	// Both parts present, apart, is strong.
	if got := MatchScore("Иван Иванов", "iva nov", "ivan", "Иванов Ирина"); got != 80 {
		t.Errorf("оба токена = %d", got)
	}
	// A one-word target found in the row text scores as a contained name: the
	// score says how the text matched, and Confidence is what decides whether
	// that is enough to act on.
	if got := MatchScore("Сергей", "Сергей Петров", "petrov", ""); got != 90 {
		t.Errorf("один токен в тексте = %d", got)
	}
	// And a stranger is not a match at all.
	if got := MatchScore("Иван Иванов", "Мария Сидорова", "sidorova", "Мария Сидорова"); got != 0 {
		t.Errorf("посторонний профиль совпал: %d", got)
	}
}

func TestConfidenceNeedsBothNamePartsToBeSure(t *testing.T) {
	person := "ИВАНОВ ИВАН ИВАНОВИЧ"
	if got := Confidence(person, true, 8, "ivan_ivanov", "Иван Иванов", ""); got != "high" {
		t.Errorf("имя и фамилия вместе = %q", got)
	}
	// Run together in a username, which is how half of them are written.
	if got := Confidence(person, true, 8, "ivanivanov", "", ""); got != "high" {
		t.Errorf("слитный логин = %q", got)
	}
	// A surname alone is worth reporting, but it is not certainty — and the
	// surname contains the first name, which must not be read as finding both.
	// Иванов holds Иван, Петров holds Пётр, Сергеев holds Сергей: without this
	// nearly every Russian surname-only hit would claim to be a full match.
	if got := Confidence(person, true, 8, "ivanov_photo", "", ""); got != "medium" {
		t.Errorf("одна фамилия = %q", got)
	}
	if got := Confidence("ПЕТРОВ ПЁТР ПЕТРОВИЧ", true, 8, "petrov_dom", "", ""); got != "medium" {
		t.Errorf("одна фамилия Петров = %q", got)
	}
	// A first name alone is not.
	if got := Confidence(person, true, 8, "ivan2000", "", ""); got != "low" {
		t.Errorf("одно имя = %q", got)
	}
}

func TestBestMatchKeepsStrangersOut(t *testing.T) {
	people := []string{"ИВАНОВ ИВАН ИВАНОВИЧ", "ПЕТРОВА МАРИЯ СЕРГЕЕВНА"}

	// Surname plus given name: the person we were looking for.
	name, score := BestMatch(Tokens("Иван Иванов"), people)
	if name != "ИВАНОВ ИВАН ИВАНОВИЧ" || score < 2 {
		t.Errorf("совпадение = %q (%d)", name, score)
	}
	// A distinctive surname alone is enough.
	if name, _ := BestMatch(Tokens("Петрова"), people); name != "ПЕТРОВА МАРИЯ СЕРГЕЕВНА" {
		t.Errorf("по фамилии не нашлось: %q", name)
	}
	// A shared first name is not: every chat has an Иван, and reporting one of
	// them as the company's owner is worse than finding nobody.
	if name, _ := BestMatch(Tokens("Иван"), people); name != "" {
		t.Errorf("по одному имени нашёлся посторонний: %q", name)
	}
	if name, _ := BestMatch(Tokens("Мария"), people); name != "" {
		t.Errorf("по одному имени нашёлся посторонний: %q", name)
	}
}

func TestRoleHintReadsTitlesOutOfNamesAndBios(t *testing.T) {
	// People put their title in the display name, which is often the only place
	// it appears when bios were not collected.
	if got := RoleHint("Лиза Руководитель отдела маркетинга"); got != "руководитель" {
		t.Errorf("роль из имени = %q", got)
	}
	if got := RoleHint("Co-Founder & CEO"); !strings.Contains(got, "ceo") {
		t.Errorf("роль из био = %q", got)
	}
	if got := RoleHint("просто человек"); got != "" {
		t.Errorf("выдумалась роль: %q", got)
	}
}

func TestNormalizeFoldsWhatDiffersWithoutMeaning(t *testing.T) {
	if got := Normalize("Пётр  ИВАНОВ!"); got != "петр иванов" {
		t.Errorf("нормализация = %q", got)
	}
	// The characters a username is built from survive.
	if got := Normalize("ivan.ivanov_2000"); !strings.Contains(got, "ivan.ivanov") {
		t.Errorf("нормализация логина = %q", got)
	}
}
