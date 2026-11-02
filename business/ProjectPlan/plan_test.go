package business

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	model "github.com/akashc777/OneCamp/models/postgres/ProjectPlan"
)

func day(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

// A start keeps its period's first day and a target its last, at every
// resolution, across a year's end and a leap February.
func TestPeriodIsTheFirstAndLastDayAtEachResolution(t *testing.T) {
	for _, c := range []struct{ on, res, first, last string }{
		{"2027-02-14", Day, "2027-02-14", "2027-02-14"},
		{"2028-02-14", Month, "2028-02-01", "2028-02-29"},
		{"2027-02-14", Month, "2027-02-01", "2027-02-28"},
		{"2027-03-31", Quarter, "2027-01-01", "2027-03-31"},
		{"2027-11-02", Quarter, "2027-10-01", "2027-12-31"},
		{"2027-06-30", Half, "2027-01-01", "2027-06-30"},
		{"2027-07-01", Half, "2027-07-01", "2027-12-31"},
		{"2027-07-01", Year, "2027-01-01", "2027-12-31"},
	} {
		first, last := Period(day(c.on), c.res)
		if first.Format("2006-01-02") != c.first || last.Format("2006-01-02") != c.last {
			t.Errorf("%s at %s: %s to %s, want %s to %s", c.on, c.res, first.Format("2006-01-02"), last.Format("2006-01-02"), c.first, c.last)
		}
	}
}

// A target "Q1 2027" is kept as 31 March and a start "Q1 2027" as 1 January;
// null clears a date; a resolution or a day that isn't one is refused in
// words.
func TestADateSnapsToItsPeriodAndBadOnesAreRefused(t *testing.T) {
	q1 := json.RawMessage(`{"date":"2027-02-10","resolution":"quarter"}`)
	if d, r, err := parseWhen(q1, true); err != nil || d.Format("2006-01-02") != "2027-03-31" || r != Quarter {
		t.Fatalf("a target in Q1: %v %q %v, want 2027-03-31 quarter", d, r, err)
	}
	if d, _, err := parseWhen(q1, false); err != nil || d.Format("2006-01-02") != "2027-01-01" {
		t.Fatalf("a start in Q1: %v %v, want 2027-01-01", d, err)
	}
	if d, r, err := parseWhen(json.RawMessage(`null`), true); err != nil || d != nil || r != "" {
		t.Fatalf("null clears a date: %v %q %v", d, r, err)
	}
	for _, bad := range []string{`{"date":"2027-02-10","resolution":"week"}`, `{"date":"10/02/2027","resolution":"day"}`, `"2027-02-10"`} {
		if _, _, err := parseWhen(json.RawMessage(bad), true); err == nil {
			t.Errorf("%s was taken", bad)
		} else if _, ok := err.(Refusal); !ok {
			t.Errorf("%s: %v isn't said in words", bad, err)
		}
	}
}

// Choosing the lead asks for the project's admins; status, priority, dates
// and labels ask for whoever posts its updates.
func TestChoosingTheLeadIsTheAdminsAndTheRestIsWhoeverPostsUpdates(t *testing.T) {
	cases := []struct {
		body string
		need workAccess.Need
	}{
		{`{"lead_uuid":"6f1c2c4e-0000-4000-8000-000000000001"}`, workAccess.NeedManage},
		{`{"lead_uuid":null}`, workAccess.NeedManage},
		{`{"status":"paused","lead_uuid":null}`, workAccess.NeedManage},
		{`{"status":"paused"}`, workAccess.NeedPostUpdate},
		{`{"priority":"high","labels":[]}`, workAccess.NeedPostUpdate},
		{`{"target":{"date":"2027-03-31","resolution":"quarter"}}`, workAccess.NeedPostUpdate},
		{`{"new_labels":[{"name":"Customer"}]}`, workAccess.NeedPostUpdate},
	}
	for _, c := range cases {
		var ch Change
		if err := json.Unmarshal([]byte(c.body), &ch); err != nil {
			t.Fatal(err)
		}
		if got := ch.Need(); got != c.need {
			t.Errorf("%s asks for %v, want %v", c.body, got, c.need)
		}
	}
}

// A name is trimmed with its spaces folded and takes 1 to 40 characters; a
// colour is one of the palette's, slate when none is given.
func TestNamesAndColoursAreKeptToTheirRules(t *testing.T) {
	if n, err := name("  In   review "); err != nil || n != "In review" {
		t.Fatalf("name: %q %v", n, err)
	}
	for _, bad := range []string{"", "   ", string(make([]rune, 41))} {
		if _, err := name(bad); err == nil {
			t.Errorf("%q was taken as a name", bad)
		}
	}
	if c, err := color(""); err != nil || c != "slate" {
		t.Fatalf("no colour: %q %v, want slate", c, err)
	}
	if _, err := color("chartreuse"); err == nil {
		t.Fatal("a colour outside the palette was taken")
	}
	if !builtInNamed("in PROGRESS") || builtInNamed("In review") {
		t.Fatal("a built-in status's name is matched whatever its case, and only theirs")
	}
}

func TestARegistryRefusalNamesWhatTheWorkspaceHoldsAndItsMost(t *testing.T) {
	for _, c := range []struct {
		err             error
		what, many, out string
		most            int
	}{
		{model.ErrFull, "label", "labels", "The workspace has 300 labels, as many as it may. Delete one first.", MostLabels},
		{model.ErrFull, "status", "statuses", "The workspace has 50 statuses, as many as it may. Delete one first.", MostStatuses},
		{model.ErrDuplicate, "label", "labels", "There's a label with that name already.", MostLabels},
	} {
		var r Refusal
		if err := duplicate(c.err, c.what, c.many, c.most); !errors.As(err, &r) || string(r) != c.out {
			t.Fatalf("%v for a %s: %v, want %q", c.err, c.what, err, c.out)
		}
	}
	other := errors.New("the database is away")
	if err := duplicate(other, "label", "labels", MostLabels); err != other {
		t.Fatalf("an error that is not a refusal came back as %v", err)
	}
}
