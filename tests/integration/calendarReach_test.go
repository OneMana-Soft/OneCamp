//go:build integration
// +build integration

package integration_test

// Search's calendar reach (H6 A1-11): which calendars a person finds events
// on (calendarBusiness.SearchCalendars) is read from Postgres once and kept
// for their next search, until a write to calendars or calendar_shares moves
// calendarModels.ReachWrites (or its max age passes). Each count is the
// statements on the call's goroutine and on any other it started (pgCalls),
// so work moved onto a goroutine is not a saving. Every event a search finds
// is still decided fresh, so a removed guest loses it at once.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	calendarModels "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

type calReachWorld struct {
	*refsWorld
	taps *integration.StoreTaps
}

func newCalReachWorld(t *testing.T) *calReachWorld {
	t.Helper()
	w := newRefsWorld(t)
	integration.MarkCalendarMoved(t, w.env.PG)
	integration.SetupRedis(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	return &calReachWorld{refsWorld: w, taps: integration.TapStores(t, w.env, w.dg, search)}
}

func (r *calReachWorld) user(name string) *userModels.UserInfo {
	u := r.as(name).Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return &u
}

// personal is name's own calendar, made the first time (the calendar list).
func (r *calReachWorld) personal(t *testing.T, name string) uuid.UUID {
	t.Helper()
	list, err := calendarBusiness.Events.Calendars(r.as(name), r.user(name))
	if err != nil {
		t.Fatalf("%s's calendars: %v", name, err)
	}
	for _, c := range list {
		if c.Mine && c.Kind == "personal" {
			return uuid.MustParse(c.ID)
		}
	}
	t.Fatalf("%s has no personal calendar in %v", name, list)
	return uuid.Nil
}

// reach is the calendars name finds events on (every one, either list) and
// the Postgres statements reading them took, sync and async.
func (r *calReachWorld) reach(t *testing.T, name string) ([]string, int, []string) {
	t.Helper()
	var all, open []string
	var err error
	w, merr := r.taps.Measure(func() {
		all, open, err = calendarBusiness.SearchCalendars(r.as(name), accessBusiness.SubjectOf(r.user(name)))
	})
	if merr != nil || err != nil {
		t.Fatalf("%s's reach: %v %v", name, merr, err)
	}
	got := append(append([]string{}, all...), open...)
	slices.Sort(got)
	return got, pgCalls(w), append(w.SyncCalls, w.AsyncCalls...)
}

// calendarStatements are the counted Postgres statements in w whose recorded
// text is one of reads, the calls of a cold reach. The taps keep a
// statement's first 90 bytes, and the reach's SELECTs name their columns for
// longer than that, so a table name in the text would match none of them.
func calendarStatements(w integration.Window, reads []string) []string {
	var out []string
	for _, c := range append(append([]string{}, w.SyncCalls...), w.AsyncCalls...) {
		if strings.HasPrefix(c, "pg ") && slices.Contains(reads, c) {
			out = append(out, c)
		}
	}
	return out
}

// TestASecondSearchReadsNoCalendarReach: the calendars a person finds events
// on are read once; their next search, and the next reach, read no calendar
// row from Postgres (H6 A1-11), and find the same calendars. The whole
// search counts both sides: what the reach no longer reads is not moved onto
// another goroutine.
func TestASecondSearchReadsNoCalendarReach(t *testing.T) {
	r := newCalReachWorld(t)
	olu, out := r.personal(t, "olu"), r.personal(t, "out")
	if err := calendarBusiness.Events.Share(r.as("olu"), r.user("olu"), olu.String(), r.people["out"].String(), calendarModels.ShareDetails); err != nil {
		t.Fatal(err)
	}
	want := []string{olu.String(), out.String()}
	slices.Sort(want)

	first, n1, cold := r.reach(t, "out")
	if !slices.Equal(first, want) || n1 == 0 {
		t.Fatalf("out's first reach: %v in %d statements; want %v read from Postgres", first, n1, want)
	}
	second, n2, calls := r.reach(t, "out")
	if !slices.Equal(second, want) {
		t.Errorf("out's second reach: %v; want %v", second, want)
	}
	if n2 != 0 {
		t.Errorf("out's second reach took %d statements (%q); want 0", n2, calls)
	}

	// The whole search, for someone whose reach is not read yet: the second
	// runs none of the statements out's cold reach ran, and its total falls
	// by what the first ran of them.
	mia := r.user("mia")
	r.personal(t, "mia")
	search := func() integration.Window {
		t.Helper()
		var err error
		w, merr := r.taps.Measure(func() {
			_, err = globalSearchBusiness.GetUnifiedGlobalSearch(r.as("mia"), accessBusiness.SubjectOf(mia), mia.UserPostgresInfo.EmailID, nil, nil, nil, "reach nothing matches")
		})
		if merr != nil || err != nil {
			t.Fatalf("mia's search: %v %v", merr, err)
		}
		return w
	}
	s1 := search()
	s2 := search()
	read := calendarStatements(s1, cold)
	if len(read) == 0 {
		t.Fatalf("mia's first search read no calendar row: the case does not reach the calendar")
	}
	if again := calendarStatements(s2, cold); len(again) != 0 {
		t.Errorf("mia's second search read calendars again: %q", again)
	}
	if pgCalls(s2) > pgCalls(s1)-len(read) {
		t.Errorf("mia's second search took %d statements (sync %d + async %d); want at most %d (the first's %d less its %d calendar reads)",
			pgCalls(s2), s2.Sync[integration.StorePostgres], s2.Async[integration.StorePostgres], pgCalls(s1)-len(read), pgCalls(s1), len(read))
	}
}

// TestEveryCalendarWriterRefreshesSearchReach: every write to calendars or
// calendar_shares (WRITERS.txt: the eight in models/postgres/CalendarV2)
// moves ReachWrites, and the next search reach reads Postgres again and
// shows the change at once: a calendar unshared, unsubscribed or taken back
// is no longer searched, one made or shared is.
func TestEveryCalendarWriterRefreshesSearchReach(t *testing.T) {
	ctx := context.Background()
	r := newCalReachWorld(t)
	olu := r.personal(t, "olu")
	r.personal(t, "out")
	oluID, outID := r.people["olu"], r.people["out"]
	subscribed, takenBack := uuid.New(), uuid.New()
	mias, imported := uuid.Nil, uuid.Nil

	cases := []struct {
		name   string
		reader string
		write  func(t *testing.T) error
		finds  func() string // the calendar the reader finds after it ("" when unchanged)
		loses  func() string // the one they no longer find ("" when none)
	}{
		{"PersonalCalendar", "mia", func(t *testing.T) error {
			id, err := calendarModels.PersonalCalendar(ctx, calendarModels.Pool(), r.people["mia"], uuid.New(), "UTC")
			mias = id
			return err
		}, func() string { return mias.String() }, nil},
		{"ImportedCalendar", "olu", func(t *testing.T) error {
			id, err := calendarModels.ImportedCalendar(ctx, oluID, "Imported", uuid.New(), "UTC")
			imported = id
			return err
		}, func() string { return imported.String() }, nil},
		{"ContainerCalendar", "olu", func(t *testing.T) error {
			_, err := calendarModels.ContainerCalendar(ctx, "project", uuid.NewString(), uuid.New(), "UTC")
			return err
		}, nil, nil},
		{"SetShare share", "out", func(t *testing.T) error {
			return calendarModels.SetShare(ctx, olu, outID, oluID, calendarModels.ShareDetails)
		}, func() string { return olu.String() }, nil},
		{"SetShare unshare", "out", func(t *testing.T) error {
			return calendarModels.SetShare(ctx, olu, outID, oluID, "")
		}, nil, func() string { return olu.String() }},
		{"UpdateCalendar", "olu", func(t *testing.T) error {
			name := "Imported, renamed"
			return calendarModels.UpdateCalendar(ctx, imported, &name, nil)
		}, nil, nil},
		{"SubscribedCalendar", "olu", func(t *testing.T) error {
			if err := calendarModels.SubscribedCalendar(ctx, takenBack, oluID, "Taken back", "", "UTC"); err != nil {
				return err
			}
			return calendarModels.SubscribedCalendar(ctx, subscribed, oluID, "Holidays", "", "UTC")
		}, func() string { return subscribed.String() }, nil},
		{"UnsubscribeCalendar", "olu", func(t *testing.T) error {
			_, err := calendarModels.UnsubscribeCalendar(ctx, subscribed, oluID)
			return err
		}, nil, func() string { return subscribed.String() }},
		{"TakeBackSubscribedCalendar", "olu", func(t *testing.T) error {
			_, err := calendarModels.TakeBackSubscribedCalendar(ctx, takenBack, oluID)
			return err
		}, nil, func() string { return takenBack.String() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r.reach(t, c.reader)
			if _, n, calls := r.reach(t, c.reader); n != 0 {
				t.Errorf("%s's reach, read just before, took %d statements (%q); want 0 (kept)", c.reader, n, calls)
			}
			was := calendarModels.ReachWrites()
			if err := c.write(t); err != nil {
				t.Fatalf("the write: %v", err)
			}
			if now := calendarModels.ReachWrites(); now == was {
				t.Errorf("ReachWrites stayed %d after the write", now)
			}
			got, n, _ := r.reach(t, c.reader)
			if n == 0 {
				t.Errorf("%s's reach after the write read nothing from Postgres: it kept what the write changed", c.reader)
			}
			if c.finds != nil && !slices.Contains(got, c.finds()) {
				t.Errorf("%s's reach %v lacks %s, just written", c.reader, got, c.finds())
			}
			if c.loses != nil && slices.Contains(got, c.loses()) {
				t.Errorf("%s's reach %v still has %s", c.reader, got, c.loses())
			}
		})
	}
}

// TestARemovedGuestNoLongerOpensASearchedEvent: whatever the reach keeps,
// each event a search finds is decided fresh (readableEvents: the Access
// Resolver at view), so a guest removed from an event loses it at once, on
// a calendar they do not otherwise see.
func TestARemovedGuestNoLongerOpensASearchedEvent(t *testing.T) {
	r := newCalReachWorld(t)
	r.personal(t, "olu")
	start := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	ev, err := calendarBusiness.Events.Create(r.as("olu"), r.user("olu"), calendarBusiness.EventInput{Title: "Reach review",
		Start: calendarBusiness.When{At: &start, Zone: "UTC"}, End: calendarBusiness.When{At: &end, Zone: "UTC"},
		Attendees: []calendarBusiness.GuestInput{{UUID: r.people["mia"].String()}}})
	if err != nil {
		t.Fatal(err)
	}
	opens := func() bool {
		t.Helper()
		r.reach(t, "mia") // kept between the two decisions
		open, err := accessBusiness.Rules.Filter(r.as("mia"), accessBusiness.SubjectOf(r.user("mia")),
			[]accessBusiness.Resource{{Kind: calendarBusiness.KindEvent, UUID: ev.ID}}, accessBusiness.LevelView)
		if err != nil {
			t.Fatal(err)
		}
		return len(open) == 1
	}
	if !opens() {
		t.Fatal("mia, a guest, cannot open the event")
	}
	none := []calendarBusiness.GuestInput{}
	seq := ev.Sequence
	if _, err := calendarBusiness.Events.Update(r.as("olu"), r.user("olu"), ev.ID, calendarBusiness.EventPatch{Sequence: &seq, Attendees: &none}); err != nil {
		t.Fatal(err)
	}
	if opens() {
		t.Error("mia, removed from the event, still opens it")
	}
}

// TestAShareRevokedElsewhereShowsNoEventInSearch (CODE-REVIEWER-407's probe,
// CR-407 F1): a share revoked on another instance moves no ReachWrites here,
// so out's kept reach still holds olu's calendar for up to its max age. Each
// event a search finds is still decided fresh (keepReadableEventHits ->
// Rules.Filter), so out's whole search (Ask AI's path too) and the event's
// own decision leave olu's event out at once.
func TestAShareRevokedElsewhereShowsNoEventInSearch(t *testing.T) {
	ctx := context.Background()
	r := newCalReachWorld(t)
	olu := r.personal(t, "olu")
	r.personal(t, "out")
	oluID, outID := r.people["olu"], r.people["out"]
	if err := calendarModels.SetShare(ctx, olu, outID, oluID, calendarModels.ShareDetails); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	ev, err := calendarBusiness.Events.Create(r.as("olu"), r.user("olu"), calendarBusiness.EventInput{Title: "Zanzibar probe review",
		Start: calendarBusiness.When{At: &start, Zone: "UTC"}, End: calendarBusiness.When{At: &end, Zone: "UTC"}})
	if err != nil {
		t.Fatal(err)
	}
	helpers.SettleBackground(30 * time.Second)
	r.settleOutbox()
	if err := opensearchInit.RefreshIndices(ctx, openSearchStruct.EVENT_INDEX); err != nil {
		t.Fatal(err)
	}
	search := func() (shown, total int) {
		t.Helper()
		u := r.user("out")
		p, err := globalSearchBusiness.GetUnifiedGlobalSearch(r.as("out"), accessBusiness.SubjectOf(u), u.UserPostgresInfo.EmailID, nil, nil, nil, "Zanzibar")
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range p.Page {
			if h.Event != nil && h.Event.Uuid == ev.ID {
				shown++
			}
		}
		return shown, p.Totals[globalSearchBusiness.KindEvent]
	}
	if s, n := search(); s != 1 {
		t.Fatalf("precondition: out, shared olu's calendar, finds the event %d times (total %d); want 1", s, n)
	}
	if got, _, _ := r.reach(t, "out"); !slices.Contains(got, olu.String()) {
		t.Fatalf("precondition: out's reach %v lacks olu's calendar", got)
	}
	was := calendarModels.ReachWrites()
	if _, err := r.env.PG.Exec(`UPDATE calendar_shares SET deleted_at = NOW() WHERE calendar_id = $1 AND person_id = $2 AND deleted_at IS NULL`, olu, outID); err != nil {
		t.Fatal(err)
	}
	if calendarModels.ReachWrites() != was {
		t.Fatal("the SQL revoke moved ReachWrites")
	}
	got, pg, _ := r.reach(t, "out")
	stale := slices.Contains(got, olu.String())
	t.Logf("after the revoke elsewhere: out's reach %v, %d pg statements, stale=%v", got, pg, stale)
	if !stale {
		t.Errorf("STALE PATH NOT EXERCISED: the reach dropped olu's calendar at once")
	}
	s, total := search()
	t.Logf("after the revoke elsewhere: out's search shows the event %d times; events total %d", s, total)
	if s != 0 {
		t.Errorf("EXPOSURE: out still finds olu's event in search after the share was revoked elsewhere")
	}
	open, err := accessBusiness.Rules.Filter(r.as("out"), accessBusiness.SubjectOf(r.user("out")),
		[]accessBusiness.Resource{{Kind: calendarBusiness.KindEvent, UUID: ev.ID}}, accessBusiness.LevelView)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("EXPOSURE: out still opens olu's event after the share was revoked elsewhere")
	}
}
