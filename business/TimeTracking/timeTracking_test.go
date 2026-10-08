package business

import (
	"errors"
	"strings"
	"testing"
	"time"

	timeModel "github.com/akashc777/OneCamp/models/postgres/TimeEntry"
	"github.com/google/uuid"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func TestCheckSpan(t *testing.T) {
	start, end, note, err := CheckSpan(now.Add(-2*time.Hour), 90, "  design   review ", now)
	if err != nil || end.Sub(start) != 90*time.Minute || note != "design review" {
		t.Fatalf("got %v %v %q %v", start, end, note, err)
	}
	var in *InputError
	for name, c := range map[string]struct {
		start   time.Time
		minutes int
		note    string
	}{
		"no start":   {time.Time{}, 30, ""},
		"zero":       {now.Add(-time.Hour), 0, ""},
		"over a day": {now.Add(-48 * time.Hour), 24*60 + 1, ""},
		"future":     {now.Add(-10 * time.Minute), 60, ""},
		"long note":  {now.Add(-time.Hour), 30, strings.Repeat("x", 501)},
	} {
		if _, _, _, err := CheckSpan(c.start, c.minutes, c.note, now); !errors.As(err, &in) {
			t.Errorf("%s: want an input error, got %v", name, err)
		}
	}
}

func TestReportRange(t *testing.T) {
	from, to, err := ReportRange("", "", now)
	if err != nil || from != time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) || !to.After(now) {
		t.Fatalf("default range: %v %v %v", from, to, err)
	}
	if _, _, err := ReportRange("2026-10-05T00:00:00Z", "2026-10-01T00:00:00Z", now); err == nil {
		t.Error("an end before the start was accepted")
	}
	if _, _, err := ReportRange("2024-01-01T00:00:00Z", "2026-01-01T00:00:00Z", now); err == nil {
		t.Error("two years was accepted")
	}
	if _, _, err := ReportRange("yesterday", "", now); err == nil {
		t.Error("an unreadable date was accepted")
	}
}

func TestSummarise(t *testing.T) {
	maya, sam := uuid.New(), uuid.New()
	design, build, gone := uuid.New(), uuid.New(), uuid.New()
	ended := func(h float64) *time.Time { t := now.Add(-time.Duration(h * float64(time.Hour))); return &t }
	entries := []timeModel.Entry{
		{UserID: maya, TaskUUID: design, StartedAt: now.Add(-5 * time.Hour), EndedAt: ended(3), Billable: true},   // 2h
		{UserID: maya, TaskUUID: build, StartedAt: now.Add(-3 * time.Hour), EndedAt: ended(2.5), Billable: false}, // 30m
		{UserID: sam, TaskUUID: gone, StartedAt: now.Add(-1 * time.Hour), Billable: true},                         // running, 1h
	}
	names := Names{People: map[uuid.UUID]string{maya: "Maya", sam: "Sam"}, Tasks: map[uuid.UUID]string{design: "Design", build: "Build"}}
	r := Summarise(entries, names, now.Add(-24*time.Hour), now, now, nil)
	if r.Seconds != 3*3600+1800 || r.BillableSeconds != 3*3600 || r.Running != 1 || r.Entries != 3 {
		t.Fatalf("totals: %+v", r)
	}
	if r.ByPerson[0].Name != "Maya" || r.ByPerson[0].Seconds != 9000 || r.ByPerson[0].BillableSeconds != 7200 {
		t.Errorf("by person: %+v", r.ByPerson)
	}
	if r.ByTask[0].Name != "Design" || r.ByTask[1].Name != "Deleted task" {
		t.Errorf("by task, most time first, with a fallback name: %+v", r.ByTask)
	}
	if r.AmountCents != nil || r.ByPerson[0].AmountCents != nil {
		t.Errorf("without rates, no money: %+v", r)
	}

	// Priced: Maya at her own 90.00 an hour, Sam at the project's 50.00; the
	// unbillable half hour comes to nothing.
	rates := &Rates{Currency: "USD", Default: 5000, People: map[uuid.UUID]int64{maya: 9000}}
	r = Summarise(entries, names, now.Add(-24*time.Hour), now, now, rates)
	if r.Currency != "USD" || r.AmountCents == nil || *r.AmountCents != 2*9000+5000 {
		t.Fatalf("the total is what the billable time comes to: %+v", r.AmountCents)
	}
	byName := map[string]Line{}
	for _, l := range append(r.ByPerson, r.ByTask...) {
		byName[l.Name] = l
	}
	if *byName["Maya"].AmountCents != 18000 || *byName["Maya"].RateCents != 9000 || *byName["Sam"].AmountCents != 5000 || *byName["Sam"].RateCents != 5000 {
		t.Errorf("per person, at their rate: %+v %+v", byName["Maya"], byName["Sam"])
	}
	if *byName["Design"].AmountCents != 18000 || *byName["Build"].AmountCents != 0 || byName["Design"].RateCents != nil {
		t.Errorf("per task, at the rate of whoever did the work: %+v %+v", byName["Design"], byName["Build"])
	}
	if d := byName["Design"].Rated; len(d) != 1 || d[0].RateCents != 9000 || d[0].BillableSeconds != 7200 {
		t.Errorf("a task's billable time at each rate, for the invoice: %+v", d)
	}
	if len(byName["Build"].Rated) != 0 {
		t.Errorf("unbillable time has no rate part: %+v", byName["Build"].Rated)
	}
	sum := func(ls []Line) (n int64) {
		for _, l := range ls {
			n += *l.AmountCents
		}
		return n
	}
	if sum(r.ByPerson) != *r.AmountCents || sum(r.ByTask) != *r.AmountCents {
		t.Errorf("by person and by task add up to the same total")
	}
}

