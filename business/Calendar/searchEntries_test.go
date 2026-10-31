package Calendar

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// searchWrites records the entries' bulk writes, and reads them into what
// they did to each event: its entry, or nil where its entry was deleted.
func searchWrites(t *testing.T) func() map[string]*openSearchStruct.OpenSearchEvent {
	t.Helper()
	var bodies []string
	prevWrite, prevConnected := writeSearch, searchConnected
	writeSearch = func(_ context.Context, body string) error {
		bodies = append(bodies, body)
		return nil
	}
	searchConnected = func() bool { return true }
	t.Cleanup(func() { writeSearch, searchConnected = prevWrite, prevConnected })
	return func() map[string]*openSearchStruct.OpenSearchEvent {
		t.Helper()
		out := map[string]*openSearchStruct.OpenSearchEvent{}
		for _, body := range bodies {
			lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
			for i := 0; i < len(lines); i++ {
				var action map[string]struct {
					Index string `json:"_index"`
					ID    string `json:"_id"`
				}
				if err := json.Unmarshal([]byte(lines[i]), &action); err != nil {
					t.Fatalf("line %d of a write isn't an action: %q", i, lines[i])
				}
				if a, ok := action["delete"]; ok && a.Index == openSearchStruct.EVENT_INDEX {
					out[a.ID] = nil
					continue
				}
				a, ok := action["index"]
				if !ok || a.Index != openSearchStruct.EVENT_INDEX || i+1 >= len(lines) {
					t.Fatalf("an unexpected action: %q", lines[i])
				}
				i++
				var entry openSearchStruct.OpenSearchEvent
				if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
					t.Fatal(err)
				}
				out[a.ID] = &entry
			}
		}
		return out
	}
}

// An event's entry is written whole from it: what it's called, its notes as
// text, where and when, who it is theirs to see, the calendar and project
// it's on and whether it's private, and never an address. Its guests' names
// are kept only while its guests may see one another, so a name never tells
// a guest who else is invited.
func TestAnEventsSearchEntryIsWrittenWholeFromIt(t *testing.T) {
	jonas, maya := uuid.New(), uuid.New()
	start := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	project := uuid.NewString()
	e := &models.Event{ID: uuid.New(), CalendarID: uuid.New(), Kind: string(EventPlain), Title: "Roadmap review",
		Notes: "<p>Bring the <strong>Q4</strong> plan</p>", NotesFormat: notesHTML, Location: "Room 4",
		StartAt: &start, EndAt: &end, TZ: "Europe/London", RRule: "FREQ=WEEKLY", CreatedBy: jonas,
		Visibility: visibilityPrivate, CreatedAt: start.Add(-48 * time.Hour), UpdatedAt: start.Add(-24 * time.Hour)}
	guests := []models.Attendee{{EventID: e.ID, UserID: &maya}, {EventID: e.ID, Email: "alex@example.com", Name: "Alex Doe"}}
	cal := models.Calendar{ID: e.CalendarID, ContainerKind: containerProject, ContainerID: project}
	people := map[string]*dgraphStruct.DgraphUser{
		jonas.String(): {Uuid: jonas.String(), UserName: "jonas", UserFullName: "Jonas Weber", EmailID: "jonas@example.com"},
		maya.String():  {Uuid: maya.String(), UserName: "maya", UserFullName: "Maya Chen", EmailID: "maya@example.com"},
	}

	got := searchEntryOf(e, guests, cal, people)
	want := openSearchStruct.OpenSearchEvent{Uuid: e.ID.String(), EventTitle: "Roadmap review", EventNotes: "Bring the Q4 plan",
		EventLocation: "Room 4", EventKind: "event", EventStart: start.Unix(), EventEnd: end.Unix(), EventZone: "Europe/London",
		EventRepeats: true, EventOrganiserUuid: jonas.String(), EventOrganiserFullName: people[jonas.String()].DisplayName(),
		EventPeople: []string{jonas.String(), maya.String()}, EventGuestNames: []string{},
		EventCalendarUuid: e.CalendarID.String(), EventProjectUuid: project, EventPrivate: true,
		EventCreatedAt: e.CreatedAt.Unix(), EventUpdatedAt: e.UpdatedAt.Unix()}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the entry\n  got  %+v\n  want %+v", got, want)
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "@") {
		t.Errorf("an entry carries an address: %s", raw)
	}

	e.GuestsCanSeeGuests = true
	if names := searchEntryOf(e, guests, cal, people).EventGuestNames; !slices.Equal(names, []string{people[maya.String()].DisplayName(), "Alex Doe"}) {
		t.Errorf("guests who see one another are found by their names %v", names)
	}

	// An all-day event on its organiser's own calendar.
	day := &models.Event{ID: uuid.New(), CalendarID: uuid.New(), Kind: string(EventAway), Title: "Off", StartDate: "2026-10-30",
		EndDate: "2026-10-31", TZ: "UTC", CreatedBy: jonas, CreatedAt: start, UpdatedAt: start}
	got = searchEntryOf(day, nil, models.Calendar{ID: day.CalendarID, Kind: "personal"}, people)
	if got.EventStart != 0 || got.EventStartDate != "2026-10-30" || got.EventEndDate != "2026-10-31" || got.EventProjectUuid != "" ||
		got.EventPrivate || got.EventRepeats || !slices.Equal(got.EventPeople, []string{jonas.String()}) {
		t.Errorf("an all-day event's entry %+v", got)
	}
}

