package formula

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// function is one of the functions a formula can call. Names and behaviour
// follow Airtable's where it has one, as that's what most people know.
type function struct {
	// min and max are how many values it takes; max -1 is any number.
	min, max int
	// result is what it gives, for a column's type; IF and SWITCH give what
	// their branches give instead.
	result   Kind
	branches func(args []node) []node
	// lazy functions work out only the arguments they need (IF, SWITCH);
	// eager ones get every argument worked out, and an error in any is the
	// answer unless takesErrors.
	lazy        func(e *env, args []node) Value
	eager       func(e *env, args []Value) Value
	takesErrors bool
}

func (f *function) arity() string {
	switch {
	case f.min == 0 && f.max == 0:
		return "no values"
	case f.max < 0:
		return fmt.Sprintf("at least %d %s", f.min, plural(f.min))
	case f.min == f.max:
		return fmt.Sprintf("%d %s", f.min, plural(f.min))
	default:
		return fmt.Sprintf("%d to %d values", f.min, f.max)
	}
}

func plural(n int) string {
	if n == 1 {
		return "value"
	}
	return "values"
}

var functions = map[string]*function{
	// Logic
	"IF":      {min: 2, max: 3, lazy: fnIf, branches: func(a []node) []node { return a[1:] }},
	"SWITCH":  {min: 3, max: -1, lazy: fnSwitch, branches: switchBranches},
	"AND":     {min: 1, max: -1, result: KindBool, eager: fnAnd},
	"OR":      {min: 1, max: -1, result: KindBool, eager: fnOr},
	"NOT":     {min: 1, max: 1, result: KindBool, eager: func(e *env, a []Value) Value { return Bool(!e.truthy(a[0])) }},
	"BLANK":   {min: 0, max: 0, result: KindBlank, eager: func(*env, []Value) Value { return Blank() }},
	"ISBLANK": {min: 1, max: 1, result: KindBool, eager: func(e *env, a []Value) Value { return Bool(e.trimmed(a[0].Text()) == "") }},
	"ISERROR": {min: 1, max: 1, result: KindBool, takesErrors: true, eager: func(_ *env, a []Value) Value { return Bool(a[0].Kind == KindError) }},
	"TRUE":    {min: 0, max: 0, result: KindBool, eager: func(*env, []Value) Value { return Bool(true) }},
	"FALSE":   {min: 0, max: 0, result: KindBool, eager: func(*env, []Value) Value { return Bool(false) }},

	// Numbers
	"SUM":       {min: 1, max: -1, result: KindNumber, eager: fnSum},
	"AVERAGE":   {min: 1, max: -1, result: KindNumber, eager: fnAverage},
	"MIN":       {min: 1, max: -1, result: KindNumber, eager: fnMin},
	"MAX":       {min: 1, max: -1, result: KindNumber, eager: fnMax},
	"ROUND":     {min: 1, max: 2, result: KindNumber, eager: rounder("ROUND", math.Round)},
	"ROUNDUP":   {min: 1, max: 2, result: KindNumber, eager: rounder("ROUNDUP", awayFromZero)},
	"ROUNDDOWN": {min: 1, max: 2, result: KindNumber, eager: rounder("ROUNDDOWN", math.Trunc)},
	"CEILING":   {min: 1, max: 2, result: KindNumber, eager: toMultiple("CEILING", math.Ceil)},
	"FLOOR":     {min: 1, max: 2, result: KindNumber, eager: toMultiple("FLOOR", math.Floor)},
	"ABS":       {min: 1, max: 1, result: KindNumber, eager: numeric1("ABS", math.Abs)},
	"SQRT":      {min: 1, max: 1, result: KindNumber, eager: fnSqrt},
	"MOD":       {min: 2, max: 2, result: KindNumber, eager: fnMod},
	"POWER":     {min: 2, max: 2, result: KindNumber, eager: fnPower},
	"VALUE":     {min: 1, max: 1, result: KindNumber, eager: fnValue},

	// Text
	"CONCATENATE": {min: 1, max: -1, result: KindText, eager: fnConcat},
	"CONCAT":      {min: 1, max: -1, result: KindText, eager: fnConcat},
	"LEN":         {min: 1, max: 1, result: KindNumber, eager: fnLen},
	"UPPER":       {min: 1, max: 1, result: KindText, eager: changeCase(strings.ToUpper)},
	"LOWER":       {min: 1, max: 1, result: KindText, eager: changeCase(strings.ToLower)},
	"TRIM":        {min: 1, max: 1, result: KindText, eager: func(e *env, a []Value) Value { return Text(e.trimmed(a[0].Text())) }},
	"LEFT":        {min: 2, max: 2, result: KindText, eager: fnLeft},
	"RIGHT":       {min: 2, max: 2, result: KindText, eager: fnRight},
	"MID":         {min: 3, max: 3, result: KindText, eager: fnMid},
	"FIND":        {min: 2, max: 3, result: KindNumber, eager: fnFind},
	"SUBSTITUTE":  {min: 3, max: 3, result: KindText, eager: fnSubstitute},
	"REPT":        {min: 2, max: 2, result: KindText, eager: fnRept},

	// Dates
	"TODAY":         {min: 0, max: 0, result: KindDate, eager: func(e *env, _ []Value) Value { return Date(e.now.In(e.loc), true) }},
	"NOW":           {min: 0, max: 0, result: KindDate, eager: func(e *env, _ []Value) Value { return e.local(Date(e.now, false)) }},
	"YEAR":          {min: 1, max: 1, result: KindNumber, eager: datePart("YEAR", func(t time.Time) int { return t.Year() })},
	"MONTH":         {min: 1, max: 1, result: KindNumber, eager: datePart("MONTH", func(t time.Time) int { return int(t.Month()) })},
	"DAY":           {min: 1, max: 1, result: KindNumber, eager: datePart("DAY", func(t time.Time) int { return t.Day() })},
	"WEEKDAY":       {min: 1, max: 1, result: KindNumber, eager: datePart("WEEKDAY", func(t time.Time) int { return int(t.Weekday()) })},
	"DATEADD":       {min: 3, max: 3, result: KindDate, eager: fnDateAdd},
	"DATETIME_DIFF": {min: 2, max: 3, result: KindNumber, eager: fnDatetimeDiff},
	"WORKDAY_DIFF":  {min: 2, max: 2, result: KindNumber, eager: fnWorkdayDiff},
}

