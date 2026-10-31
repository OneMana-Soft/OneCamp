package formula

import (
	"fmt"
	"math"
	"strings"
	"time"
)

func init() {
	register(map[string]*function{
		"ARRAYJOIN":    {min: 1, max: 2, result: KindText, eager: fnArrayJoin},
		"ARRAYUNIQUE":  {min: 1, max: 1, result: KindText, eager: fnArrayUnique},
		"ARRAYCOMPACT": {min: 1, max: 1, result: KindText, eager: fnArrayCompact},
		"ARRAYFLATTEN": {min: 1, max: -1, result: KindText, eager: fnArrayFlatten},
		"ARRAYSLICE":   {min: 2, max: 3, result: KindText, eager: fnArraySlice},

		"RECORD_ID":          {min: 0, max: 0, result: KindText, eager: func(e *env, _ []Value) Value { return Text(e.row.ID) }},
		"CREATED_TIME":       {min: 0, max: 0, result: KindDate, eager: func(e *env, _ []Value) Value { return stamp(e, e.row.Created) }},
		"LAST_MODIFIED_TIME": {min: 0, max: 0, result: KindDate, eager: func(e *env, _ []Value) Value { return stamp(e, e.row.Modified) }},
	})
}

// stamp is when a row was made or changed, on the reader's clock; blank for
// a row that has none (a draft's preview).
func stamp(e *env, t time.Time) Value {
	if t.IsZero() {
		return Blank()
	}
	return e.local(Date(t, false))
}

// built is a list a function made, charged for making it: about 1 µs an item
// to keep it and join its text (measured).
func built(e *env, items []Value) Value {
	e.spend(20 * len(items))
	v := List(items)
	e.scan(len(v.Str))
	return v
}

// listOf is a value's items, charged a step each as they're read.
func listOf(e *env, v Value) []Value {
	items := v.items()
	e.spend(len(items))
	return items
}

// ARRAYJOIN(list, [separator]) joins a list's items with the separator
// (", " when none is given).
func fnArrayJoin(e *env, a []Value) Value {
	sep := ", "
	if len(a) == 2 {
		sep = a[1].Text()
	}
	items := listOf(e, a[0])
	parts := make([]string, len(items))
	size := 0
	e.spend(4 * len(items))
	for i, it := range items {
		parts[i] = it.Text()
		if size += len(parts[i]) + len(sep); size > 4*maxText {
			return tooLong()
		}
	}
	out := strings.Join(parts, sep)
	e.scan(len(out))
	return textResult(out, maxText)
}

// ARRAYUNIQUE(list) is the list with each item once, in the order it first
// appears; items are the same when their text is, ignoring case, as filters
// compare.
func fnArrayUnique(e *env, a []Value) Value {
	items := listOf(e, a[0])
	e.spend(40 * len(items)) // a set of them, about 2 µs an item
	seen := make(map[string]bool, len(items))
	var out []Value
	for _, it := range items {
		key := strings.ToLower(it.Text())
		e.slow(len(key))
		if !seen[key] {
			seen[key] = true
			out = append(out, it)
		}
	}
	return built(e, out)
}

// ARRAYCOMPACT(list) is the list without its blank items.
func fnArrayCompact(e *env, a []Value) Value {
	var out []Value
	for _, it := range listOf(e, a[0]) {
		if it.Kind != KindBlank && !(it.Kind == KindText && strings.TrimSpace(it.Str) == "") {
			out = append(out, it)
		}
	}
	return built(e, out)
}

// maxItems is the most items a list a formula builds can have.
const maxItems = 10000

// ARRAYFLATTEN(list, …) is the items of every list given, as one list.
func fnArrayFlatten(e *env, a []Value) Value {
	var out []Value
	for _, v := range a {
		out = append(out, listOf(e, v)...)
		if len(out) > maxItems {
			return Error(fmt.Sprintf("ARRAYFLATTEN would make a list of more than %d items", maxItems))
		}
	}
	return built(e, out)
}

// ARRAYSLICE(list, start, [end]) is the items from start to end, both
// included, counted from 1; a negative count is from the end (-1 the last).
func fnArraySlice(e *env, a []Value) Value {
	items := listOf(e, a[0])
	at := func(v Value, fallback int) (int, Value, bool) {
		f, bad, ok := num("ARRAYSLICE", v)
		if !ok {
			return 0, bad, false
		}
		n := int(math.Max(math.Min(math.Trunc(f), 1e9), -1e9))
		switch {
		case n < 0:
			n = len(items) + n + 1
		case n == 0:
			n = fallback
		}
		return n, Value{}, true
	}
	start, bad, ok := at(a[1], 1)
	if !ok {
		return bad
	}
	end := len(items)
	if len(a) == 3 {
		if end, bad, ok = at(a[2], len(items)); !ok {
			return bad
		}
	}
	start = max(start, 1)
	end = min(end, len(items))
	if start > end {
		return Blank()
	}
	return built(e, items[start-1:end])
}

// FILTER and MAP go through a list a formula at a time, as Notion's filter()
// and map() do: CURRENT() is the item the formula is looking at.
func init() {
	register(map[string]*function{
		"FILTER":  {min: 2, max: 2, lazy: fnFilter, result: KindText},
		"MAP":     {min: 2, max: 2, lazy: fnMap, result: KindText},
		"CURRENT": {min: 0, max: 0, result: KindBlank, eager: fnCurrent},
	})
}

func fnCurrent(e *env, _ []Value) Value {
	if len(e.current) == 0 {
		return Error("CURRENT() is the item FILTER or MAP is looking at, so it goes inside one")
	}
	return e.current[len(e.current)-1]
}

// each runs f on every item of the list a[0] gives, with CURRENT() reading
// it; an error from f is the answer.
func each(e *env, name string, a []node, f func(item, got Value) bool) (Value, bool) {
	list := e.run(a[0])
	if list.Kind == KindError {
		return list, false
	}
	items := listOf(e, list)
	if len(items) > maxItems {
		return Error(fmt.Sprintf("%s goes through at most %d items", name, maxItems)), false
	}
	for _, it := range items {
		e.current = append(e.current, it)
		got := e.run(a[1])
		e.current = e.current[:len(e.current)-1]
		if got.Kind == KindError {
			return got, false
		}
		if !f(it, got) {
			break
		}
	}
	return Value{}, true
}

// FILTER(list, test) is the items the test holds for: FILTER({Amounts},
// CURRENT() > 100).
func fnFilter(e *env, a []node) Value {
	var out []Value
	if bad, ok := each(e, "FILTER", a, func(item, got Value) bool {
		if e.truthy(got) {
			out = append(out, item)
		}
		return true
	}); !ok {
		return bad
	}
	return built(e, out)
}

// MAP(list, value) is the value worked out for each item: MAP({Names},
// UPPER(CURRENT())).
func fnMap(e *env, a []node) Value {
	var out []Value
	if bad, ok := each(e, "MAP", a, func(_, got Value) bool {
		out = append(out, got)
		return true
	}); !ok {
		return bad
	}
	return built(e, out)
}
