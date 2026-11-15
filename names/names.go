// Package names turns a Russian personal name into the forms it might appear
// under elsewhere, and decides whether two of them are the same person.
//
// This is the quiet centre of the social collectors. A decision-maker arrives
// from the company registry as «ИВАНОВ ИВАН ИВАНОВИЧ» and appears on Instagram
// as ivan.ivanov, ivanivanov, or «Иван Иванов» — and there is no transliteration
// standard anyone follows, so every plausible spelling has to be generated and
// tried. Getting this wrong is expensive in both directions: too strict and a
// real match is missed, too loose and a stranger is reported as the company's
// owner.
package names

import (
	"regexp"
	"strings"
)

// charVariants is how each Cyrillic letter is written in Latin. Several letters
// have more than one common spelling, and which one a person picked is not
// predictable — г is both g and h, х is kh and h — so both are generated.
var charVariants = map[rune][]string{
	'а': {"a"}, 'б': {"b"}, 'в': {"v"}, 'г': {"g", "h"}, 'д': {"d"},
	'е': {"e", "ye"}, 'ё': {"e", "yo"}, 'ж': {"zh"}, 'з': {"z"}, 'и': {"i"},
	'й': {"y", "i"}, 'к': {"k"}, 'л': {"l"}, 'м': {"m"}, 'н': {"n"},
	'о': {"o"}, 'п': {"p"}, 'р': {"r"}, 'с': {"s"}, 'т': {"t"},
	'у': {"u"}, 'ф': {"f"}, 'х': {"kh", "h"}, 'ц': {"ts", "c"}, 'ч': {"ch"},
	'ш': {"sh"}, 'щ': {"shch", "sch"}, 'ъ': {""}, 'ы': {"y", "i"}, 'ь': {""},
	'э': {"e"}, 'ю': {"yu", "iu"}, 'я': {"ya", "ia"},
}

// firstNameAliases are the spellings people actually use for their own names,
// which transliteration alone does not produce: Александр signs himself
// "alexander" far more often than the "aleksandr" any rule would generate.
var firstNameAliases = map[string][]string{
	"александр":  {"alexandr", "aleksandr", "alexander"},
	"александра": {"alexandra", "aleksandra"},
	"алексей":    {"alexey", "aleksey", "alexei", "aleksei"},
	"алена":      {"alena", "alyona"},
	"андрей":     {"andrey", "andrei"},
	"анна":       {"anna"},
	"анастасия":  {"anastasia", "anastasiya", "anastasiia"},
	"антон":      {"anton"},
	"артем":      {"artem", "artyom"},
	"артём":      {"artem", "artyom"},
	"артур":      {"artur", "arthur"},
	"вадим":      {"vadim"},
	"валентин":   {"valentin"},
	"валентина":  {"valentina"},
	"валерий":    {"valeriy", "valery", "valerii"},
	"вера":       {"vera"},
	"виктор":     {"viktor", "victor"},
	"виктория":   {"viktoria", "victoria"},
	"виталий":    {"vitaliy", "vitaly", "vitalii"},
	"владимир":   {"vladimir"},
	"вячеслав":   {"vyacheslav", "viacheslav"},
	"георгий":    {"georgiy", "georgy", "george"},
	"дарья":      {"darya", "daria"},
	"денис":      {"denis"},
	"дмитрий":    {"dmitriy", "dmitry", "dmitrii", "dmitri"},
	"евгений":    {"evgeniy", "evgeny", "evgenii", "eugene"},
	"евгения":    {"evgeniya", "evgenia"},
	"екатерина":  {"ekaterina", "katerina"},
	"елена":      {"elena", "yelena"},
	"иван":       {"ivan"},
	"игорь":      {"igor", "igorj"},
	"илья":       {"ilya", "ilia"},
	"ирина":      {"irina"},
	"кирилл":     {"kirill", "cyrill"},
	"константин": {"konstantin", "constantine"},
	"кристина":   {"kristina", "christina"},
	"ксения":     {"kseniya", "ksenia", "xenia"},
	"максим":     {"maksim", "maxim"},
	"мария":      {"mariya", "maria"},
	"михаил":     {"mikhail", "mihail", "michael"},
	"наталья":    {"natalya", "natalia", "nataliya"},
	"никита":     {"nikita"},
	"николай":    {"nikolay", "nikolai"},
	"оксана":     {"oksana", "oxana"},
	"олег":       {"oleg"},
	"ольга":      {"olga"},
	"павел":      {"pavel", "paul"},
	"петр":       {"petr", "pyotr", "peter"},
	"пётр":       {"petr", "pyotr", "peter"},
	"роман":      {"roman"},
	"светлана":   {"svetlana"},
	"сергей":     {"sergey", "sergei"},
	"станислав":  {"stanislav"},
	"татьяна":    {"tatyana", "tatiana"},
	"юлия":       {"yuliya", "yulia", "iulia", "julia"},
	"юрий":       {"yuriy", "yury", "yuri"},
}