// The entries are what the store holds when they're written: a series and a
// single event each get theirs whole, a series once; an instance changed on
// its own has its series' written and none of its own; an event in the
// trash, a task block, a day's working location and an id the store doesn't
// know have none. The nightly pass writes every series' and single event's
// entry, and passes over the kinds search leaves out.
func TestTheSearchEntriesAreWhatTheStoreHoldsNow(t *testing.T) {
	h := newHarness(t)
	written := searchWrites(t)
	ctx := context.Background()
	jonas, maya := h.member("Jonas"), h.member("Maya")
	create := func(title, rule string) uuid.UUID {
		t.Helper()
		start, end := timed(time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC), 30, "Europe/London")
		in := EventInput{Title: title, Start: start, End: end, Attendees: []GuestInput{{UUID: personOf(maya).String()}}}
		if rule != "" {
			in.Recurrence = &Recurrence{Rule: rule}
		}
		made, err := h.s.Create(ctx, jonas, in)
		if err != nil {
			t.Fatal(err)
		}
		return uuid.MustParse(made.ID)
	}
	series, single, trashed := create("Standup", "FREQ=DAILY"), create("Roadmap review", ""), create("Old sync", "")
	block, where := create("Write the plan", ""), create("Home", "")
	h.store.mu.Lock()
	h.store.events[block].Kind = string(EventTaskBlock)
	h.store.events[where].Kind = string(EventWorkingLocation)
	moved := copyEvent(h.store.events[series])
	rid := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	moved.ID, moved.SeriesID, moved.RecurrenceID, moved.RRule, moved.Title = uuid.New(), &series, &rid, "", "Standup, moved"
	h.store.events[moved.ID] = moved
	h.store.mu.Unlock()
	if err := h.s.Delete(ctx, jonas, trashed.String(), "", nil); err != nil {
		t.Fatal(err)
	}
	unknown := uuid.New()

	if err := h.s.writeSearchEntries(ctx, []uuid.UUID{single, moved.ID, trashed, block, where, unknown, single}); err != nil {
		t.Fatal(err)
	}
	got := written()
	for id, entry := range map[uuid.UUID]string{single: "Roadmap review", series: "Standup"} {
		if e := got[id.String()]; e == nil || e.EventTitle != entry || !slices.Contains(e.EventPeople, personOf(maya).String()) {
			t.Errorf("%s's entry %+v", entry, e)
		}
	}
	for _, id := range []uuid.UUID{moved.ID, trashed, block, where, unknown} {
		if e, ok := got[id.String()]; !ok || e != nil {
			t.Errorf("%s has an entry, or its entry wasn't deleted: %+v %v", id, e, ok)
		}
	}
	if len(got) != 7 {
		t.Errorf("wrote %d entries' changes, want 7 (each event once)", len(got))
	}

	// The pass: every series' and single event's entry, the trash's taken out,
	// and nothing of the kinds search leaves out.
	written = searchWrites(t)
	if err := h.s.writeEverySearchEntry(ctx); err != nil {
		t.Fatal(err)
	}
	got = written()
	if got[series.String()] == nil || got[single.String()] == nil {
		t.Errorf("the pass didn't write the series or the single event: %v", got)
	}
	if e, ok := got[trashed.String()]; !ok || e != nil {
		t.Errorf("the pass left the trashed event's entry: %+v %v", e, ok)
	}
	for _, id := range []uuid.UUID{block, where, moved.ID} {
		if _, ok := got[id.String()]; ok {
			t.Errorf("the pass touched %s, which has no entry", id)
		}
	}
}