func fnIf(e *env, a []node) Value {
	c := e.run(a[0])
	if c.Kind == KindError {
		return c
	}
	if e.truthy(c) {
		return e.run(a[1])
	}
	if len(a) == 3 {
		return e.run(a[2])
	}
	return Blank()
}

// SWITCH(value, match, result, …, [otherwise]).
func fnSwitch(e *env, a []node) Value {
	x := e.run(a[0])
	if x.Kind == KindError {
		return x
	}
	rest := a[1:]
	for len(rest) >= 2 {
		m := e.run(rest[0])
		if m.Kind == KindError {
			return m
		}
		if compare(e, x, m) == 0 {
			return e.run(rest[1])
		}
		rest = rest[2:]
	}
	if len(rest) == 1 {
		return e.run(rest[0])
	}
	return Blank()
}

func switchBranches(a []node) []node {
	var out []node
	rest := a[1:]
	for len(rest) >= 2 {
		out = append(out, rest[1])
		rest = rest[2:]
	}
	return append(out, rest...)
}

func fnAnd(e *env, a []Value) Value {
	for _, v := range a {
		if !e.truthy(v) {
			return Bool(false)
		}
	}
	return Bool(true)
}

func fnOr(e *env, a []Value) Value {
	for _, v := range a {
		if e.truthy(v) {
			return Bool(true)
		}
	}
	return Bool(false)
}