var (
	nameTokenRe  = regexp.MustCompile(`[A-Za-zА-Яа-яЁё-]+`)
	wordRe       = regexp.MustCompile(`[а-яёa-z]+`)
	cleanTokenRe = regexp.MustCompile(`[^а-яa-z-]+`)
	latinOnlyRe  = regexp.MustCompile(`^[a-z-]+$`)
	alnumRe      = regexp.MustCompile(`[^a-z0-9]+`)
	normalizeRe  = regexp.MustCompile(`[^0-9a-zа-я._@]+`)
	spacesRe     = regexp.MustCompile(`\s+`)
)

// Person is a name split into the parts that matter.
type Person struct {
	Raw   string
	First string
	Last  string
}

// Split reads a full name. The registry writes surname first — «ИВАНОВ ИВАН
// ИВАНОВИЧ» — and that is where these names come from, so that is the default;
// a name typed by a person usually is not.
func Split(person string, surnameFirst bool) (Person, bool) {
	tokens := nameTokenRe.FindAllString(person, -1)
	if len(tokens) < 2 {
		return Person{}, false
	}
	p := Person{Raw: strings.TrimSpace(person)}
	if surnameFirst {
		p.Last, p.First = tokens[0], tokens[1]
	} else {
		p.First, p.Last = tokens[0], tokens[1]
	}
	return p, true
}

// CleanToken strips a name down to letters.
func CleanToken(token string) string {
	token = strings.ToLower(strings.TrimSpace(token))
	token = strings.ReplaceAll(token, "ё", "е")
	return cleanTokenRe.ReplaceAllString(token, "")
}

// Transliterate spells a Cyrillic token in Latin, every plausible way. A token
// already in Latin is returned as-is: it is presumably how its owner writes it.
func Transliterate(token string, maxVariants int) []string {
	token = CleanToken(token)
	if token == "" {
		return nil
	}
	if latinOnlyRe.MatchString(token) {
		return []string{strings.ReplaceAll(token, "-", "")}
	}
	if maxVariants <= 0 {
		maxVariants = 8
	}

	variants := []string{""}
	for _, ch := range token {
		options := []string{""}
		if ch != '-' {
			if mapped, ok := charVariants[ch]; ok {
				options = mapped
			}
		}
		next := make([]string, 0, len(variants)*len(options))
		for _, prefix := range variants {
			for _, suffix := range options {
				next = append(next, prefix+suffix)
			}
		}
		// The combinations multiply out fast, and the ones past this point are
		// spellings nobody uses.
		if len(next) > maxVariants*4 {
			next = next[:maxVariants*4]
		}
		variants = next
	}
	return dedupeLower(variants, maxVariants)
}

// Variants is every Latin spelling worth trying for one name token.
func Variants(token string, isFirstName bool, maxVariants int) []string {
	cleaned := CleanToken(token)
	var out []string
	if isFirstName {
		out = append(out, firstNameAliases[cleaned]...)
	}
	out = append(out, Transliterate(cleaned, maxVariants)...)
	return dedupeLower(out, maxVariants)
}

