package business

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// A date's group is the first day of its period where the reader is, as
// Postgres's date_trunc finds it: a day by its own date wherever they are, a
// moment by theirs, a week from its Monday.
func TestADateGroupsByThePeriodItIsInWhereTheReaderIs(t *testing.T) {
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	newYork, _ := time.LoadLocation("America/New_York")
	day := func(s string) model.IndexCell { return model.IndexCell{T: &s} }
	moment := func(s string) model.IndexCell {
		at, _ := time.Parse(time.RFC3339, s)
		return model.IndexCell{D: &at}
	}
	for _, c := range []struct {
		entry  model.IndexCell
		period string
		zone   *time.Location
		want   string
	}{
		{day("2026-10-18"), query.GranularityDay, newYork, "2026-10-18"},
		{day("2026-10-18"), query.GranularityWeek, kolkata, "2026-10-12"}, // a Sunday
		{day("2026-10-19"), query.GranularityWeek, kolkata, "2026-10-19"}, // a Monday
		{moment("2026-10-18T20:00:00Z"), query.GranularityDay, kolkata, "2026-10-19"},
		{moment("2026-10-18T20:00:00Z"), query.GranularityWeek, kolkata, "2026-10-19"},
		{moment("2026-10-18T20:00:00Z"), query.GranularityWeek, time.UTC, "2026-10-12"},
		{moment("2026-11-01T02:00:00Z"), query.GranularityMonth, newYork, "2026-10-01"},
		{moment("2026-12-31T20:00:00Z"), query.GranularityYear, kolkata, "2027-01-01"},
		{day("2026-08-31"), query.GranularityQuarter, kolkata, "2026-07-01"},
		{day("2026-12-31"), query.GranularityQuarter, kolkata, "2026-10-01"},
		{day("2026-01-01"), query.GranularityWeek, kolkata, "2025-12-29"},
	} {
		if got := periodOf(c.entry, c.period, c.zone); got != c.want {
			t.Errorf("%v by %s in %s: %s, want %s", c.entry, c.period, c.zone, got, c.want)
		}
	}
}

// A number's key has no exponent and no negative zero, as Postgres's numeric
// writes it; a collapsed group's key is read the one way keys are written,
// and a key no row can have hides nothing.
func TestAGroupsKeyIsWrittenOneWay(t *testing.T) {
	for n, want := range map[float64]string{30.5: "30.5", -50: "-50", 1e21: "1000000000000000000000", 1e-7: "0.0000001"} {
		if got := numberKey(n); got != want {
			t.Errorf("%v: %s, want %s", n, got, want)
		}
	}
	if got := numberKey(math.Copysign(0, -1)); got != "0" {
		t.Errorf("-0: %s, want 0", got)
	}
	for _, c := range []struct {
		fam       family
		key, want string
		ok        bool
	}{
		{famNumber, "30.50", "30.5", true},
		{famNumber, "", "", true},
		{famNumber, "lots", "", false},
		{famNumber, "Inf", "", false},
		{famCheckbox, "true", "true", true},
		{famCheckbox, "TRUE", "", false},
		{famCheckbox, "", "", false},
		{famChoice, "Won", "Won", true},
		{famDate, "2026-10-01", "2026-10-01", true},
	} {
		if _, _, got, ok := keyValue(c.fam, c.key); got != c.want || ok != c.ok {
			t.Errorf("%q: %q %v, want %q %v", c.key, got, ok, c.want, c.ok)
		}
	}
}

// Rows in their groups' order fall into every level's groups, depth first,
// each counted once a level; a collapsed group hides its rows at any depth.
func TestTheEvaluatorsGroupsAreEveryLevelsDepthFirst(t *testing.T) {
	rows := make([]*evalRow, 5)
	for i := range rows {
		rows[i] = &evalRow{id: uuid.New()}
	}
	paths := [][]string{{"Won", "true"}, {"Won", "true"}, {"Won", "false"}, {"Lost", "false"}, {"", "false"}}
	groups, err := foldGroups(nil, nil, nil, rows, paths)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range groups {
		got = append(got, strings.Join(g.Key, "/")+"="+strconv.Itoa(g.Count))
	}
	if want := "Won=3 Won/true=2 Won/false=1 Lost=1 Lost/false=1 =1 /false=1"; strings.Join(got, " ") != want {
		t.Errorf("groups %s, want %s", strings.Join(got, " "), want)
	}
	collapsed := collapsedOf([]query.Group{{Field: "s", Collapsed: []string{`["Lost"]`}}, {Field: "w", Collapsed: []string{`["Won","false"]`}}},
		map[string]*column{"s": {family: famChoice}, "w": {family: famCheckbox}}, nil)
	for i, want := range []bool{false, false, true, true, false} {
		if hidden(paths[i], collapsed) != want {
			t.Errorf("%v hidden: %v, want %v", paths[i], !want, want)
		}
	}
	// One group past the cap is refused.
	many := make([][]string, query.MaxGroupCounts+1)
	manyRows := make([]*evalRow, len(many))
	for i := range many {
		many[i], manyRows[i] = []string{numberKey(float64(i))}, rows[0]
	}
	if _, err := foldGroups(nil, nil, nil, manyRows, many); err == nil {
		t.Errorf("%d groups answered, want refused", len(many))
	}
}