// numbersOf reads the numbers a function was given, leaving out blanks. bad
// is the error when one isn't a number.
func numbersOf(name string, a []Value) (nums []float64, bad Value, ok bool) {
	for _, v := range a {
		if v.Kind == KindBlank {
			continue
		}
		f, isNum := v.number()
		if !isNum {
			return nil, Error(fmt.Sprintf("%s needs numbers, and %q isn't one", name, clip(v.Text()))), false
		}
		nums = append(nums, f)
	}
	return nums, Value{}, true
}

func fnSum(_ *env, a []Value) Value {
	nums, bad, ok := numbersOf("SUM", a)
	if !ok {
		return bad
	}
	total := 0.0
	for _, f := range nums {
		total += f
	}
	return Number(total)
}

func fnAverage(_ *env, a []Value) Value {
	nums, bad, ok := numbersOf("AVERAGE", a)
	if !ok {
		return bad
	}
	if len(nums) == 0 {
		return Blank()
	}
	total := 0.0
	for _, f := range nums {
		total += f
	}
	return Number(total / float64(len(nums)))
}

func extreme(name string, a []Value, better func(x, best float64) bool) Value {
	nums, bad, ok := numbersOf(name, a)
	if !ok {
		return bad
	}
	if len(nums) == 0 {
		return Blank()
	}
	best := nums[0]
	for _, f := range nums[1:] {
		if better(f, best) {
			best = f
		}
	}
	return Number(best)
}

func fnMin(_ *env, a []Value) Value {
	return extreme("MIN", a, func(x, best float64) bool { return x < best })
}

func fnMax(_ *env, a []Value) Value {
	return extreme("MAX", a, func(x, best float64) bool { return x > best })
}

// num reads argument i as a number for function name.
func num(name string, v Value) (float64, Value, bool) {
	f, ok := v.number()
	if !ok {
		return 0, Error(fmt.Sprintf("%s needs a number, and %q isn't one", name, clip(v.Text()))), false
	}
	return f, Value{}, true
}

func numeric1(name string, f func(float64) float64) func(*env, []Value) Value {
	return func(_ *env, a []Value) Value {
		x, bad, ok := num(name, a[0])
		if !ok {
			return bad
		}
		return Number(f(x))
	}
}

func awayFromZero(x float64) float64 {
	if x < 0 {
		return -math.Ceil(-x)
	}
	return math.Ceil(x)
}

// rounder rounds to a number of decimal places (0 when not given; negative
// rounds to tens, hundreds…).
func rounder(name string, round func(float64) float64) func(*env, []Value) Value {
	return func(_ *env, a []Value) Value {
		x, bad, ok := num(name, a[0])
		if !ok {
			return bad
		}
		places := 0.0
		if len(a) == 2 {
			if places, bad, ok = num(name, a[1]); !ok {
				return bad
			}
		}
		scale := math.Pow(10, math.Trunc(places))
		return Number(round(tidy(x*scale)) / scale)
	}
}

// toMultiple rounds up or down to a multiple of the second value (1 when not
// given), as CEILING and FLOOR do.
func toMultiple(name string, round func(float64) float64) func(*env, []Value) Value {
	return func(_ *env, a []Value) Value {
		x, bad, ok := num(name, a[0])
		if !ok {
			return bad
		}
		step := 1.0
		if len(a) == 2 {
			if step, bad, ok = num(name, a[1]); !ok {
				return bad
			}
		}
		if step == 0 {
			return Number(0)
		}
		return Number(round(tidy(x/step)) * step)
	}
}

func fnSqrt(_ *env, a []Value) Value {
	x, bad, ok := num("SQRT", a[0])
	if !ok {
		return bad
	}
	if x < 0 {
		return Error("SQRT needs a number that isn't negative")
	}
	return Number(math.Sqrt(x))
}

