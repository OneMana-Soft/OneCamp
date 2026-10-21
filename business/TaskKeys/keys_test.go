package business

import (
	"errors"
	"slices"
	"testing"
)

func TestAKeyIsTwoToFiveCapitalsAndDigitsStartingWithALetter(t *testing.T) {
	for typed, want := range map[string]string{"q4": "Q4", " web ": "WEB", "LAUNC": "LAUNC", "a1b2c": "A1B2C"} {
		got, err := Valid(typed)
		if err != nil || got != want {
			t.Errorf("Valid(%q) = %q, %v; want %q", typed, got, err, want)
		}
	}
	for _, typed := range []string{"", "Q", "LAUNCH", "4Q", "Q-4", "Q 4", "QÉ", "Q4!"} {
		if _, err := Valid(typed); !errors.Is(err, ErrBadKey) {
			t.Errorf("Valid(%q) = %v; want ErrBadKey", typed, err)
		}
	}
}

func TestATaskKeyIsReadAsAnyoneTypesIt(t *testing.T) {
	cases := map[string]struct {
		key string
		n   int
	}{
		"Q4-12":   {"Q4", 12},
		"q4-12":   {"Q4", 12},
		" WEB-1 ": {"WEB", 1},
		"A1B2C-7": {"A1B2C", 7},
	}
	for raw, want := range cases {
		k, n, ok := Parse(raw)
		if !ok || k != want.key || n != want.n {
			t.Errorf("Parse(%q) = %q %d %v; want %q %d", raw, k, n, ok, want.key, want.n)
		}
	}
	// Not keys: no number, a zero or leading zero, a key too long, a uuid,
	// a word with a dash, spaces inside.
	for _, raw := range []string{"Q4", "Q4-", "-12", "Q4-0", "Q4-012", "LAUNCH-1", "Q4-1x", "4Q-1",
		"a2573bba-d50b-44a2-b9f0-94df8d7310a7", "follow-up", "Q4 -12", "Q4-1234567890"} {
		if k, n, ok := Parse(raw); ok {
			t.Errorf("Parse(%q) = %q %d; want not a key", raw, k, n)
		}
	}
}

func TestAProjectsKeyComesFromItsName(t *testing.T) {
	first := map[string]string{
		"Q4 launch":           "Q4",  // a short first word that reads as a code
		"API v2":              "API", // written in capitals
		"Customer onboarding": "CO",  // initials
		"Website":             "WEB", // one word: its start
		"The launch plan":     "LP",  // minor words skipped
		"2026 roadmap":        "ROA", // starts from the first word with a letter
		"Café rebrand":        "CR",  // accents taken off
		"Marketing site redo": "MSR",
		"日本語":                 "PRJ", // no Latin letters
		"":                    "PRJ",
	}
	for name, want := range first {
		got := Derive(name)
		if len(got) == 0 || got[0] != want {
			t.Errorf("Derive(%q) starts %v; want %q first", name, head(got), want)
		}
	}
}

func TestEveryCandidateIsAValidKeyAndNoneRepeats(t *testing.T) {
	for _, name := range []string{"Q4 launch", "Website", "a", "日本語", "Customer onboarding and support for the whole year"} {
		got := Derive(name)
		if len(got) < 50 {
			t.Errorf("Derive(%q) offers only %d keys; a busy workspace needs more", name, len(got))
		}
		seen := map[string]bool{}
		for _, k := range got {
			if v, err := Valid(k); err != nil || v != k {
				t.Errorf("Derive(%q) offers %q, which isn't a key", name, k)
			}
			if seen[k] {
				t.Errorf("Derive(%q) offers %q twice", name, k)
			}
			seen[k] = true
		}
	}
	// Numbered forms follow the plain ones.
	got := Derive("Website")
	if i, j := slices.Index(got, "WEB"), slices.Index(got, "WEB2"); i < 0 || j < 0 || j < i {
		t.Errorf("Derive(Website) = %v; want WEB before WEB2", head(got))
	}
}

func head(s []string) []string {
	if len(s) > 6 {
		return s[:6]
	}
	return s
}
