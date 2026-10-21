package business

import (
	"context"
	"strings"
	"testing"
	"time"

	viewModels "github.com/akashc777/OneCamp/models/postgres/ResourceView"
)

type dayRow struct{ kind, id, source, key, day string }

// fakes the stores for one test; answers what was recorded.
func fakeViews(t *testing.T, records bool) (*[]string, *[]dayRow) {
	t.Helper()
	prev := []any{now, recordsViews, recordView, recordDay, daySalt, pruneDays}
	byDay := recordsByDay
	recordsByDay = true // the per-day rules themselves, as beta runs them
	t.Cleanup(func() {
		recordsByDay = byDay
		now = prev[0].(func() time.Time)
		recordsViews = prev[1].(func(context.Context, string) bool)
		recordView = prev[2].(func(context.Context, string, string, string) error)
		recordDay = prev[3].(func(context.Context, string, string, string, string, time.Time) error)
		daySalt = prev[4].(func(context.Context, string) ([]byte, error))
		pruneDays = prev[5].(func(context.Context, time.Time, int) (int64, error))
	})
	viewed := &[]string{}
	days := &[]dayRow{}
	at := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	now = func() time.Time { return at }
	recordsViews = func(context.Context, string) bool { return records }
	recordView = func(_ context.Context, kind, id, user string) error {
		*viewed = append(*viewed, kind+":"+id+":"+user)
		return nil
	}
	recordDay = func(_ context.Context, kind, id, source, key string, day time.Time) error {
		*days = append(*days, dayRow{kind, id, source, key, day.UTC().Format("2006-01-02")})
		return nil
	}
	salts := map[string][]byte{}
	daySalt = func(_ context.Context, day string) ([]byte, error) {
		if salts[day] == nil {
			salts[day] = []byte("salt-of-" + day)
		}
		return salts[day], nil
	}
	return viewed, days
}

// A member's view is recorded in "Viewed by" and by day; one who chose
// "Don't record my views" is recorded in neither.
func TestAMembersViewIsRecordedUnlessTheyOptedOut(t *testing.T) {
	viewed, days := fakeViews(t, true)
	if err := RecordView(context.Background(), ResourceDoc, "d1", "u1"); err != nil {
		t.Fatal(err)
	}
	if len(*viewed) != 1 || len(*days) != 1 || (*days)[0] != (dayRow{"doc", "d1", viewModels.SourceMember, "u1", "2026-10-20"}) {
		t.Fatalf("viewed %v, days %v", *viewed, *days)
	}
	viewed, days = fakeViews(t, false)
	if err := RecordView(context.Background(), ResourceDoc, "d1", "u1"); err != nil || len(*viewed) != 0 || len(*days) != 0 {
		t.Fatalf("opted out: viewed %v, days %v, %v", *viewed, *days, err)
	}
}

// A visit with Global Privacy Control or Do Not Track, or from a bot, or with
// no browser named, is never counted.
func TestSomeVisitsAreNeverCounted(t *testing.T) {
	chrome := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36"
	for _, c := range []struct {
		v    Visit
		want bool
	}{
		{Visit{Address: "203.0.113.9", UserAgent: chrome}, true},
		{Visit{Address: "203.0.113.9", UserAgent: chrome, GPC: true}, false},
		{Visit{Address: "203.0.113.9", UserAgent: chrome, DNT: true}, false},
		{Visit{Address: "203.0.113.9", UserAgent: "Mozilla/5.0 (compatible; Googlebot/2.1)"}, false},
		{Visit{Address: "203.0.113.9", UserAgent: "Slackbot-LinkExpanding 1.0"}, false},
		{Visit{Address: "203.0.113.9", UserAgent: "facebookexternalhit/1.1"}, false},
		{Visit{Address: "203.0.113.9", UserAgent: "Mozilla/5.0 HeadlessChrome/140.0"}, false},
		{Visit{Address: "203.0.113.9", UserAgent: "curl/8.5.0"}, false},
		{Visit{Address: "203.0.113.9"}, false},
	} {
		if got := c.v.Counted(); got != c.want {
			t.Errorf("%+v: counted %v", c.v, got)
		}
	}
}