// MOD's result takes the divisor's sign, as in a spreadsheet.
func fnMod(_ *env, a []Value) Value {
	x, bad, ok := num("MOD", a[0])
	if !ok {
		return bad
	}
	d, bad, ok := num("MOD", a[1])
	if !ok {
		return bad
	}
	if d == 0 {
		return Error("Divided by zero")
	}
	r := math.Mod(x, d)
	if r != 0 && (r < 0) != (d < 0) {
		r += d
	}
	return Number(r)
}

func fnPower(_ *env, a []Value) Value {
	x, bad, ok := num("POWER", a[0])
	if !ok {
		return bad
	}
	y, bad, ok := num("POWER", a[1])
	if !ok {
		return bad
	}
	return Number(math.Pow(x, y))
}

func fnValue(_ *env, a []Value) Value {
	if a[0].Kind == KindBlank {
		return Blank()
	}
	x, bad, ok := num("VALUE", a[0])
	if !ok {
		return bad
	}
	return Number(x)
}

// changeCase is UPPER or LOWER, which go through text a character at a time.
func changeCase(to func(string) string) func(*env, []Value) Value {
	return func(e *env, a []Value) Value {
		s := a[0].Text()
		e.slow(len(s))
		return Text(to(s))
	}
}

func fnConcat(_ *env, a []Value) Value {
	var b strings.Builder
	for _, v := range a {
		b.WriteString(v.Text())
		if b.Len() > 4*maxText {
			return tooLong()
		}
	}
	return textResult(b.String(), maxText)
}

// count reads a count of characters (or repeats): a whole number, not negative.
func count(name string, v Value) (int, Value, bool) {
	f, bad, ok := num(name, v)
	if !ok {
		return 0, bad, false
	}
	if f < 0 {
		return 0, Error(fmt.Sprintf("%s needs a count that isn't negative", name)), false
	}
	return int(math.Min(math.Trunc(f), 1e6)), Value{}, true
}

// after is where the text after s's first n characters starts, in bytes
// (len(s) when it has fewer), found without copying s: LEFT, MID and FIND
// count characters, and a cell can be long.
func after(e *env, s string, n int) int {
	at := len(s)
	for i := range s {
		if n == 0 {
			at = i
			break
		}
		n--
	}
	e.scan(at)
	return at
}

// runes counts s's characters.
func runes(e *env, s string) int {
	e.scan(len(s))
	return utf8.RuneCountInString(s)
}

func fnLen(e *env, a []Value) Value { return Number(float64(runes(e, a[0].Text()))) }

func fnLeft(e *env, a []Value) Value {
	s := a[0].Text()
	n, bad, ok := count("LEFT", a[1])
	if !ok {
		return bad
	}
	return Text(s[:after(e, s, n)])
}

func fnRight(e *env, a []Value) Value {
	s := a[0].Text()
	n, bad, ok := count("RIGHT", a[1])
	if !ok {
		return bad
	}
	if total := runes(e, s); n < total {
		s = s[after(e, s, total-n):]
	}
	return Text(s)
}

// MID(text, start, count), with start counted from 1.
func fnMid(e *env, a []Value) Value {
	s := a[0].Text()
	start, bad, ok := count("MID", a[1])
	if !ok {
		return bad
	}
	if start < 1 {
		return Error("MID counts from 1")
	}
	n, bad, ok := count("MID", a[2])
	if !ok {
		return bad
	}
	i := after(e, s, start-1)
	if i == len(s) {
		return Blank()
	}
	return Text(s[i : i+after(e, s[i:], n)])
}

// FIND(what, in, [from]) is where what first appears in in, counted from 1,
// or 0 when it doesn't. It minds case, as Airtable's does.
func fnFind(e *env, a []Value) Value {
	what, in := a[0].Text(), a[1].Text()
	from := 1
	if len(a) == 3 {
		f, bad, ok := count("FIND", a[2])
		if !ok {
			return bad
		}
		if f > 1 {
			from = f
		}
	}
	start := after(e, in, from-1)
	if start == len(in) && runes(e, in) < from-1 {
		return Number(0) // from is past the end
	}
	e.search(len(in)-start, len(what))
	i := strings.Index(in[start:], what)
	if i < 0 {
		return Number(0)
	}
	return Number(float64(from + runes(e, in[start:start+i])))
}

