package business

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func written(t *testing.T, s query.Summaries, field string) string {
	t.Helper()
	b, err := json.Marshal(s[field])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Over no rows a count or a share is 0, as the summary bar writes it, and a
// number's or a date's function is null: there is nothing to add up.
func TestSummariesOfNoRowsCountNoneAndFoldNothing(t *testing.T) {
	reqs := []query.SummaryRequest{
		{Field: "n", Fns: []string{"count", "empty", "filled", "pct_empty", "pct_filled", "unique", "sum", "average", "median", "min", "max", "range"}},
		{Field: "d", Fns: []string{"earliest", "latest", "range"}},
		{Field: "b", Fns: []string{"checked", "unchecked"}},
	}
	got, err := summariesOf(reqs, 0, map[string]*totals{"n": {}, "d": {}, "b": {}})
	if err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{
		"n": `{"average":null,"count":0,"empty":0,"filled":0,"max":null,"median":null,"min":null,"pct_empty":0,"pct_filled":0,"range":null,"sum":null,"unique":0}`,
		"d": `{"earliest":null,"latest":null,"range":null}`,
		"b": `{"checked":0,"unchecked":0}`,
	} {
		if g := written(t, got, field); g != want {
			t.Errorf("%s: got %s, want %s", field, g, want)
		}
	}
}

// A sum past what a float holds is written as null, never as an error that
// would cost the reader the whole window.
func TestASumPastAFloatIsNull(t *testing.T) {
	big := math.MaxFloat64
	got, err := summariesOf([]query.SummaryRequest{{Field: "n", Fns: []string{"sum", "max"}}}, 2,
		map[string]*totals{"n": {filled: 2, sum: math.Inf(1), max: &big}})
	if err != nil {
		t.Fatal(err)
	}
	if g := written(t, got, "n"); g != `{"max":1.7976931348623157e+308,"sum":null}` {
		t.Errorf("got %s", g)
	}
}

// A day is written as its date and a moment at UTC; the day comes first at
// its own midnight, either way, as the index sorts them.
func TestDaysAndMomentsAreWrittenAsTheyCame(t *testing.T) {
	midnight := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	day, at := moment{at: midnight, day: true}, moment{at: midnight}
	if !earlier(day, at) || earlier(at, day) || !later(day, at) || later(at, day) {
		t.Error("a day should win its midnight's tie both ways")
	}
	f := &folding{seen: map[interface{}]bool{}}
	for _, m := range []moment{at, day, {at: midnight.Add(36 * time.Hour)}} {
		d := m.at
		e := model.IndexCell{D: &d}
		if m.day {
			s := d.Format(dayLayout)
			e.T = &s
		}
		f.add(famDate, e, true)
	}
	f.unique = len(f.seen)
	got, err := summariesOf([]query.SummaryRequest{{Field: "d", Fns: []string{"earliest", "latest", "range", "unique"}}}, 3, map[string]*totals{"d": &f.totals})
	if err != nil {
		t.Fatal(err)
	}
	if g := written(t, got, "d"); g != `{"earliest":"2026-10-01","latest":"2026-10-02T12:00:00Z","range":["2026-10-01","2026-10-02T12:00:00Z"],"unique":3}` {
		t.Errorf("got %s", g)
	}
}

// The median is the middle value, or between the middle two as Postgres's
// percentile_cont(0.5) takes it.
func TestTheMedianIsTheMiddle(t *testing.T) {
	for _, c := range []struct {
		nums []float64
		want *float64
	}{
		{nil, nil},
		{[]float64{7}, ptr(7)},
		{[]float64{30.5, -50, 120}, ptr(30.5)},
		{[]float64{120, 0, -50, 30.5}, ptr(15.25)},
	} {
		got := medianOf(c.nums)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%v: got %v, want %v", c.nums, got, c.want)
		}
	}
}

// What each kind of column can be summed up by; a many-valued field's
// different sets come with its groups, but who made a row is one person.
func TestEachKindHasItsOwnFunctions(t *testing.T) {
	for _, c := range []struct {
		fn   string
		col  *column
		want bool
	}{
		{"pct_filled", &column{family: famMulti}, true},
		{"unique", &column{family: famText}, true},
		{"unique", &column{family: famMulti}, false},
		{"unique", &column{family: famPeople}, false},
		{"unique", &column{family: famRowLink}, false},
		{"unique", &column{family: famPeople, col: colCreatedBy}, true},
		{"sum", &column{family: famNumber, col: colRowNo}, true},
		{"sum", &column{family: famText}, false},
		{"range", &column{family: famDate}, true},
		{"range", &column{family: famChoice}, false},
		{"checked", &column{family: famCheckbox}, true},
		{"checked", &column{family: famNumber}, false},
		{"earliest", &column{family: famDate, col: colCreatedAt}, true},
		{"none", &column{family: famText}, false},
	} {
		if got := fits(c.fn, c.col, c.col.family); got != c.want {
			t.Errorf("%s over family %d (%q): got %v, want %v", c.fn, c.col.family, c.col.col, got, c.want)
		}
	}
}

func ptr(f float64) *float64 { return &f }
