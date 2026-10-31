package Calendar

// The calendar's events in global search (gcal-13). Each series or single
// event has one entry in the events index, written whole from the one store;
// business/GlobalSearch reads it (its sources.go says who finds one).
//
// WHO FINDS AN EVENT. Its entry names the people it is theirs to see (its
// organiser, or the reader of a subscribed calendar, and its member guests)
// and the calendar it is on. Search finds it for them, and for whoever sees
// that calendar (a project's, or one shared with them): every event on a
// calendar they may change, and on one they only see, the events that aren't
// private, as the grid shows them (hiddenFrom); a calendar seen only as busy
// finds nothing (SearchCalendars). Search then asks the Access Resolver about
// every event on a page (business/GlobalSearch readableEvents.go), so an entry
// that is behind costs a result, and never shows an event to someone who may
// not open it.
//
// HOW AN ENTRY IS KEPT. Every change the service makes is announced
// (announce). A sink takes each event's announcements and writes an outbox
// event naming it (SearchEntriesChanged), and the outbox's sink writes the
// entries of the events it names as the store has them when it runs: a repeat
// writes the same, a late one what is there now, and an event that is gone,
// in the trash or of a kind search leaves out loses its entry. A writer that
// announces nothing (an import, a subscribed calendar's read, Google's
// changes) names what it wrote, and a pass at start and every night writes
// every event's entry, so an index that is new, emptied by the demo's reset
// or restored without them fills again.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	bulkModel "github.com/akashc777/OneCamp/models/openSearch/Bulk"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// The entries' outbox topic, its one sink and its event's kind.
const (
	searchTopic       = "calendar.search"
	searchSink        = "calendar.search"
	kindSearchChanged = "entries.changed"
	// searchPage is how many events one outbox event names, and one page of
	// the nightly pass holds: one read of each store and one bulk write.
	searchPage = 100
)

// unsearchedKinds are the events with no entry: a task block, called by its
// task, whose name not everyone who sees the block may read, and which search
// finds as the task itself; and a day's working location, one a day, never
// what anyone looks for.
var unsearchedKinds = []string{string(EventTaskBlock), string(EventWorkingLocation)}

func init() {
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: searchTopic, Name: searchSink, Run: runSearchSink})
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{
		Name:  "calendar.search",
		Wants: func(e resourceEventBusiness.Event) bool { return e.ResourceKind == KindEvent },
		Run: func(ctx context.Context, e resourceEventBusiness.Event) {
			SearchEntriesChanged(ctx, []string{e.ResourceUUID})
		},
		InBulk: true,
	})
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "calendar.search", Every: 24 * time.Hour, RunAtStart: true, Run: WriteEverySearchEntry})
}

// The entries' writes, seams for tests: the outbox events asking for them,
// whether there is a search index to write to, and the bulk write itself.
var (
	enqueueSearch   = outboxBusiness.Enqueue
	searchConnected = func() bool { return opensearchInit.OpenSearchClient != nil }
	writeSearch     = bulkModel.BulkIndexChecked
)

// SearchEntriesChanged has the search entries of the events ids name written
// again from the store, through the outbox: events of at most searchPage ids,
// written ready in one statement. An instance is its series. It never fails
// the caller: an entry it couldn't ask for is tonight's pass's.
func SearchEntriesChanged(ctx context.Context, ids []string) {
	keys := searchKeys(ids)
	var es []outboxBusiness.Entry
	for from := 0; from < len(keys); from += searchPage {
		page := keys[from:min(from+searchPage, len(keys))]
		es = append(es, outboxBusiness.Entry{
			Topic: searchTopic, Kind: kindSearchChanged, Key: "search:" + page[0],
			Entities: map[string]string{"ids": strings.Join(page, ",")},
			Sinks:    []string{searchSink},
		})
	}
	if len(es) == 0 {
		return
	}
	if err := enqueueSearch(ctx, es...); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: the search entries of %d events weren't asked for, so tonight's pass writes them: %v", len(keys), err)
	}
}