// SUBSTITUTE can build text up to maxText characters, or keep text that was
// longer as long as it was: swapping a long cell's line breaks for spaces
// works.
func fnSubstitute(e *env, a []Value) Value {
	s, old, repl := a[0].Text(), a[1].Text(), a[2].Text()
	if old == "" {
		return Text(s)
	}
	e.slow(len(s))
	// Counted, then replaced: strings.Count, and ReplaceAll's own count and
	// search, each look for old through s.
	e.search(3*len(s), len(old))
	limit := max(maxText, utf8.RuneCountInString(s))
	// Sized before it's built: a few characters swapped for long ones
	// multiply.
	if n := strings.Count(s, old); len(s)+n*(len(repl)-len(old)) > 4*limit {
		return tooLong()
	}
	return textResult(strings.ReplaceAll(s, old, repl), limit)
}

func fnRept(_ *env, a []Value) Value {
	s := a[0].Text()
	n, bad, ok := count("REPT", a[1])
	if !ok {
		return bad
	}
	if utf8.RuneCountInString(s)*n > maxText {
		return Error(fmt.Sprintf("REPT would make more than %d characters", maxText))
	}
	return Text(strings.Repeat(s, n))
}

// date reads a value as a date for function name, a moment on the reader's
// clock.
func date(e *env, name string, v Value) (Value, Value, bool) {
	d, ok := asDate(v)
	if !ok {
		return Value{}, Error(fmt.Sprintf("%s needs a date, and %q isn't one", name, clip(v.Text()))), false
	}
	return e.local(d), Value{}, true
}

// calendar is the date as a calendar shows it: a day as is, a moment in the
// workspace's time zone.
func calendar(e *env, d Value) time.Time {
	if d.Day {
		return d.Time
	}
	return d.Time.In(e.loc)
}

func datePart(name string, part func(time.Time) int) func(*env, []Value) Value {
	return func(e *env, a []Value) Value {
		if a[0].Kind == KindBlank {
			return Blank()
		}
		d, bad, ok := date(e, name, a[0])
		if !ok {
			return bad
		}
		return Number(float64(part(calendar(e, d))))
	}
}

// unit reads a unit of time: years, months, weeks, days, hours or minutes.
func unit(name string, v Value) (string, Value, bool) {
	if s := v.Text(); len(s) <= maxReadLen {
		switch u := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), "s"); u {
		case "year", "month", "week", "day", "hour", "minute":
			return u, Value{}, true
		}
	}
	return "", Error(fmt.Sprintf("%s counts in years, months, weeks, days, hours or minutes, not %q", name, clip(v.Text()))), false
}

// addTo moves a date by n of a unit. A day moved by whole days stays a day.
func addTo(d Value, n float64, u string) Value {
	whole := n == math.Trunc(n)
	switch u {
	case "year":
		return Date(addMonths(d.Time, int(n)*12), d.Day)
	case "month":
		return Date(addMonths(d.Time, int(n)), d.Day)
	case "week":
		n *= 7
		whole = n == math.Trunc(n)
		fallthrough
	case "day":
		if whole {
			return Date(d.Time.AddDate(0, 0, int(n)), d.Day)
		}
		return Date(d.Time.Add(time.Duration(n*24*float64(time.Hour))), false)
	case "hour":
		return Date(d.Time.Add(time.Duration(n*float64(time.Hour))), false)
	default: // minute
		return Date(d.Time.Add(time.Duration(n*float64(time.Minute))), false)
	}
}