func dedupeLower(items []string, limit int) []string {
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		item = alnumRe.ReplaceAllString(strings.ToLower(item), "")
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// SearchQueries is what to type into a search box to find one person, most
// promising first: the name as written, the surname alone, then transliterated
// combinations — because a search that matches nothing costs a round trip, and
// there are a lot of them.
func SearchQueries(person string, surnameFirst bool, maxVariants int) []string {
	parsed, ok := Split(person, surnameFirst)
	if !ok {
		return nil
	}
	firstVariants := Variants(parsed.First, true, maxVariants)
	lastVariants := Variants(parsed.Last, false, maxVariants)

	var queries []string
	add := func(value string) {
		value = spacesRe.ReplaceAllString(strings.TrimSpace(value), " ")
		if value == "" {
			return
		}
		for _, existing := range queries {
			if existing == value {
				return
			}
		}
		queries = append(queries, value)
	}

	add(parsed.Raw)
	add(parsed.Last)
	for _, last := range lastVariants {
		add(last)
	}
	for _, first := range head(firstVariants, 4) {
		for _, last := range head(lastVariants, 4) {
			add(first + " " + last)
		}
	}
	for _, first := range head(firstVariants, 4) {
		for _, last := range head(lastVariants, 4) {
			add(first + last)
		}
	}
	if cf, cl := CleanToken(parsed.First), CleanToken(parsed.Last); cf != "" && cl != "" {
		add(alnumRe.ReplaceAllString(strings.ToLower(cf+cl), ""))
	}
	return queries
}

func head[T any](items []T, n int) []T {
	if len(items) < n {
		return items
	}
	return items[:n]
}

// Normalize folds text to the form matching compares: lowercase, ё as е, and
// punctuation reduced to spaces.
func Normalize(value string) string {
	text := strings.ToLower(strings.NewReplacer("ё", "е", "Ё", "Е").Replace(value))
	text = normalizeRe.ReplaceAllString(text, " ")
	return strings.TrimSpace(spacesRe.ReplaceAllString(text, " "))
}

// MatchScore rates how well a found profile matches the name being searched for,
// 0 meaning not at all. The ladder matters more than the numbers: an exact
// username or full name is certain, a full name inside the row text is nearly
// so, and a single shared token is weak enough that it only survives as a hint.
func MatchScore(targetName, candidateText, username, fullName string) int {
	target := Normalize(targetName)
	if target == "" {
		return 0
	}
	usernameNorm := Normalize(username)
	fullNameNorm := Normalize(fullName)
	payload := strings.Join(nonEmpty(usernameNorm, fullNameNorm, Normalize(candidateText)), " ")

	if target == usernameNorm || target == fullNameNorm {
		return 100
	}
	if strings.Contains(payload, target) {
		return 90
	}

	tokens := NameTokens(targetName)
	if len(tokens) >= 2 {
		all := true
		for _, token := range tokens {
			if !strings.Contains(payload, token) {
				all = false
				break
			}
		}
		if all {
			return 80
		}
	}
	if len(tokens) == 1 {
		if tokens[0] == usernameNorm || tokens[0] == fullNameNorm {
			return 70
		}
		if strings.Contains(payload, tokens[0]) {
			return 55
		}
	}
	return 0
}

// NameTokens are the words of a name worth matching on. Conjunctions and
// articles are dropped: they match everything and mean nothing.
func NameTokens(value string) []string {
	var out []string
	for _, token := range strings.Split(Normalize(value), " ") {
		if token == "" || token == "и" || token == "the" {
			continue
		}
		out = append(out, token)
	}
	return out
}

// Confidence grades a match for a person, which is what decides whether the
// result is shown as a finding or as a maybe.
//
// Both name parts present is certain; the two run together inside a username
// (ivanivanov) equally so. The surname alone is worth reporting — surnames are
// distinctive — and anything less is a guess.
func Confidence(person string, surnameFirst bool, maxVariants int, username, fullName, matchedText string) string {
	parsed, ok := Split(person, surnameFirst)
	if !ok {
		return ""
	}
	firstVariants := append([]string{parsed.First}, Variants(parsed.First, true, maxVariants)...)
	lastVariants := append([]string{parsed.Last}, Variants(parsed.Last, false, maxVariants)...)
	for i := range firstVariants {
		firstVariants[i] = Normalize(firstVariants[i])
	}
	for i := range lastVariants {
		lastVariants[i] = Normalize(lastVariants[i])
	}

	payload := Normalize(strings.Join([]string{username, fullName, matchedText}, " "))
	usernameNorm := Normalize(username)

	hasLast := containsAny(payload, lastVariants)
	// A great many Russian surnames contain a first name whole — Иванов holds
	// Иван, Петров holds Пётр, Сергеев holds Сергей. Looking for the first name
	// in the raw text would then find it inside the surname and call a
	// surname-only match certain, which is how a stranger who merely shares a
	// surname ends up presented as the company's owner. So the surname is
	// removed before asking whether the first name is also there.
	hasFirst := containsAny(withoutAny(payload, lastVariants), firstVariants)
	hasCompact := false
	for _, first := range firstVariants {
		for _, last := range lastVariants {
			if first != "" && last != "" && strings.Contains(usernameNorm, first+last) {
				hasCompact = true
			}
		}
	}

	switch {
	case hasFirst && hasLast, hasCompact:
		return "high"
	case hasLast:
		return "medium"
	}
	return "low"
}

// withoutAny removes every occurrence of the needles from the text.
func withoutAny(payload string, needles []string) string {
	for _, needle := range needles {
		if needle != "" {
			payload = strings.ReplaceAll(payload, needle, " ")
		}
	}
	return payload
}

func containsAny(payload string, needles []string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(payload, needle) {
			return true
		}
	}
	return false
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ---------- matching collected members against a company's people ----------

// roleKeywords suggest a decision-maker. Broad on purpose, and matched as
// substrings so «сооснователь» and "co-founder" hit too.
var roleKeywords = []string{
	"основатель", "сооснователь", "фаундер", "владелец", "совладелец",
	"директор", "руководитель", "собственник", "учредитель", "гендиректор",
	"founder", "co-founder", "cofounder", "owner", "ceo", "founding",
}

// RoleHint returns the role words found in a person's display name or bio.
// Names are scanned as well as bios because people put their title in the name
// («Лиза Руководитель отдела маркетинга»), which is often the only place it
// appears when bios were not collected.
func RoleHint(text string) string {
	haystack := strings.ToLower(text)
	if strings.TrimSpace(haystack) == "" {
		return ""
	}
	var hits []string
	seen := map[string]bool{}
	for _, keyword := range roleKeywords {
		if strings.Contains(haystack, keyword) && !seen[keyword] {
			seen[keyword] = true
			hits = append(hits, keyword)
		}
	}
	return strings.Join(hits, ", ")
}

// Tokens are the words of a text long enough to be a name rather than an
// initial or noise.
func Tokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, word := range wordRe.FindAllString(strings.ToLower(text), -1) {
		if len([]rune(word)) >= 3 {
			out[word] = true
		}
	}
	return out
}

// OrderedTokens is Tokens in the order they appear, which is what makes the
// first one the surname in a registry-formatted name.
func OrderedTokens(text string) []string {
	var out []string
	for _, word := range wordRe.FindAllString(strings.ToLower(text), -1) {
		if len([]rune(word)) >= 3 {
			out = append(out, word)
		}
	}
	return out
}

// BestMatch picks which of a company's people a collected member looks like,
// and how strongly.
//
// A match needs either two shared name tokens (surname and given name) or the
// surname alone, which is what keeps every «Сергей» in a chat from being read as
// the company's Sergey.
func BestMatch(memberTokens map[string]bool, people []string) (string, int) {
	bestName, bestScore := "", 0
	for _, person := range people {
		personTokens := Tokens(person)
		if len(personTokens) == 0 {
			continue
		}
		ordered := OrderedTokens(person)
		surname := ""
		if len(ordered) > 0 {
			surname = ordered[0]
		}

		score := 0
		for token := range personTokens {
			if memberTokens[token] {
				score++
			}
		}
		strong := score >= 2 || (len([]rune(surname)) >= 4 && memberTokens[surname])
		if strong && score > bestScore {
			bestName, bestScore = strings.TrimSpace(person), score
		}
	}
	return bestName, bestScore
}