// searchKeys are the events ids name, each once, in their order: an
// instance's ref names its series; what names no event is dropped. Pure.
func searchKeys(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		u, _, err := parseRef(strings.TrimSpace(id))
		if err != nil {
			continue
		}
		if k := u.String(); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// runSearchSink is the outbox's sink: the entries of the events its event
// names, written as the store has them now.
func runSearchSink(ctx context.Context, ev outboxBusiness.Event) error {
	if !helpers.CalendarReadsFromStore() {
		return nil
	}
	var ids []uuid.UUID
	for _, id := range strings.Split(ev.Entities["ids"], ",") {
		if u, err := uuid.Parse(id); err == nil {
			ids = append(ids, u)
		}
	}
	return stored.writeSearchEntries(ctx, ids)
}

// WriteEverySearchEntry is the pass, at start and every night: every series'
// and single event's entry written from the store. While the calendar isn't
// read from its store, no event has an entry.
func WriteEverySearchEntry(ctx context.Context) error {
	if !helpers.CalendarReadsFromStore() {
		return nil
	}
	return stored.writeEverySearchEntry(ctx)
}

// writeEverySearchEntry is the pass a page at a time, in order of id.
func (s *service) writeEverySearchEntry(ctx context.Context) error {
	if !searchConnected() {
		return nil
	}
	if err := s.ready(ctx); err != nil {
		return err
	}
	after := uuid.Nil
	for {
		ids, err := s.store.seriesAfter(ctx, after, unsearchedKinds, true, searchPage)
		if err != nil || len(ids) == 0 {
			return err
		}
		if err := s.writeSearchEntries(ctx, ids); err != nil {
			return err
		}
		after = ids[len(ids)-1]
	}
}

// writeSearchEntries writes the entries of the events ids name as the store
// has them, in one bulk write: a series or single event that search lists
// gets its entry whole, an instance changed on its own has its series'
// written, and anything else (gone, in the trash, a task block) has none. One
// read of the events, their guests, their calendars and the people they name.
func (s *service) writeSearchEntries(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 || !searchConnected() {
		return nil
	}
	if err := s.ready(ctx); err != nil {
		return err
	}
	rows, err := s.store.getMany(ctx, ids)
	if err != nil {
		return err
	}
	byID := make(map[uuid.UUID]*models.Event, len(rows))
	for _, e := range rows {
		byID[e.ID] = e
	}
	var series []uuid.UUID
	for _, e := range rows {
		if e.SeriesID != nil && byID[*e.SeriesID] == nil && !slices.Contains(series, *e.SeriesID) {
			series = append(series, *e.SeriesID)
		}
	}
	if len(series) > 0 {
		more, err := s.store.getMany(ctx, series)
		if err != nil {
			return err
		}
		for _, e := range more {
			byID[e.ID] = e
		}
		ids = append(slices.Clip(ids), series...)
	}
	var listed []*models.Event
	var calendarIDs []uuid.UUID
	for _, e := range byID {
		if searched(e) {
			listed = append(listed, e)
			if !slices.Contains(calendarIDs, e.CalendarID) {
				calendarIDs = append(calendarIDs, e.CalendarID)
			}
		}
	}
	guests, err := s.store.attendees(ctx, idsOf(listed))
	if err != nil {
		return err
	}
	cals, err := s.store.calendarsByID(ctx, calendarIDs)
	if err != nil {
		return err
	}
	calendarOf := make(map[uuid.UUID]models.Calendar, len(cals))
	for _, c := range cals {
		calendarOf[c.ID] = c
	}
	people, err := s.peopleNamed(ctx, listed, guests)
	if err != nil {
		return err
	}
	var b strings.Builder
	written := map[uuid.UUID]bool{}
	for _, id := range ids {
		if written[id] {
			continue
		}
		written[id] = true
		e := byID[id]
		if !searched(e) {
			fmt.Fprintf(&b, "{ \"delete\" : { \"_index\" : %q, \"_id\" : %q } }\n", openSearchStruct.EVENT_INDEX, id.String())
			continue
		}
		doc, err := json.Marshal(searchEntryOf(e, guests[e.ID], calendarOf[e.CalendarID], people))
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "{ \"index\" : { \"_index\" : %q, \"_id\" : %q } }\n%s\n", openSearchStruct.EVENT_INDEX, id.String(), doc)
	}
	return writeSearch(ctx, b.String())
}