// addMonths moves t by n calendar months, keeping its day of the month where
// the month has it and the month's last day where it doesn't: 31 January plus
// a month is 28 February, as Airtable and a spreadsheet's EDATE have it.
func addMonths(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	total := int(m) - 1 + n
	q := total / 12
	if total < 0 && total%12 != 0 {
		q--
	}
	month := total - q*12 // 0 to 11
	year := y + q
	if last := time.Date(year, time.Month(month+2), 0, 0, 0, 0, 0, time.UTC).Day(); d > last {
		d = last
	}
	return time.Date(year, time.Month(month+1), d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// DATEADD(date, count, unit).
func fnDateAdd(e *env, a []Value) Value {
	if a[0].Kind == KindBlank {
		return Blank()
	}
	d, bad, ok := date(e, "DATEADD", a[0])
	if !ok {
		return bad
	}
	n, bad, ok := num("DATEADD", a[1])
	if !ok {
		return bad
	}
	u, bad, ok := unit("DATEADD", a[2])
	if !ok {
		return bad
	}
	return addTo(d, n, u)
}

// DATETIME_DIFF(later, earlier, [unit]) is later minus earlier in whole units
// (days when not given), negative when "later" is earlier.
func fnDatetimeDiff(e *env, a []Value) Value {
	if a[0].Kind == KindBlank || a[1].Kind == KindBlank {
		return Blank()
	}
	later, bad, ok := date(e, "DATETIME_DIFF", a[0])
	if !ok {
		return bad
	}
	earlier, bad, ok := date(e, "DATETIME_DIFF", a[1])
	if !ok {
		return bad
	}
	u := "day"
	if len(a) == 3 {
		if u, bad, ok = unit("DATETIME_DIFF", a[2]); !ok {
			return bad
		}
	}
	switch u {
	case "year":
		return Number(math.Trunc(float64(monthsBetween(earlier.Time, later.Time)) / 12))
	case "month":
		return Number(float64(monthsBetween(earlier.Time, later.Time)))
	case "week":
		return Number(math.Trunc(daysBetween(earlier, later) / 7))
	case "day":
		return Number(math.Trunc(daysBetween(earlier, later)))
	case "hour":
		return Number(math.Trunc(secondsBetween(earlier.Time, later.Time) / 3600))
	default:
		return Number(math.Trunc(secondsBetween(earlier.Time, later.Time) / 60))
	}
}

// monthsBetween counts whole calendar months from a to b (negative when b is
// earlier), as a spreadsheet's DATEDIF does.
func monthsBetween(a, b time.Time) int {
	sign := 1
	if b.Before(a) {
		a, b, sign = b, a, -1
	}
	m := (b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month())
	if b.Day() < a.Day() || (b.Day() == a.Day() && clock(b) < clock(a)) {
		m--
	}
	return sign * m
}

// clock is the time of day.
func clock(t time.Time) time.Duration {
	return t.Sub(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()))
}

// WORKDAY_DIFF(start, end) counts the weekdays from start to end, both
// included, as Airtable's does; negative when end is earlier.
func fnWorkdayDiff(e *env, a []Value) Value {
	if a[0].Kind == KindBlank || a[1].Kind == KindBlank {
		return Blank()
	}
	s, bad, ok := date(e, "WORKDAY_DIFF", a[0])
	if !ok {
		return bad
	}
	t, bad, ok := date(e, "WORKDAY_DIFF", a[1])
	if !ok {
		return bad
	}
	from, to := dayOf(calendar(e, s)), dayOf(calendar(e, t))
	sign := 1.0
	if to.Before(from) {
		from, to, sign = to, from, -1
	}
	days := int((to.Unix()-from.Unix())/86400) + 1
	n := days / 7 * 5
	for i, wd := 0, from.Weekday(); i < days%7; i, wd = i+1, (wd+1)%7 {
		if wd != time.Saturday && wd != time.Sunday {
			n++
		}
	}
	return Number(sign * float64(n))
}

func dayOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
