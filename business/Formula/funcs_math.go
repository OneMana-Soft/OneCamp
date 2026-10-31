package formula

import (
	"math"
	"strings"
)

func init() {
	register(map[string]*function{
		"LOG":  {min: 1, max: 2, result: KindNumber, eager: fnLog},
		"EXP":  {min: 1, max: 1, result: KindNumber, eager: numeric1("EXP", math.Exp)},
		"INT":  {min: 1, max: 1, result: KindNumber, eager: numeric1("INT", math.Floor)},
		"EVEN": {min: 1, max: 1, result: KindNumber, eager: numeric1("EVEN", toParity(0))},
		"ODD":  {min: 1, max: 1, result: KindNumber, eager: numeric1("ODD", toParity(1))},

		"XOR": {min: 1, max: -1, result: KindBool, eager: fnXor},
		// ERROR gives no kind of its own, so IF({x}, ERROR(), 5) is a number column.
		"ERROR": {min: 0, max: 1, result: KindBlank, eager: fnError},

		// Counting goes into lists: COUNT({Tags}) counts a multi-select's
		// options, as Airtable's does with a lookup.
		"COUNT":    {min: 1, max: -1, result: KindNumber, eager: counter(func(v Value) bool { _, ok := countedNumber(v); return ok })},
		"COUNTA":   {min: 1, max: -1, result: KindNumber, eager: counter(func(v Value) bool { return v.Kind != KindBlank })},
		"COUNTALL": {min: 1, max: -1, result: KindNumber, eager: counter(func(Value) bool { return true })},
	})
}

// LOG(number, [base]) is the logarithm, in base 10 when no base is given.
func fnLog(_ *env, a []Value) Value {
	x, bad, ok := num("LOG", a[0])
	if !ok {
		return bad
	}
	base := 10.0
	if len(a) == 2 {
		if base, bad, ok = num("LOG", a[1]); !ok {
			return bad
		}
	}
	if x <= 0 {
		return Error("LOG needs a number above 0")
	}
	if base <= 0 || base == 1 {
		return Error("LOG needs a base above 0 that isn't 1")
	}
	return Number(math.Log(x) / math.Log(base))
}

// toParity rounds away from zero to the next even (0) or odd (1) whole
// number, as a spreadsheet's EVEN and ODD do: EVEN(1.5) is 2, ODD(-1.5) -3.
func toParity(parity float64) func(float64) float64 {
	return func(x float64) float64 {
		sign := 1.0
		if x < 0 {
			sign, x = -1, -x
		}
		n := math.Ceil(tidy(x))
		if math.Mod(n, 2) != parity {
			n++
		}
		if parity == 0 && n == 0 {
			return 0
		}
		return sign * n
	}
}

// XOR is true when an odd number of its values are.
func fnXor(e *env, a []Value) Value {
	odd := false
	for _, v := range a {
		if e.truthy(v) {
			odd = !odd
		}
	}
	return Bool(odd)
}

// ERROR([why]) is an error, saying why when it's given: a formula's way of
// marking a row as wrong, which ISERROR finds.
func fnError(_ *env, a []Value) Value {
	if len(a) == 1 {
		if why := a[0].Text(); why != "" {
			return Error(clip(why))
		}
	}
	return Error("#ERROR")
}

// countedNumber is whether a value counts as a number for COUNT: a number, or
// text that reads as one. A blank, a checkbox and a date don't.
func countedNumber(v Value) (float64, bool) {
	switch v.Kind {
	case KindNumber:
		return v.Num, true
	case KindText:
		return ParseNumber(v.Str, PlainSigns)
	default:
		return 0, false
	}
}

// counter counts the values, and each list's items, that counted says to.
func counter(counted func(Value) bool) func(*env, []Value) Value {
	return func(e *env, a []Value) Value {
		n := 0
		for _, v := range a {
			if v.list == nil {
				if counted(v) {
					n++
				}
				continue
			}
			e.spend(len(*v.list))
			for _, it := range *v.list {
				if counted(it) {
					n++
				}
			}
		}
		return Number(float64(n))
	}
}

// Beyond Airtable's list: what Notion's formulas and a spreadsheet have that
// people reach for.
func init() {
	register(map[string]*function{
		"IFS":      {min: 2, max: -1, lazy: fnIfs, branches: ifsBranches},
		"CONTAINS": {min: 2, max: 2, result: KindBool, eager: fnContains},
		"LN":       {min: 1, max: 1, result: KindNumber, eager: fnLn},
		"SIGN": {min: 1, max: 1, result: KindNumber, eager: numeric1("SIGN", func(x float64) float64 {
			switch {
			case x > 0:
				return 1
			case x < 0:
				return -1
			}
			return 0
		})},
		"PI": {min: 0, max: 0, result: KindNumber, eager: func(*env, []Value) Value { return Number(math.Pi) }},
	})
}

// IFS(test, value, test, value, …, [otherwise]) is the value after the first
// test that holds, as Notion's ifs() and a spreadsheet's IFS: IF without the
// nesting. An odd last value is what it gives when no test holds.
func fnIfs(e *env, a []node) Value {
	rest := a
	for len(rest) >= 2 {
		c := e.run(rest[0])
		if c.Kind == KindError {
			return c
		}
		if e.truthy(c) {
			return e.run(rest[1])
		}
		rest = rest[2:]
	}
	if len(rest) == 1 {
		return e.run(rest[0])
	}
	return Blank()
}

func ifsBranches(a []node) []node {
	var out []node
	rest := a
	for len(rest) >= 2 {
		out = append(out, rest[1])
		rest = rest[2:]
	}
	return append(out, rest...)
}

// CONTAINS(text, what) is whether what appears in text, ignoring case, as
// Notion's contains(); a list contains an item whose text is what.
func fnContains(e *env, a []Value) Value {
	what := strings.ToLower(a[1].Text())
	if a[0].list != nil {
		for _, it := range listOf(e, a[0]) {
			t := it.Text()
			e.slow(len(t))
			if strings.EqualFold(t, what) {
				return Bool(true)
			}
		}
		return Bool(false)
	}
	in := a[0].Text()
	e.slow(len(in))
	e.search(len(in), len(what))
	return Bool(strings.Contains(strings.ToLower(in), what))
}

// LN(number) is the natural logarithm.
func fnLn(_ *env, a []Value) Value {
	x, bad, ok := num("LN", a[0])
	if !ok {
		return bad
	}
	if x <= 0 {
		return Error("LN needs a number above 0")
	}
	return Number(math.Log(x))
}