// Search is asked for each event once, in outbox events of at most a hundred
// ids, keyed by their first: an instance's id is its series', an id in upper
// case is the same event, and what names no event is dropped. Nothing named,
// nothing asked.
func TestSearchIsAskedForEachEventOnceInPagesOfAHundred(t *testing.T) {
	var asked []outboxBusiness.Entry
	prev := enqueueSearch
	enqueueSearch = func(_ context.Context, es ...outboxBusiness.Entry) error {
		asked = append(asked, es...)
		return nil
	}
	t.Cleanup(func() { enqueueSearch = prev })

	var ids []string
	for range 250 {
		ids = append(ids, uuid.NewString())
	}
	named := append(append([]string{}, ids...), strings.ToUpper(ids[1]),
		instanceRef(uuid.MustParse(ids[0]), time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC), false), "not-an-event", "")
	SearchEntriesChanged(context.Background(), named)
	if len(asked) != 3 {
		t.Fatalf("asked in %d events, want 3", len(asked))
	}
	var all []string
	for i, e := range asked {
		page := strings.Split(e.Entities["ids"], ",")
		if want := []int{100, 100, 50}[i]; len(page) != want {
			t.Errorf("event %d names %d ids, want %d", i, len(page), want)
		}
		if e.Topic != searchTopic || e.Kind != kindSearchChanged || e.Key != "search:"+page[0] || !slices.Equal(e.Sinks, []string{searchSink}) {
			t.Errorf("event %d: %+v", i, e)
		}
		all = append(all, page...)
	}
	if !slices.Equal(all, ids) {
		t.Errorf("asked for %d ids, not each of the 250 once in order", len(all))
	}

	asked = nil
	SearchEntriesChanged(context.Background(), []string{"not-an-event"})
	if len(asked) != 0 {
		t.Errorf("asked for nothing named: %+v", asked)
	}
}

// Search finds the events on the calendars the grid shows someone (the
// people it names find it whatever its calendar): every event on those they
// may change, those that aren't private on those they only see, and nothing
// on a calendar they see only as busy, or not at all.
func TestSearchFindsEventsOnTheCalendarsTheGridShows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jonas, maya, lee := h.member("Jonas"), h.member("Maya"), h.member("Lee")
	project := uuid.NewString()
	projectsDecide(t, map[string]map[string]accessBusiness.Level{
		personOf(jonas).String(): {project: accessBusiness.LevelEdit},
		personOf(maya).String():  {project: accessBusiness.LevelView},
	}, map[string]string{project: "Launch"})
	launch, err := h.s.ProjectCalendarFor(ctx, jonas, project)
	if err != nil {
		t.Fatal(err)
	}
	own := map[*model.UserInfo]string{}
	for _, who := range []*model.UserInfo{jonas, maya, lee} {
		list, err := h.s.Calendars(ctx, who)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range list {
			if c.Mine {
				own[who] = c.ID
			}
		}
	}
	jonasCal, err := h.s.PersonCalendar(ctx, maya, personOf(jonas).String())
	if err != nil {
		t.Fatal(err)
	}
	split := func(who *model.UserInfo) string {
		t.Helper()
		all, open, err := h.s.searchCalendars(ctx, subjectOf(who))
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(all)
		slices.Sort(open)
		return strings.Join(all, ",") + " | " + strings.Join(open, ",")
	}
	sorted := func(ids ...string) string {
		slices.Sort(ids)
		return strings.Join(ids, ",")
	}
	if got, want := split(jonas), sorted(own[jonas], launch.String())+" | "; got != want {
		t.Errorf("Jonas, who edits the project: %s, want %s", got, want)
	}
	if got, want := split(maya), own[maya]+" | "+launch.String(); got != want {
		t.Errorf("Maya, who sees the project and Jonas only as busy: %s, want %s", got, want)
	}
	if got, want := split(lee), own[lee]+" | "; got != want {
		t.Errorf("Lee, in no project: %s, want %s", got, want)
	}
	if err := h.s.Share(ctx, jonas, jonasCal.String(), personOf(maya).String(), models.ShareDetails); err != nil {
		t.Fatal(err)
	}
	if got, want := split(maya), own[maya]+" | "+sorted(launch.String(), jonasCal.String()); got != want {
		t.Errorf("Maya, shown Jonas's details: %s, want %s", got, want)
	}
	if err := h.s.Share(ctx, jonas, jonasCal.String(), personOf(maya).String(), models.ShareEdit); err != nil {
		t.Fatal(err)
	}
	if got, want := split(maya), sorted(own[maya], jonasCal.String())+" | "+launch.String(); got != want {
		t.Errorf("Maya, who may change Jonas's calendar: %s, want %s", got, want)
	}
	if all, open, err := h.s.searchCalendars(ctx, accessBusiness.Subject{}); all != nil || open != nil || err != nil {
		t.Errorf("nobody's calendars: %v %v %v", all, open, err)
	}
}

// An import announces nothing, so it asks search for the entries of the
// events it made, as it tells the work graph of them; one that makes nothing
// asks for nothing.
func TestAnImportAsksSearchForItsEventsEntries(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	file := googleFile("jonas@example.com")
	made, err := h.s.importCalendars(context.Background(), sam, readFile(t, file), func(int, int) {})
	if err != nil || len(made.Created) != 2 {
		t.Fatalf("import %+v %v", made, err)
	}
	if want := []string{made.Created[0].UUID, made.Created[1].UUID}; !slices.Equal(h.searchTold, want) {
		t.Errorf("search was asked for %v, want %v", h.searchTold, want)
	}
	if _, err := h.s.importCalendars(context.Background(), sam, readFile(t, file), func(int, int) {}); err != nil || len(h.searchTold) != 2 {
		t.Errorf("an import that made nothing asked search for %v (%v)", h.searchTold[2:], err)
	}
}
