// Package formula works out a table's formula fields: Airtable-style
// expressions over a row's other fields, such as {Price} * {Quantity} or
// IF({Due} < TODAY(), "Late", "On time").
//
// There is one evaluator, on the server, so the grid, filters, totals, guest
// links and questions asked of a table all read the same value. It is pure:
// formulas and a row's cells in, values out; the caller says how a cell reads
// (business/DataTable/formulaFields.go).
//
// It is tables' Formula Engine (design/tables-vs-airtable/PROGRAM.md §4.2
// #61), shared with what else reads formulas: task formula fields, automation
// conditions, AI fields' references. Refs is its dependency list (the
// programme's Deps): a stored formula names each field it reads as {#<id>}.
// The function list is kept equal to the web editor's by the same list in
// each side's test (TestFunctionsTheEditorLists, lib/tables/formula.test.ts).
package formula

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Kind is what a value is.
type Kind uint8

const (
	KindBlank Kind = iota
	KindNumber
	KindText
	KindBool
	KindDate
	KindError
)

// Name is the kind as the API and the web app spell it.
func (k Kind) Name() string {
	switch k {
	case KindNumber:
		return "number"
	case KindBool:
		return "checkbox"
	case KindDate:
		return "date"
	default:
		return "text"
	}
}

// Value is one formula's value for one row.
type Value struct {
	Kind Kind
	Num  float64
	// Str is the text, or what went wrong for an error.
	Str  string
	Bool bool
	Time time.Time
	// Day marks a date with no time of day: a date field, or TODAY().
	Day bool
}

// Blank is no value: an empty cell, or BLANK().
func Blank() Value { return Value{} }

// Number is a number; one that isn't finite is an error.
func Number(f float64) Value {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return Error("The result isn't a number")
	}
	if f == 0 {
		f = 0 // never "-0"
	}
	return Value{Kind: KindNumber, Num: f}
}

// maxText is the most characters of text a formula can build with &,
// CONCATENATE, REPT or SUBSTITUTE, so none can make a string that fills the
// server's memory (one SUBSTITUTE of a REPT made 100 MB a row). Text that's
// only cut or changed in case (LEFT, TRIM, UPPER…) can be as long as the
// cell it came from.
const maxText = 10000

func tooLong() Value {
	return Error(fmt.Sprintf("The text would be longer than %d characters", maxText))
}

// textResult is text a formula built: an error past limit characters.
func textResult(s string, limit int) Value {
	if len(s) > limit && utf8.RuneCountInString(s) > limit {
		return tooLong()
	}
	return Text(s)
}

// sentLen is how long s is once sent as JSON, at most: its quotes, and the
// escapes encoding/json writes for quotes, backslashes, control characters,
// <, > and &, which make one character up to six.
func sentLen(s string) int {
	n := 2
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				n += 2
			case c < 0x20 || c == '<' || c == '>' || c == '&':
				n += 6
			default:
				n++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || r == '\u2028' || r == '\u2029' {
			n += 6
		} else {
			n += size
		}
		i += size
	}
	return n
}

// quoteLen is the most of a value an error message quotes.
const quoteLen = 40

