package Calendar

// The move into the one store (D-CAL-1): every Dgraph Event node and every
// calendar_events row, matched by uuid, into the events tables. It can run
// any number of times, and two processes running it at once write the same
// rows:
//
//   - what people saw wins: an event in the graph moves with the graph's
//     title, notes, times, organiser, guests (answered "accepted"), focus and
//     away, and the Google id from either store (the graph lost some);
//   - a row only Postgres has moves to the trash: deleted ones with their
//     date, and live ones (a create whose graph write failed, BE-F-1, which
//     nobody ever saw) as of the move, so nothing appears unannounced and
//     nothing is lost;
//   - each organiser gets their own calendar, each event its organiser's zone,
//     and an event that runs midnight to midnight there becomes its dates;
//   - an event the move made and nobody has changed since is brought up to
//     date on the next run, or put in the trash when it has gone from the old
//     stores; one changed in the new store is never touched again.
//
// Every run reports what it did, or would do, in counts and ids, never what an
// event says (CAL11), and checks that what it wrote reads back as it meant.

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	"github.com/google/uuid"
)

// movePage is how many events one step of the move reads and writes.
const movePage = 500

// maxDifferentIDs is how many differing events a report names.
const maxDifferentIDs = 50

// MoveReport is what a move did, or would do without Apply.
type MoveReport struct {
	Apply bool `json:"apply"`
	// What was read.
	GraphEvents int `json:"graph_events"`
	OldRows     int `json:"old_rows"`
	// PartialNodes are graph nodes with no times or no organiser, which an
	// old Google id write-back left beside the real node: nothing to move.
	PartialNodes int `json:"partial_nodes"`
	// What became of the events.
	Moved     int `json:"moved"`
	Refreshed int `json:"refreshed"`
	Unchanged int `json:"unchanged"`
	// Kept are events changed in the new store since: never touched again.
	Kept int `json:"kept"`
	// Trashed moved straight to the trash: deleted, or never shown.
	Trashed int `json:"trashed"`
	// Withdrawn were moved before and have gone from the old stores since.
	Withdrawn   int `json:"withdrawn"`
	NoOrganiser int `json:"no_organiser"`
	AllDay      int `json:"all_day"`
	TimesFixed  int `json:"times_fixed"`
	Guests      int `json:"guests"`
	GuestsLeft  int `json:"guests_left"`
	// Zones counts the moved events by where their zone came from.
	Zones map[string]int `json:"zones"`
	// The check: events read back after writing, and those that differ.
	Checked      int      `json:"checked"`
	Different    int      `json:"different"`
	DifferentIDs []string `json:"different_ids,omitempty"`
	// Pages is how many steps the move took.
	Pages int           `json:"pages"`
	Took  time.Duration `json:"took"`
}

// String is the report in words.
func (r *MoveReport) String() string {
	verb := "would move"
	if r.Apply {
		verb = "moved"
	}
	s := fmt.Sprintf("%d graph events and %d calendar_events rows read in %d steps (%s); %s %d, refreshed %d, unchanged %d, kept %d; "+
		"%d to the trash, %d withdrawn, %d without an organiser, %d partial nodes left; %d all-day, %d with times fixed; "+
		"%d guests (%d left out); zones %v",
		r.GraphEvents, r.OldRows, r.Pages, r.Took.Round(time.Millisecond), verb, r.Moved, r.Refreshed, r.Unchanged, r.Kept,
		r.Trashed, r.Withdrawn, r.NoOrganiser, r.PartialNodes, r.AllDay, r.TimesFixed, r.Guests, r.GuestsLeft, r.Zones)
	if r.Apply {
		s += fmt.Sprintf("; checked %d, %d different", r.Checked, r.Different)
		if len(r.DifferentIDs) > 0 {
			s += ": " + strings.Join(r.DifferentIDs, " ")
		}
	}
	return s
}

// Seams for tests: the graph's pages.
var graphEventsPage = domain.GetDgraphEventsPage

