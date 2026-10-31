// Package business (TaskKeys) is Task Keys (design/projects-vs-asana-jira/
// PROGRAM.md §4.2 #94, D-PJ-7; projects A8K): every project has a key, two to
// five capitals and digits that start with a letter ("Q4", "WEB"), unique in
// the workspace, and every task in it a number, so the task reads as "Q4-12"
// on every row, card, panel, link, search and API answer.
//
//   - A key comes from the project's name when the project is made (Derive)
//     and can be changed by the project's admins. The key it had stays an
//     alias, so "Q4-12" in an old message still finds the task.
//   - Numbers are given in order as tasks are made, in the same statement
//     that makes the task's row (models/postgres/TaskKey InsertTask), and are
//     never reused. A task made before its project had a key is numbered by
//     Settle, or by the projects.keys sweep.
//   - The key lives in Postgres and is laid over a task read for the app
//     (Lay), as custom field values are. It is never stored in the graph.
//
// Who may see a task is never this package's question: Resolve finds the
// task a key names, and its caller checks the reader may open it, so a key
// for a task you can't open answers exactly as one that names nothing.
package business

import (
	"errors"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// The lengths a key may have.
const (
	MinKeyLen = 2
	MaxKeyLen = 5
)

// ErrBadKey is a key that isn't two to five capitals and digits starting with
// a letter. Its message says so in words.
var ErrBadKey = errors.New("a project key is 2 to 5 letters and digits, starting with a letter")

// Valid says whether key, as typed, can be a project's key, and answers it
// in capitals.
func Valid(key string) (string, error) {
	k := strings.ToUpper(strings.TrimSpace(key))
	if len(k) < MinKeyLen || len(k) > MaxKeyLen || !isKeyStart(rune(k[0])) {
		return "", ErrBadKey
	}
	for _, r := range k {
		if !isKeyChar(r) {
			return "", ErrBadKey
		}
	}
	return k, nil
}

func isKeyStart(r rune) bool { return r >= 'A' && r <= 'Z' }
func isKeyChar(r rune) bool  { return isKeyStart(r) || (r >= '0' && r <= '9') }

// Parse reads a task's key as anyone might type it ("Q4-12", "q4-12", " Q4-12
// "): its project's key in capitals and its number. ok is false for anything
// else, a uuid included.
func Parse(raw string) (projectKey string, number int, ok bool) {
	return parseKey(raw, func(k string) (string, bool) {
		v, err := Valid(k)
		return v, err == nil && v == strings.ToUpper(k)
	})
}

// ParseSource reads a key a task had in the source it was imported from, as
// anyone might type it ("ENG-7", "engine-7"): any prefix SourceKey takes, which
// includes every project's key, and its number.
func ParseSource(raw string) (sourceKey string, number int, ok bool) {
	return parseKey(raw, SourceKey)
}

// parseKey reads raw as a prefix that valid takes, a dash and a number.
func parseKey(raw string, valid func(string) (string, bool)) (string, int, bool) {
	s := strings.TrimSpace(raw)
	dash := strings.LastIndexByte(s, '-')
	if dash <= 0 || dash == len(s)-1 {
		return "", 0, false
	}
	k, ok := valid(s[:dash])
	if !ok {
		return "", 0, false
	}
	digits := s[dash+1:]
	if len(digits) > 9 || digits[0] == '0' {
		return "", 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 {
		return "", 0, false
	}
	return k, n, true
}

// Words that initials skip ("The launch plan" is LP, not TLP).
var minorWords = map[string]bool{"THE": true, "A": true, "AN": true, "OF": true, "AND": true, "FOR": true, "TO": true, "IN": true, "ON": true, "AT": true, "BY": true, "WITH": true}

// Derive is the keys a project named name could take, best first: a short
// first word that reads as a code ("Q4 launch" is Q4, "API v2" is API), the
// initials of its words ("Customer onboarding" is CO), the start of its
// first word ("Website" is WEB), longer starts, then any of those with a
// number after it, so a workspace full of similar names still finds one. A
// name with no Latin letters (日本語) is PRJ and its numbered forms. Every
// candidate is a valid key and none repeats.
func Derive(name string) []string {
	words := latinWords(name)
	var out []string
	seen := map[string]bool{}
	add := func(k string) {
		if k, err := Valid(k); err == nil && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	if len(words) > 0 {
		if first := words[0]; len(first.ascii) >= MinKeyLen && len(first.ascii) <= 4 && (first.hasDigit || first.allCaps) {
			add(first.ascii)
		}
		// The words a key can start from: those that start with a letter
		// ("2026 roadmap" starts from ROADMAP).
		var lettered []word
		for _, w := range words {
			if isKeyStart(rune(w.ascii[0])) {
				lettered = append(lettered, w)
			}
		}
		var initials strings.Builder
		for _, w := range lettered {
			if minorWords[w.ascii] && len(lettered) > 2 {
				continue
			}
			if initials.Len() < 4 {
				initials.WriteByte(w.ascii[0])
			}
		}
		if initials.Len() >= MinKeyLen {
			add(initials.String())
		}
		if len(lettered) > 0 {
			lead := lettered[0].ascii
			for _, n := range []int{3, 2, 4} {
				if len(lead) >= n {
					add(lead[:n])
				}
			}
			if len(lettered) > 1 && len(lead) >= 2 {
				add(lead[:2] + lettered[1].ascii[:1])
			}
		}
	}
	if len(out) == 0 {
		add("PRJ")
	}
	base := append([]string(nil), out...)
	for n := 2; n <= 99; n++ {
		suffix := strconv.Itoa(n)
		for _, b := range base {
			if room := MaxKeyLen - len(suffix); len(b) > room {
				b = b[:room]
			}
			add(b + suffix)
		}
	}
	return out
}

type word struct {
	ascii    string // its Latin letters and digits, in capitals, accents gone
	hasDigit bool
	allCaps  bool // written all in capitals, as a code is ("API")
}

// latinWords is name's words that keep at least one letter or digit once
// accents are taken off and only Latin letters and digits are left.
func latinWords(name string) []word {
	plain, _, err := transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), name)
	if err != nil {
		plain = name
	}
	var out []word
	for _, raw := range strings.FieldsFunc(plain, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		var b strings.Builder
		w := word{allCaps: true}
		letters := 0
		for _, r := range raw {
			switch {
			case r >= 'a' && r <= 'z':
				w.allCaps = false
				b.WriteRune(unicode.ToUpper(r))
				letters++
			case r >= 'A' && r <= 'Z':
				b.WriteRune(r)
				letters++
			case r >= '0' && r <= '9':
				w.hasDigit = true
				b.WriteRune(r)
			}
		}
		w.ascii = b.String()
		if w.ascii == "" {
			continue
		}
		if letters == 0 {
			w.allCaps = false
		}
		out = append(out, w)
	}
	return out
}

// MaxSourceKeyLen is the longest key prefix a task keeps from its source
// (task_key_aliases.key). Linear publishes no limit for its team keys; ten
// covers any a team would type before each issue.
const MaxSourceKeyLen = 10

// SourceKey is a key prefix as another tool writes it (Linear's ENG), in
// capitals: 1 to MaxSourceKeyLen capitals and digits, starting with a letter.
// A project's own key is always one. ok is false for anything else.
func SourceKey(raw string) (string, bool) {
	k := strings.ToUpper(raw)
	if k == "" || len(k) > MaxSourceKeyLen || !isKeyStart(rune(k[0])) {
		return "", false
	}
	for _, r := range k {
		if !isKeyChar(r) {
			return "", false
		}
	}
	return k, true
}