// clip is text as an error message quotes it: cut to quoteLen characters,
// with "…" when it's cut, so a message never carries a whole cell.
func clip(s string) string {
	n := 0
	for i := range s {
		if n == quoteLen {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// Text is text; the empty string is blank.
func Text(s string) Value {
	if s == "" {
		return Blank()
	}
	return Value{Kind: KindText, Str: s}
}

// Bool is a checkbox's yes or no.
func Bool(b bool) Value { return Value{Kind: KindBool, Bool: b} }

// Date is a moment, or a whole day when day is set.
func Date(t time.Time, day bool) Value {
	if day {
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	return Value{Kind: KindDate, Time: t, Day: day}
}

// Error is a formula that can't be worked out for this row, and why.
func Error(why string) Value { return Value{Kind: KindError, Str: why} }

// tidy drops float noise (0.1 + 0.2 reads 0.3) without touching real digits.
func tidy(f float64) float64 {
	if f == 0 || math.Abs(f) >= 1e15 {
		return f
	}
	if r := math.Round(f*1e10) / 1e10; r != 0 {
		return r
	}
	return 0
}

// FormatNumber is a number as text, without float noise or trailing zeros,
// and from 1e21 up as JavaScript writes it, 1e+21, rather than in 22 digits
// or more: 1e308 would be 309.
func FormatNumber(f float64) string {
	if f = tidy(f); math.Abs(f) >= 1e21 {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// Text is the value as text, the way & and CONCATENATE join it.
func (v Value) Text() string {
	switch v.Kind {
	case KindNumber:
		return FormatNumber(v.Num)
	case KindText:
		return v.Str
	case KindBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case KindDate:
		if v.Day {
			return v.Time.Format("2006-01-02")
		}
		return v.Time.Format("2006-01-02 15:04")
	default:
		return ""
	}
}

// number is the value as a number: blank is 0 and a checkbox 1 or 0, as in a
// spreadsheet. ok is false for text that isn't a number, and for a date.
func (v Value) number() (float64, bool) {
	switch v.Kind {
	case KindNumber:
		return v.Num, true
	case KindBlank:
		return 0, true
	case KindBool:
		if v.Bool {
			return 1, true
		}
		return 0, true
	case KindText:
		return ParseNumber(v.Str, PlainSigns)
	default:
		return 0, false
	}
}

// maxReadLen is the longest text read as a number or a date, and four times
// that with the spaces around it. None that people write is longer, failing
// to read a long cell as one copies it, and trimming some spaces is slow.
const maxReadLen = 64

// NumberSigns are the signs a person writes numbers with: the decimal sign,
// and the sign between groups of digits. "1,234.5" is {".", ","} (en-US),
// "1.234,5" is {",", "."} (de-DE), and "1 234,5" {",", " "}, a narrow
// no-break space (fr-FR). The web sends its reader's (lib/format/number.ts).
type NumberSigns struct {
	Decimal string `json:"decimal,omitempty"`
	Group   string `json:"group,omitempty"`
}

// PlainSigns are how programs write numbers, and people in the US, the UK
// and India: a point, and commas between groups.
var PlainSigns = NumberSigns{Decimal: ".", Group: ","}

// Checked is the signs to read numbers with: PlainSigns when none are given.
// ok is false for signs no number is written with: a decimal sign that isn't
// one character, a group sign of more than one, either a digit, a letter, a
// currency sign, a plus, a minus or "_", a space as the decimal sign, or one
// sign for both.
func (s NumberSigns) Checked() (NumberSigns, bool) {
	if s == (NumberSigns{}) {
		return PlainSigns, true
	}
	sign := func(t string, space bool) bool {
		r, n := utf8.DecodeRuneInString(t)
		return n > 0 && n == len(t) && r != utf8.RuneError && !unicode.IsDigit(r) && !unicode.IsLetter(r) &&
			!unicode.Is(unicode.Sc, r) && !strings.ContainsRune("+-−_", r) && (space || !unicode.IsSpace(r))
	}
	return s, sign(s.Decimal, false) && (s.Group == "" || sign(s.Group, true)) && s.Group != s.Decimal
}

// ParseNumber reads a number as a person writes it with signs (the zero
// NumberSigns are PlainSigns): "1,200.50", "1.234,5" with de-DE's signs,
// "€12", "12 €", "−1 234" and India's "1,00,000". Digits are grouped only in
// threes, or in India's twos then a three, by the group sign, a space, an
// apostrophe, or whichever of "." and "," isn't the decimal sign. A sign and
// one currency sign may come first, a currency sign last instead, and an
// exponent ("1e3") at the end. ok is false for anything else, so no text is
// read two ways: "12 kg", "TBD", "1,5" where a comma groups digits, NaN and
// Inf, and a number too big to hold. The web reads what a person types the
// same way (lib/format/number.ts parseTypedNumber).
func ParseNumber(s string, signs NumberSigns) (float64, bool) {
	if len(s) > 4*maxReadLen {
		return 0, false
	}
	s = strings.TrimSpace(s)
	if len(s) > maxReadLen || strings.ContainsRune(s, '_') {
		return 0, false
	}
	if signs.Decimal == "" {
		signs = PlainSigns
	}
	s = strings.ReplaceAll(s, "−", "-")
	minus, signed, currency := false, false, false
lead:
	for {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		r, n := utf8.DecodeRuneInString(s)
		switch {
		case (r == '-' || r == '+') && !signed:
			signed, minus = true, r == '-'
		case unicode.Is(unicode.Sc, r) && !currency:
			currency = true
		default:
			break lead
		}
		s = s[n:]
	}
	if r, n := utf8.DecodeLastRuneInString(s); unicode.Is(unicode.Sc, r) {
		if currency {
			return 0, false
		}
		s = strings.TrimRightFunc(s[:len(s)-n], unicode.IsSpace)
	}
	decimal, _ := utf8.DecodeRuneInString(signs.Decimal)
	group, _ := utf8.DecodeRuneInString(signs.Group)
	other := ','
	if decimal == ',' {
		other = '.'
	}
	// Each sign grouping digits becomes "_".
	s = strings.Map(func(r rune) rune {
		if r != decimal && (r == other || (signs.Group != "" && r == group) || r == '\'' || r == '’' || unicode.IsSpace(r)) {
			return '_'
		}
		return r
	}, s)
	whole, fraction, _ := strings.Cut(s, string(decimal))
	last := &whole
	if strings.ContainsRune(s, decimal) {
		last = &fraction
	}
	var exponent string
	*last, exponent = cutExponent(*last)
	if !allDigits(fraction) || !(allDigits(whole) || grouped(whole)) {
		return 0, false
	}
	digits := strings.ReplaceAll(whole, "_", "")
	if digits == "" && fraction == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat("0"+digits+"."+fraction+"0"+exponent, 64)
	if err != nil || math.IsInf(f, 0) {
		return 0, false
	}
	if minus && f != 0 {
		f = -f
	}
	return f, true
}

// cutExponent cuts an exponent ("e3", "E-7") off the end of s.
func cutExponent(s string) (string, string) {
	i := strings.LastIndexAny(s, "eE")
	if i < 0 {
		return s, ""
	}
	digits := s[i+1:]
	if digits != "" && (digits[0] == '+' || digits[0] == '-') {
		digits = digits[1:]
	}
	if digits == "" || !allDigits(digits) {
		return s, ""
	}
	return s[:i], s[i:]
}

// allDigits is whether s is digits alone ("" is).
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// grouped is whether s is digits in groups split by "_": threes after the
// first ("1_234_567"), or India's twos and then a three ("12_34_567").
func grouped(s string) bool {
	groups := strings.Split(s, "_")
	first, last := groups[0], groups[len(groups)-1]
	if len(groups) < 2 || len(first) < 1 || len(first) > 3 || len(last) != 3 || !allDigits(first) || !allDigits(last) {
		return false
	}
	threes, twos := true, len(first) <= 2
	for _, g := range groups[1 : len(groups)-1] {
		if !allDigits(g) {
			return false
		}
		threes, twos = threes && len(g) == 3, twos && len(g) == 2
	}
	return threes || twos
}

// ParseDate reads a date cell: a day ("2026-10-08") or a moment (RFC 3339).
func ParseDate(s string) (Value, bool) {
	if len(s) > 4*maxReadLen {
		return Value{}, false
	}
	s = strings.TrimSpace(s)
	if len(s) > maxReadLen {
		return Value{}, false
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return Date(t, true), true
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.Parse(layout, s); err == nil {
			return Date(t, false), true
		}
	}
	return Value{}, false
}

// JSON is the value as it's sent and stored in a row: a number, text, a
// checkbox's true or false, a date as "2026-10-08" (or RFC 3339 with a time),
// null when blank, and {"error": why} when it couldn't be worked out.
func (v Value) JSON() interface{} {
	switch v.Kind {
	case KindNumber:
		return tidy(v.Num)
	case KindText:
		return v.Str
	case KindBool:
		return v.Bool
	case KindDate:
		if v.Day {
			return v.Time.Format("2006-01-02")
		}
		return v.Time.UTC().Format(time.RFC3339)
	case KindError:
		return map[string]interface{}{"error": v.Str}
	default:
		return nil
	}
}
