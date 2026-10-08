// Package formula works out a table's formula fields: Airtable-style
// expressions over a row's other fields, such as {Price} * {Quantity} or
// IF({Due} < TODAY(), "Late", "On time").
//
// There is one evaluator, on the server, so the grid, filters, totals, guest
// links and questions asked of a table all read the same value. It is pure:
// formulas and a row's cells in, values out; the caller says how a cell reads
// (business/DataTable/formulaFields.go).
package formula

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
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

// text is the value as text, the way & and CONCATENATE join it.
func (v Value) text() string {
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
		return ParseNumber(v.Str)
	default:
		return 0, false
	}
}

// maxReadLen is the longest text read as a number or a date, and four times
// that with the spaces around it. None that people write is longer, failing
// to read a long cell as one copies it, and trimming some spaces is slow.
const maxReadLen = 64

// ParseNumber reads a number written as people write them: "1,200",
// " 3.5 ", "$40", "₹ 1,999". ok is false when it isn't one.
func ParseNumber(s string) (float64, bool) {
	if len(s) > 4*maxReadLen {
		return 0, false
	}
	s = strings.TrimSpace(s)
	if len(s) > maxReadLen {
		return 0, false
	}
	s = strings.TrimLeft(s, "$€£₹¥ ")
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
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
