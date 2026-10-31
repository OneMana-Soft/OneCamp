package formula

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// quote writes s as a formula's text.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Any pattern over any text: an answer or an error, never a panic, never
// text past the limit, never more work than a row has.
func FuzzRegexFunctions(f *testing.F) {
	f.Add("Order 4521 shipped", `\d+`, "[$0]")
	f.Add(strings.Repeat("a", 3000), "", strings.Repeat("b", 50))
	f.Add("aaaa", "(a*)*", "$1$1")
	f.Add("x", "(", "")
	f.Fuzz(func(t *testing.T, text, pat, with string) {
		if !utf8.ValidString(text) || !utf8.ValidString(pat) || !utf8.ValidString(with) || len(text) > 20000 {
			return
		}
		for _, src := range []string{
			"REGEX_MATCH(" + quote(text) + ", " + quote(pat) + ")",
			"REGEX_EXTRACT(" + quote(text) + ", " + quote(pat) + ")",
			"REGEX_REPLACE(" + quote(text) + ", " + quote(pat) + ", " + quote(with) + ")",
		} {
			if utf8.RuneCountInString(src) > MaxLength {
				continue
			}
			v, steps := evalSpent(t, src)
			if v.Kind == KindText && utf8.RuneCountInString(v.Str) > max(maxText, utf8.RuneCountInString(text)) {
				t.Fatalf("%s gave %d characters", src, utf8.RuneCountInString(v.Str))
			}
			if steps > maxSteps+maxSteps/10 {
				t.Fatalf("%s took %d steps", src, steps)
			}
		}
	})
}

// Any layout writes and reads without a panic, and a day written in a
// layout of numbers alone reads back as the same day.
func FuzzMoments(f *testing.F) {
	f.Add("D MMMM YYYY", "12 October 2026", int64(1791763200))
	f.Add("[at] HH:mm Do", "Do", int64(0))
	f.Add("YYYYMMDD", "20261012", int64(-62135596800))
	f.Fuzz(func(t *testing.T, layout, text string, unix int64) {
		if len(layout) > 128 || len(text) > 64 {
			return
		}
		tm := time.Unix(unix%(1<<34), 0).UTC()
		_ = formatMoment(tm, layout, localeOf("fr"))
		_, _ = parseMoment(text, layout, localeOf(""), time.UTC)
		day := time.Date(tm.Year(), tm.Month(), tm.Day(), 0, 0, 0, 0, time.UTC)
		if day.Year() < 1000 || day.Year() > 9999 {
			return
		}
		written := formatMoment(day, "YYYY-MM-DD", localeOf(""))
		if v, ok := parseMoment(written, "YYYY-MM-DD", localeOf(""), time.UTC); !ok || !v.Time.Equal(day) || !v.Day {
			t.Fatalf("%s read back as %v %v", written, v, ok)
		}
	})
}