func TestAmountRoundsEachEntry(t *testing.T) {
	// 20 minutes at 100.00 an hour is 33.33; a third of a cent rounds away once.
	if got := amount(1200, 10000); got != 3333 {
		t.Errorf("amount = %d", got)
	}
	if got := amount(1800, 1); got != 1 { // half a cent rounds up
		t.Errorf("amount = %d", got)
	}
	if Money(123456) != "1234.56" || Money(5) != "0.05" || Money(-250) != "-2.50" {
		t.Errorf("money: %s %s %s", Money(123456), Money(5), Money(-250))
	}
}

func TestCheckRates(t *testing.T) {
	in := RatesInput{Currency: " usd ", DefaultRateCents: 5000}
	in.People = append(in.People, struct {
		UserUUID  string `json:"user_uuid"`
		RateCents int64  `json:"rate_cents"`
	}{UserUUID: uuid.NewString(), RateCents: 9000})
	r, err := CheckRates(in)
	if err != nil || r.Currency != "USD" || r.Default != 5000 || len(r.People) != 1 {
		t.Fatalf("rates: %+v %v", r, err)
	}
	for _, bad := range []RatesInput{{Currency: "US"}, {Currency: "U$D"}, {Currency: "USD", DefaultRateCents: -1}, {Currency: "USD", DefaultRateCents: maxRateCents + 1}} {
		if _, err := CheckRates(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestWriteCSVNeutralisesFormulas(t *testing.T) {
	maya, task := uuid.New(), uuid.New()
	end := now.Add(-time.Hour)
	var b strings.Builder
	err := WriteCSV(&b, []timeModel.Entry{{UserID: maya, TaskUUID: task, StartedAt: now.Add(-2 * time.Hour), EndedAt: &end, Billable: true, Note: "=HYPERLINK(\"x\")"}},
		Names{People: map[uuid.UUID]string{maya: "Maya"}, Tasks: map[uuid.UUID]string{task: "+Launch"}}, time.UTC, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "'+Launch") || !strings.Contains(out, "'=HYPERLINK") || !strings.Contains(out, ",1.00,yes,") {
		t.Fatalf("got %s", out)
	}
}