// mover's state for one run.
type moveRun struct {
	apply  bool
	report *MoveReport
	now    time.Time
	db     models.DB
	// seen are the events this run has accounted for; live, the ones it
	// moved as live.
	seen, live map[uuid.UUID]bool
	calendars  map[uuid.UUID]uuid.UUID
	zones      map[uuid.UUID]zoneSource
}

// zoneSource is an organiser's zone and where it came from.
type zoneSource struct{ zone, from string }

// movedEvent is one event as the move means to store it.
type movedEvent struct {
	event  *models.Event
	guests []models.Attendee
}

// MoveToPostgres moves, or with apply false only counts what it would move.
func MoveToPostgres(ctx context.Context, apply bool) (*MoveReport, error) {
	start := time.Now()
	r := &MoveReport{Apply: apply, Zones: map[string]int{}}
	m := &moveRun{apply: apply, report: r, now: time.Now().UTC(), db: models.Pool(),
		seen: map[uuid.UUID]bool{}, live: map[uuid.UUID]bool{}, calendars: map[uuid.UUID]uuid.UUID{}, zones: map[uuid.UUID]zoneSource{}}
	defer func() { r.Took = time.Since(start) }()

	for after := ""; ; {
		nodes, err := graphEventsPage(ctx, after, movePage)
		if err != nil {
			return r, err
		}
		if len(nodes) == 0 {
			break
		}
		after = nodes[len(nodes)-1].Uid
		if err := m.graphPage(ctx, nodes); err != nil {
			return r, err
		}
	}
	for after := uuid.Nil; ; {
		rows, err := models.OldEventsAfter(ctx, m.db, after, movePage)
		if err != nil {
			return r, err
		}
		if len(rows) == 0 {
			break
		}
		after = rows[len(rows)-1].ID
		if err := m.oldPage(ctx, rows); err != nil {
			return r, err
		}
	}
	return r, m.withdraw(ctx)
}

// moveAndMark is the move the switched-on server runs by itself: it marks the
// move done only when every event it wrote reads back as meant.
func moveAndMark(ctx context.Context) error {
	r, err := MoveToPostgres(ctx, true)
	if err != nil {
		return err
	}
	if r.Different > 0 {
		return fmt.Errorf("%d moved events don't read back as they were meant to (%s)", r.Different, strings.Join(r.DifferentIDs, " "))
	}
	helpers.MessageLogs.InfoLog.Printf("calendar: moved into the one store: %s", r)
	return configModel.UpsertConfig(movedKey, "done: "+r.String())
}

// graphPage moves one page of graph nodes.
func (m *moveRun) graphPage(ctx context.Context, nodes []*dgraphStruct.DgraphEvent) error {
	m.report.Pages++
	var good []*dgraphStruct.DgraphEvent
	var ids []uuid.UUID
	for _, n := range nodes {
		m.report.GraphEvents++
		id, err := uuid.Parse(n.Uuid)
		if err != nil || n.StartTime == nil || n.EndTime == nil || n.CreatedBy == nil || n.CreatedBy.Uuid == "" || m.seen[id] {
			m.report.PartialNodes++
			continue
		}
		m.seen[id] = true
		good = append(good, n)
		ids = append(ids, id)
	}
	olds, err := models.OldEventsByID(ctx, m.db, ids)
	if err != nil {
		return err
	}
	oldByID := map[uuid.UUID]*models.OldEvent{}
	for _, o := range olds {
		oldByID[o.ID] = o
	}
	var people []uuid.UUID
	for _, n := range good {
		people = appendUUIDs(people, n.CreatedBy.Uuid)
		for _, p := range n.Participants {
			if p != nil {
				people = appendUUIDs(people, p.Uuid)
			}
		}
	}
	known, err := models.PeopleIn(ctx, m.db, people)
	if err != nil {
		return err
	}
	var moved []movedEvent
	for _, n := range good {
		creator, _ := uuid.Parse(n.CreatedBy.Uuid)
		if !known[creator] {
			m.report.NoOrganiser++
			continue
		}
		me, err := m.fromGraph(ctx, n, oldByID[uuid.MustParse(n.Uuid)], creator, known)
		if err != nil {
			return err
		}
		m.live[me.event.ID] = true
		moved = append(moved, me)
	}
	return m.write(ctx, moved)
}

