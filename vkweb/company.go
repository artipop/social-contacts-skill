package vkweb

import (
	"regexp"
	"strings"
	"unicode"
)

// legalForms are the words of a registered company name that say what kind of
// legal entity it is. Nobody writes them when naming their workplace.
var legalForms = wordSet(`
	ооо ао пао зао оао нао ип ано нко чу чуз нуз ну фгбу фгбоу гбу гбуз гауз гуп муп фгуп мбу
	общество с ограниченной ответственностью акционерное публичное закрытое открытое непубличное
	индивидуальный предприниматель автономная некоммерческая организация частное учреждение
	государственное бюджетное федеральное муниципальное унитарное предприятие
	здравоохранения города г
	llc ltd inc co corp gmbh jsc
	группа компаний гк
`)

// genericWords say what a company does, not which one it is: «клиника»,
// «медицинский центр», «стоматология». They are left out when comparing, so
// «КЛИНИКА ИННОВАТИКА» matches a profile that says just «Innovatica». A name
// made of nothing else («Семейный доктор») is compared whole.
var genericWords = wordSet(`
	клиника клиники клиник клинику клиникой клинический клиническая клиническое
	медицинский медицинская медицинское медицинские медицинского медицинской медицина медицины мед
	центр центра центре центры
	стоматология стоматологии стоматологическая стоматологический стоматологическое стоматологической
	поликлиника поликлиники больница больницы госпиталь
	лечебно лечебный лечебная диагностический диагностическая диагностики диагностика
	многопрофильный многопрофильная лаборатория лаборатории
	салон студия кабинет офис компания фирма
	clinic clinics clinica klinika medical med center centre dental stomatology
	и в на the and of
`)

func wordSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(text) {
		out[w] = true
	}
	return out
}

var wordRe = regexp.MustCompile(`[\p{L}\p{N}]+`)

// words splits text into lower-case words, ё read as е.
func words(text string) []string {
	text = strings.ToLower(strings.NewReplacer("ё", "е", "Ё", "е").Replace(text))
	return wordRe.FindAllString(text, -1)
}

// companyWords are the words of a company name that tell it apart: without
// its legal form, and without the generic words unless nothing else is left.
func companyWords(company string) []string {
	distinct, _ := splitCompany(company)
	return distinct
}

// splitCompany reads a company name into the words that tell it apart and
// all its words but the legal form.
func splitCompany(company string) (distinct, named []string) {
	for _, w := range words(company) {
		if legalForms[w] {
			continue
		}
		named = append(named, w)
		if !genericWords[w] {
			distinct = append(distinct, w)
		}
	}
	if len(distinct) == 0 {
		return named, named
	}
	return distinct, named
}

// CompanyQuery is what to type into the search's place-of-work filter for a
// company: the words that tell it apart, as people write them — «ООО
// "КЛИНИКА ИННОВАТИКА"» becomes «Инноватика».
func CompanyQuery(company string) string {
	ws := companyWords(company)
	for i, w := range ws {
		ws[i] = titleCase(w)
	}
	return strings.Join(ws, " ")
}

// CompanyMatches reports whether a result's snippet names the company: every
// word that tells the company apart appears in it, in Cyrillic or Latin
// letters and give or take a case ending.
func CompanyMatches(company, snippet string) bool {
	want, named := splitCompany(company)
	if len(want) == 0 {
		return false
	}
	var have []string
	for _, w := range words(snippet) {
		have = append(have, skeleton(w))
	}
	// A name written as one word in the snippet («MedSwiss» for «Мед Свисс»).
	if len(named) > 1 {
		joined := ""
		for _, w := range named {
			joined += skeleton(w)
		}
		for _, h := range have {
			if sameWord(joined, h) {
				return true
			}
		}
	}
	for _, w := range want {
		k := skeleton(w)
		found := false
		for _, h := range have {
			if sameWord(k, h) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// sameWord compares two skeletons, allowing a short case ending on either
// side of a long word («Инноватика», «Инноватике»).
func sameWord(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 6 && strings.HasPrefix(b, a[:len(a)-1]) && len(b)-len(a) <= 2
}

// cyrillicLatin spells each Cyrillic letter in Latin, one way: skeleton then
// folds the Latin spellings that differ only in convention.
var cyrillicLatin = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ж': "zh", 'з': "z",
	'и': "i", 'й': "i", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p",
	'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "h", 'ц': "ts", 'ч': "ch",
	'ш': "sh", 'щ': "sh", 'ъ': "", 'ы': "i", 'ь': "", 'э': "e", 'ю': "iu", 'я': "ia",
}

var latinFolds = strings.NewReplacer(
	"shch", "sh", "sch", "sh", "kh", "h", "ph", "f", "ck", "k", "qu", "kv",
	"ce", "tse", "ci", "tsi", "cy", "tsi", "ch", "ch", "c", "k", "q", "k", "x", "ks",
	"w", "v", "y", "i", "j", "i",
)

// skeleton is a word reduced to how it sounds in Latin letters, so that
// «Инноватика», "INNOVATICA" and "innovatika" are the same word: Cyrillic is
// transliterated, c/k, y/i, w/v and the like are folded, doubled letters
// collapsed.
func skeleton(word string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(word) {
		if latin, ok := cyrillicLatin[r]; ok {
			b.WriteString(latin)
		} else if r < unicode.MaxASCII {
			b.WriteRune(r)
		}
	}
	folded := latinFolds.Replace(b.String())
	var out []byte
	for i := 0; i < len(folded); i++ {
		if i > 0 && folded[i] == folded[i-1] {
			continue
		}
		out = append(out, folded[i])
	}
	return string(out)
}