// searched is whether an event has an entry: a series or a single event, out
// of the trash, of a kind search lists. Pure.
func searched(e *models.Event) bool {
	return e != nil && e.SeriesID == nil && e.DeletedAt == nil && !slices.Contains(unsearchedKinds, e.Kind)
}

// searchEntryOf is an event's whole entry, from the event, its guests, the
// calendar it is on and the people they name. A guest's name is kept only
// while the guests may see one another. Pure.
func searchEntryOf(e *models.Event, guests []models.Attendee, cal models.Calendar, people map[string]*dgraphStruct.DgraphUser) openSearchStruct.OpenSearchEvent {
	entry := openSearchStruct.OpenSearchEvent{
		Uuid: e.ID.String(), EventTitle: e.Title, EventNotes: notesAsText(e), EventLocation: e.Location, EventKind: e.Kind,
		EventStartDate: e.StartDate, EventEndDate: e.EndDate, EventZone: e.TZ, EventRepeats: e.RRule != "",
		EventOrganiserUuid: e.CreatedBy.String(),
		EventPeople:        []string{e.CreatedBy.String()},
		EventGuestNames:    []string{},
		EventCalendarUuid:  e.CalendarID.String(),
		EventPrivate:       e.Visibility == visibilityPrivate,
		EventCreatedAt:     e.CreatedAt.Unix(),
		EventUpdatedAt:     e.UpdatedAt.Unix(),
	}
	if e.StartAt != nil {
		entry.EventStart = e.StartAt.Unix()
	}
	if e.EndAt != nil {
		entry.EventEnd = e.EndAt.Unix()
	}
	if p := people[e.CreatedBy.String()]; p != nil {
		entry.EventOrganiserFullName = p.DisplayName()
	}
	if cal.ContainerKind == containerProject {
		entry.EventProjectUuid = cal.ContainerID
	}
	for _, g := range guests {
		name := g.Name
		if g.UserID != nil {
			entry.EventPeople = append(entry.EventPeople, g.UserID.String())
			if p := people[g.UserID.String()]; p != nil {
				name = p.DisplayName()
			}
		}
		if e.GuestsCanSeeGuests && name != "" {
			entry.EventGuestNames = append(entry.EventGuestNames, name)
		}
	}
	return entry
}

// SearchCalendars are the calendars whose events who finds in search besides
// their own and their invitations, as the grid shows them (hiddenFrom): every
// event on those they may change (all), and the events that aren't private on
// those they only see (open). None while the calendar isn't read from its
// store.
func SearchCalendars(ctx context.Context, who accessBusiness.Subject) (all, open []string, err error) {
	if !helpers.CalendarReadsFromStore() {
		return nil, nil, nil
	}
	return stored.searchCalendars(ctx, who)
}

func (s *service) searchCalendars(ctx context.Context, who accessBusiness.Subject) (all, open []string, err error) {
	me, perr := uuid.Parse(who.UserUUID)
	if perr != nil {
		return nil, nil, nil
	}
	if err := s.ready(ctx); err != nil {
		return nil, nil, err
	}
	seen, decided, err := s.seenCalendars(ctx, who, me)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range seen {
		if decided[c.ID.String()].Level >= accessBusiness.LevelEdit {
			all = append(all, c.ID.String())
		} else {
			open = append(open, c.ID.String())
		}
	}
	return all, open, nil
}