// oldPage moves the calendar_events rows of one page that the graph had no
// event for: to the trash.
func (m *moveRun) oldPage(ctx context.Context, rows []*models.OldEvent) error {
	var fresh []*models.OldEvent
	var people []uuid.UUID
	for _, o := range rows {
		m.report.OldRows++
		if m.seen[o.ID] {
			continue
		}
		m.seen[o.ID] = true
		fresh = append(fresh, o)
		if o.CreatedBy != nil {
			people = append(people, *o.CreatedBy)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	m.report.Pages++
	known, err := models.PeopleIn(ctx, m.db, people)
	if err != nil {
		return err
	}
	var moved []movedEvent
	for _, o := range fresh {
		if o.CreatedBy == nil || !known[*o.CreatedBy] {
			m.report.NoOrganiser++
			continue
		}
		kind := EventPlain
		if o.IsFocus {
			kind = EventFocus
		}
		e, err := m.event(ctx, o.ID, *o.CreatedBy, kind, o.Title, o.Description, o.StartTime, o.EndTime)
		if err != nil {
			return err
		}
		e.GoogleEventID = o.GoogleEventID
		e.CreatedAt, e.UpdatedAt = o.CreatedAt, o.UpdatedAt
		deleted := m.now
		if o.DeletedAt != nil {
			deleted = *o.DeletedAt
		}
		e.DeletedAt = &deleted
		moved = append(moved, movedEvent{event: e})
	}
	return m.write(ctx, moved)
}

// fromGraph is a graph node as the move stores it.
func (m *moveRun) fromGraph(ctx context.Context, n *dgraphStruct.DgraphEvent, old *models.OldEvent, creator uuid.UUID, known map[uuid.UUID]bool) (movedEvent, error) {
	kind := EventPlain
	switch {
	case n.IsAway != nil && *n.IsAway:
		kind = EventAway
	case n.IsFocus != nil && *n.IsFocus:
		kind = EventFocus
	}
	e, err := m.event(ctx, uuid.MustParse(n.Uuid), creator, kind, n.Title, n.Description, *n.StartTime, *n.EndTime)
	if err != nil {
		return movedEvent{}, err
	}
	if n.GoogleCalendarEventId != nil && *n.GoogleCalendarEventId != "" {
		e.GoogleEventID = *n.GoogleCalendarEventId
	} else if old != nil {
		e.GoogleEventID = old.GoogleEventID
	}
	switch {
	case old != nil:
		e.CreatedAt, e.UpdatedAt = old.CreatedAt, old.UpdatedAt
	case n.CreatedAt != nil && n.CreatedAt.Year() > 1970:
		e.CreatedAt, e.UpdatedAt = n.CreatedAt.UTC(), n.CreatedAt.UTC()
		if n.UpdatedAt != nil && n.UpdatedAt.Year() > 1970 {
			e.UpdatedAt = n.UpdatedAt.UTC()
		}
	}
	var guests []models.Attendee
	seen := map[uuid.UUID]bool{creator: true}
	for _, p := range n.Participants {
		if p == nil {
			continue
		}
		id, err := uuid.Parse(p.Uuid)
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		if !known[id] {
			m.report.GuestsLeft++
			continue
		}
		guest := id
		guests = append(guests, models.Attendee{EventID: e.ID, UserID: &guest, Role: roleRequired, Response: responseAccepted, AddedAt: e.CreatedAt})
	}
	return movedEvent{event: e, guests: guests}, nil
}

// event is the common part of a moved event: its organiser's calendar and
// zone, its kind and words, and its time, as dates when it is whole days
// there (focus time never: legacyWhen).
func (m *moveRun) event(ctx context.Context, id, creator uuid.UUID, kind EventKind, title, notes string, start, end time.Time) (*models.Event, error) {
	zone, err := m.zoneOf(ctx, creator)
	if err != nil {
		return nil, err
	}
	calendar, err := m.calendarOf(ctx, creator, zone)
	if err != nil {
		return nil, err
	}
	e := blank(zone)
	e.ID, e.CalendarID, e.CreatedBy = id, calendar, creator
	e.ICalUID, e.Source, e.Sequence = id.String()+"@onecamp", sourceMoved, 0
	e.Kind, e.Title, e.Notes = string(kind), title, notes
	e.CreatedAt, e.UpdatedAt = m.now, m.now
	start, end = start.UTC(), end.UTC()
	if end.Before(start) {
		end = start
		m.report.TimesFixed++
	}
	legacyWhen(e, start, end, nil)
	return e, nil
}

// zoneOf is an organiser's zone, read once a run.
func (m *moveRun) zoneOf(ctx context.Context, person uuid.UUID) (string, error) {
	if z, ok := m.zones[person]; ok {
		return z.zone, nil
	}
	hints, err := models.ZoneHintsOf(ctx, m.db, []uuid.UUID{person})
	if err != nil {
		return "", err
	}
	z := zoneFrom(hints[person])
	m.zones[person] = zoneSource{zone: z, from: zoneSourceOf(hints[person], z)}
	return z, nil
}

// calendarOf is an organiser's own calendar, made once (none in a dry run).
func (m *moveRun) calendarOf(ctx context.Context, person uuid.UUID, zone string) (uuid.UUID, error) {
	if !m.apply {
		return uuid.Nil, nil
	}
	if c, ok := m.calendars[person]; ok {
		return c, nil
	}
	c, err := models.PersonalCalendar(ctx, m.db, person, uuid.New(), zone)
	if err != nil {
		return uuid.Nil, err
	}
	m.calendars[person] = c
	return c, nil
}

// zoneSourceOf says where a zone came from, for the report. Pure.
func zoneSourceOf(h models.ZoneHints, zone string) string {
	for _, c := range []struct{ name, hint string }{{"quiet_hours", h.QuietHours}, {"booking_page", h.BookingPage}, {"task_repeat", h.TaskRepeat}} {
		if loc, ok := helpers.FirstLocation(c.hint); ok && loc.String() == zone {
			return c.name
		}
	}
	return "utc"
}

// write stores one page: new events inserted, the move's own brought up to
// date, the rest left; then (applying) reads them back to check them.
func (m *moveRun) write(ctx context.Context, moved []movedEvent) error {
	if len(moved) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(moved))
	for _, me := range moved {
		ids = append(ids, me.event.ID)
	}
	if !m.apply {
		existing, guests, err := m.stored(ctx, ids)
		if err != nil {
			return err
		}
		for _, me := range moved {
			m.classify(me, existing[me.event.ID], guests[me.event.ID])
		}
		return nil
	}
	err := postgresInit.InTx(ctx, postgresInit.BulkTimeout, func(ctx context.Context, tx *sql.Tx) error {
		events := make([]*models.Event, 0, len(moved))
		for _, me := range moved {
			events = append(events, me.event)
		}
		wrote, err := models.InsertIfAbsent(ctx, tx, events)
		if err != nil {
			return err
		}
		var newGuests []models.Attendee
		var older []uuid.UUID
		for _, me := range moved {
			if wrote[me.event.ID] {
				newGuests = append(newGuests, me.guests...)
				m.counted(me)
				m.report.Moved++
			} else {
				older = append(older, me.event.ID)
			}
		}
		if err := models.AddAttendees(ctx, tx, newGuests); err != nil {
			return err
		}
		existing, guests, err := m.storedIn(ctx, tx, older)
		if err != nil {
			return err
		}
		for _, me := range moved {
			if wrote[me.event.ID] {
				continue
			}
			switch m.classify(me, existing[me.event.ID], guests[me.event.ID]) {
			case "refresh":
				if _, err := models.RefreshMoved(ctx, tx, me.event); err != nil {
					return err
				}
				if err := models.ReplaceMovedAttendees(ctx, tx, me.event.ID, me.guests); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return m.check(ctx, moved)
}

// counted adds an event about to be written to the report's tallies.
func (m *moveRun) counted(me movedEvent) {
	if me.event.AllDay() {
		m.report.AllDay++
	}
	if me.event.DeletedAt != nil {
		m.report.Trashed++
	}
	m.report.Guests += len(me.guests)
	m.report.Zones[m.zones[me.event.CreatedBy].from]++
}

// classify counts an event the store already has, and says what to do:
// "refresh" for the move's own when it differs, "" otherwise.
func (m *moveRun) classify(me movedEvent, have *models.Event, haveGuests []models.Attendee) string {
	switch {
	case have == nil:
		m.counted(me)
		m.report.Moved++
		return ""
	case have.Source != sourceMoved || have.Sequence != 0:
		m.report.Kept++
		return ""
	case sameMoved(have, haveGuests, me):
		m.report.Unchanged++
		return ""
	default:
		m.counted(me)
		m.report.Refreshed++
		return "refresh"
	}
}

// stored is these events and their guests as the store has them.
func (m *moveRun) stored(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*models.Event, map[uuid.UUID][]models.Attendee, error) {
	return m.storedIn(ctx, m.db, ids)
}

func (m *moveRun) storedIn(ctx context.Context, db models.DB, ids []uuid.UUID) (map[uuid.UUID]*models.Event, map[uuid.UUID][]models.Attendee, error) {
	out := map[uuid.UUID]*models.Event{}
	if len(ids) == 0 {
		return out, nil, nil
	}
	rows, err := models.GetMany(ctx, db, ids)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range rows {
		out[e.ID] = e
	}
	guests, err := models.AttendeesOf(ctx, db, ids)
	if err != nil {
		return nil, nil, err
	}
	return out, guests, nil
}

// check reads a written page back: every event the move owns must read as
// it was meant to.
func (m *moveRun) check(ctx context.Context, moved []movedEvent) error {
	ids := make([]uuid.UUID, 0, len(moved))
	for _, me := range moved {
		ids = append(ids, me.event.ID)
	}
	have, guests, err := m.stored(ctx, ids)
	if err != nil {
		return err
	}
	for _, me := range moved {
		e := have[me.event.ID]
		if e != nil && (e.Source != sourceMoved || e.Sequence != 0) {
			continue // changed in the new store since: not the move's to check
		}
		m.report.Checked++
		if e == nil || !sameMoved(e, guests[me.event.ID], me) {
			m.report.Different++
			if len(m.report.DifferentIDs) < maxDifferentIDs {
				m.report.DifferentIDs = append(m.report.DifferentIDs, me.event.ID.String())
			}
		}
	}
	return nil
}

// sameMoved is whether a stored event is what the move means it to be. Pure.
func sameMoved(have *models.Event, haveGuests []models.Attendee, want movedEvent) bool {
	w := want.event
	if have.Kind != w.Kind || have.Title != w.Title || have.Notes != w.Notes || have.TZ != w.TZ ||
		have.StartDate != w.StartDate || have.EndDate != w.EndDate ||
		!sameTime(have.StartAt, w.StartAt) || !sameTime(have.EndAt, w.EndAt) ||
		have.GoogleEventID != w.GoogleEventID || have.CreatedBy != w.CreatedBy ||
		(have.DeletedAt == nil) != (w.DeletedAt == nil) {
		return false
	}
	var a, b []string
	for _, g := range haveGuests {
		if g.UserID != nil {
			a = append(a, g.UserID.String())
		}
	}
	for _, g := range want.guests {
		b = append(b, g.UserID.String())
	}
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// withdraw puts in the trash the move's own events that have gone from the
// old stores since an earlier run brought them across.
func (m *moveRun) withdraw(ctx context.Context) error {
	for after := uuid.Nil; ; {
		ids, err := models.MovedUnchangedAfter(ctx, m.db, after, movePage)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		after = ids[len(ids)-1]
		var gone []uuid.UUID
		for _, id := range ids {
			if !m.live[id] {
				gone = append(gone, id)
			}
		}
		if !m.apply {
			m.report.Withdrawn += len(gone)
			continue
		}
		n, err := models.TrashMoved(ctx, m.db, gone, m.now)
		if err != nil {
			return err
		}
		m.report.Withdrawn += n
	}
}

// appendUUIDs adds a uuid, read from text, to a list. Pure.
func appendUUIDs(list []uuid.UUID, id string) []uuid.UUID {
	if u, err := uuid.Parse(id); err == nil {
		return append(list, u)
	}
	return list
}