// A visitor's key is never their address, is the same all day, changes the
// next day, and differs between pages.
func TestAVisitorsKeyKeepsNoAddressAndChangesDaily(t *testing.T) {
	_, days := fakeViews(t, true)
	v := Visit{Address: "203.0.113.9", UserAgent: "Mozilla/5.0 Firefox/141.0"}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := CountPageVisit(ctx, ResourceDoc, "d1", "grant-a", v); err != nil {
			t.Fatal(err)
		}
	}
	if err := CountPageVisit(ctx, ResourceDoc, "d1", "grant-b", v); err != nil {
		t.Fatal(err)
	}
	next := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	now = func() time.Time { return next }
	if err := CountPageVisit(ctx, ResourceDoc, "d1", "grant-a", v); err != nil {
		t.Fatal(err)
	}
	r := *days
	if len(r) != 4 {
		t.Fatalf("rows %v", r)
	}
	for _, row := range r {
		if row.source != viewModels.SourcePage || strings.Contains(row.key, "203.0.113") || len(row.key) != 32 {
			t.Fatalf("a visitor's key: %+v", row)
		}
	}
	if r[0].key != r[1].key {
		t.Error("the same visitor, page and day keep one key")
	}
	if r[0].key == r[2].key {
		t.Error("another page gives another key")
	}
	if r[0].key == r[3].key || r[3].day != "2026-10-21" {
		t.Error("the next day gives another key")
	}
	if err := CountPageVisit(ctx, ResourceDoc, "d1", "grant-a", Visit{Address: "203.0.113.9", UserAgent: "Mozilla/5.0", GPC: true}); err != nil || len(*days) != 4 {
		t.Fatal("a GPC visit is not counted")
	}
}

// Rows past 400 days are swept.
func TestOldDaysAreSwept(t *testing.T) {
	fakeViews(t, true)
	var before time.Time
	pruneDays = func(_ context.Context, b time.Time, limit int) (int64, error) {
		before = b
		return 3, nil
	}
	if err := pruneOld(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := before.Format("2006-01-02"); got != "2025-09-15" {
		t.Fatalf("swept before %s", got)
	}
}

// Insights cover every day of 28, oldest first, members apart from visitors,
// and leave out what falls outside the window.
func TestInsightsLayOutEveryDay(t *testing.T) {
	fakeViews(t, true)
	from := windowStart(now())
	if got := from.Format("2006-01-02"); got != "2026-09-23" {
		t.Fatalf("window starts %s", got)
	}
	in := summarise(from, []viewModels.DayCount{
		{Day: "2026-09-22", Source: viewModels.SourceMember, Views: 9, Viewers: 9},
		{Day: "2026-09-23", Source: viewModels.SourceMember, Views: 3, Viewers: 2},
		{Day: "2026-10-20", Source: viewModels.SourceMember, Views: 1, Viewers: 1},
		{Day: "2026-10-20", Source: viewModels.SourcePage, Views: 7, Viewers: 4},
	})
	if len(in.Days) != InsightDays || in.Days[0].Day != "2026-09-23" || in.Days[27].Day != "2026-10-20" {
		t.Fatalf("days %v", in.Days)
	}
	if in.Days[0] != (DayPoint{Day: "2026-09-23", MemberViews: 3, Members: 2}) ||
		in.Days[27] != (DayPoint{Day: "2026-10-20", MemberViews: 1, Members: 1, PageViews: 7, Visitors: 4}) {
		t.Fatalf("first %+v, last %+v", in.Days[0], in.Days[27])
	}
	if in.MemberViews != 4 || in.PageViews != 7 || in.VisitorDays != 4 || in.Days[5] != (DayPoint{Day: in.Days[5].Day}) {
		t.Fatalf("totals %+v", in)
	}
}
