package Calendar

// A store in memory for the service's unit tests: the Postgres store's rules
// (models/postgres/CalendarV2) without Postgres. The SQL itself is held to the
// same answers by the integration tests (tests/integration/calendar_store_test.go).

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

type memStore struct {
	mu     sync.Mutex
	events map[uuid.UUID]*models.Event
	guests map[uuid.UUID][]models.Attendee
	// answered are the answers given for single instances alone.
	answered  []models.InstanceAnswer
	calendars map[uuid.UUID]uuid.UUID
	hints     map[uuid.UUID]models.ZoneHints
	noAccount map[uuid.UUID]bool
	// notMembers keep their accounts but are no members now: deactivated,
	// or an outside guest's or a bot's account.
	notMembers map[uuid.UUID]bool
	// addresses are the accounts' addresses, in lower case.
	addresses map[string]uuid.UUID
	writes    int
	// imported are the calendars imports made, by owner and name;
	// containers the containers' calendars, by kind and id.
	imported   map[string]uuid.UUID
	containers map[string]uuid.UUID
	shares     []models.CalendarShare
	// calendarEdits are calendars' names and colours as changed, by id.
	calendarEdits map[uuid.UUID][2]string
	// ooo is each out-of-office event's decline rule (head 5b).
	ooo map[uuid.UUID]models.OOORule
	// feeds are secret feeds, in the order made; revoked ones stay, marked.
	feeds []memFeed
	// subscribed are calendars read from addresses, by id.
	subscribed map[uuid.UUID]models.Calendar
	// reads counts the reads a test pins, by the store's method.
	readMu sync.Mutex
	reads  map[string]int
}

// countRead counts one call of a store read.
func (m *memStore) countRead(name string) {
	m.readMu.Lock()
	defer m.readMu.Unlock()
	if m.reads == nil {
		m.reads = map[string]int{}
	}
	m.reads[name]++
}

// readsOf is how many times a read was called since the last resetReads.
func (m *memStore) readsOf(name string) int {
	m.readMu.Lock()
	defer m.readMu.Unlock()
	return m.reads[name]
}

func (m *memStore) resetReads() {
	m.readMu.Lock()
	defer m.readMu.Unlock()
	m.reads = nil
}

func newMemStore() *memStore {
	return &memStore{events: map[uuid.UUID]*models.Event{}, guests: map[uuid.UUID][]models.Attendee{},
		calendars: map[uuid.UUID]uuid.UUID{}, hints: map[uuid.UUID]models.ZoneHints{}, noAccount: map[uuid.UUID]bool{},
		notMembers: map[uuid.UUID]bool{}, addresses: map[string]uuid.UUID{}}
}

func copyEvent(e *models.Event) *models.Event {
	c := *e
	c.Changed = slices.Clone(e.Changed)
	c.Exdates = slices.Clone(e.Exdates)
	c.Rdates = slices.Clone(e.Rdates)
	return &c
}

func (m *memStore) personalCalendar(_ context.Context, owner uuid.UUID, _ string) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.calendars[owner]; ok {
		return c, nil
	}
	c := uuid.New()
	m.calendars[owner] = c
	return c, nil
}

func (m *memStore) containerCalendar(_ context.Context, kind, id, _ string) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.containers == nil {
		m.containers = map[string]uuid.UUID{}
	}
	k := kind + "/" + id
	if c, ok := m.containers[k]; ok {
		return c, nil
	}
	c := uuid.New()
	m.containers[k] = c
	return c, nil
}

func (m *memStore) calendarsByID(_ context.Context, ids []uuid.UUID) ([]models.Calendar, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.Calendar
	defer func() {
		for i := range out {
			if e, ok := m.calendarEdits[out[i].ID]; ok {
				out[i].Name, out[i].Hue = e[0], e[1]
			}
		}
	}()
	for _, id := range ids {
		for owner, c := range m.calendars {
			if c == id {
				o := owner
				out = append(out, models.Calendar{ID: id, Kind: "personal", OwnerID: &o})
			}
		}
		for k, c := range m.containers {
			if c == id {
				kind, cid, _ := strings.Cut(k, "/")
				out = append(out, models.Calendar{ID: id, Kind: kind, ContainerKind: kind, ContainerID: cid})
			}
		}
		for k, c := range m.imported {
			if c == id {
				who, name, _ := strings.Cut(k, "/")
				o := uuid.MustParse(who)
				out = append(out, models.Calendar{ID: id, Kind: "external", OwnerID: &o, Name: name})
			}
		}
		if c, ok := m.subscribed[id]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memStore) nextOf(_ context.Context, calendars []uuid.UUID, now time.Time, limit int) ([]*models.Event, error) {
	m.countRead("nextOf")
	var out []*models.Event
	for _, calendar := range calendars {
		out = append(out, m.sorted(func(e *models.Event) bool {
			if e.CalendarID != calendar || e.DeletedAt != nil || e.SeriesID != nil {
				return false
			}
			_, end := spanOf(e)
			return e.RRule != "" || !end.Before(now)
		}, limit)...)
	}
	return out, nil
}

func (m *memStore) sharesFor(_ context.Context, person uuid.UUID, calendars []uuid.UUID) (map[uuid.UUID]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uuid.UUID]string{}
	for _, sh := range m.shares {
		if sh.PersonID == person && slices.Contains(calendars, sh.CalendarID) {
			out[sh.CalendarID] = sh.Level
		}
	}
	return out, nil
}

func (m *memStore) sharesOf(_ context.Context, calendar uuid.UUID) ([]models.CalendarShare, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.CalendarShare
	for _, sh := range m.shares {
		if sh.CalendarID == calendar {
			out = append(out, sh)
		}
	}
	return out, nil
}

func (m *memStore) setShare(_ context.Context, calendar, person, _ uuid.UUID, level string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shares = slices.DeleteFunc(m.shares, func(sh models.CalendarShare) bool { return sh.CalendarID == calendar && sh.PersonID == person })
	if level != "" {
		m.shares = append(m.shares, models.CalendarShare{CalendarID: calendar, PersonID: person, Level: level})
	}
	return nil
}

type memFeed struct {
	models.Feed
	revoked bool
}

func (m *memStore) liveFeed(_ context.Context, calendar, person uuid.UUID) (*models.Feed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.feeds {
		if !f.revoked && f.CalendarID == calendar && f.PersonID == person {
			c := f.Feed
			return &c, nil
		}
	}
	return nil, nil
}

func (m *memStore) feedByHash(_ context.Context, hash []byte) (*models.Feed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.feeds {
		if !f.revoked && bytes.Equal(f.TokenHash, hash) {
			c := f.Feed
			return &c, nil
		}
	}
	return nil, nil
}

func (m *memStore) makeFeed(_ context.Context, f models.Feed) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.feeds {
		if m.feeds[i].CalendarID == f.CalendarID && m.feeds[i].PersonID == f.PersonID {
			m.feeds[i].revoked = true
		}
	}
	m.feeds = append(m.feeds, memFeed{Feed: f})
	return nil
}

func (m *memStore) revokeFeed(_ context.Context, calendar, person uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.feeds {
		if m.feeds[i].CalendarID == calendar && m.feeds[i].PersonID == person {
			m.feeds[i].revoked = true
		}
	}
	return nil
}

func (m *memStore) updateCalendar(_ context.Context, id uuid.UUID, name, hue *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calendarEdits == nil {
		m.calendarEdits = map[uuid.UUID][2]string{}
	}
	cur := m.calendarEdits[id]
	if name != nil {
		cur[0] = *name
	}
	if hue != nil {
		cur[1] = *hue
	}
	m.calendarEdits[id] = cur
	return nil
}

func (m *memStore) sharedWith(ctx context.Context, person uuid.UUID) ([]models.Calendar, error) {
	m.mu.Lock()
	var ids []uuid.UUID
	for _, sh := range m.shares {
		if sh.PersonID == person {
			ids = append(ids, sh.CalendarID)
		}
	}
	m.mu.Unlock()
	return m.calendarsByID(ctx, ids)
}

func (m *memStore) personalCalendarsOf(_ context.Context, owners []uuid.UUID) ([]models.Calendar, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.Calendar
	for _, o := range owners {
		if c, ok := m.calendars[o]; ok {
			owner := o
			out = append(out, models.Calendar{ID: c, Kind: "personal", OwnerID: &owner})
		}
	}
	return out, nil
}

func (m *memStore) ownCalendars(_ context.Context, owner uuid.UUID) ([]models.Calendar, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.Calendar
	if c, ok := m.calendars[owner]; ok {
		o := owner
		out = append(out, models.Calendar{ID: c, Kind: "personal", OwnerID: &o})
	}
	for k, c := range m.imported {
		who, name, _ := strings.Cut(k, "/")
		if who == owner.String() {
			o := owner
			out = append(out, models.Calendar{ID: c, Kind: "external", OwnerID: &o, Name: name})
		}
	}
	for _, c := range m.subscribed {
		if *c.OwnerID == owner {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memStore) subscribedCalendar(_ context.Context, id, owner uuid.UUID, name, hue, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.subscribed == nil {
		m.subscribed = map[uuid.UUID]models.Calendar{}
	}
	o := owner
	m.subscribed[id] = models.Calendar{ID: id, Kind: kindSubscribed, OwnerID: &o, Name: name, Hue: hue}
	return nil
}

// deleteSubscribedCalendar deletes it and, as Postgres cascades, every event on it.
func (m *memStore) deleteSubscribedCalendar(_ context.Context, id, owner uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.subscribed[id]
	if !ok || *c.OwnerID != owner {
		return false, nil
	}
	delete(m.subscribed, id)
	for eid, e := range m.events {
		if e.CalendarID == id {
			delete(m.events, eid)
		}
	}
	return true, nil
}

// deleteSubscribedEvents deletes the address's events of these ids on the
// calendar, and (as Postgres cascades) their changed instances.
func (m *memStore) deleteSubscribedEvents(_ context.Context, calendar uuid.UUID, ids []uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		e := m.events[id]
		if e == nil || e.CalendarID != calendar || e.Source != sourceSubscribed {
			continue
		}
		delete(m.events, id)
		for oid, o := range m.events {
			if o.SeriesID != nil && *o.SeriesID == id {
				delete(m.events, oid)
			}
		}
	}
	return nil
}

func (m *memStore) containerCalendars(_ context.Context, kind string, limit int) ([]models.Calendar, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.Calendar
	for k, c := range m.containers {
		ck, cid, _ := strings.Cut(k, "/")
		if ck == kind && len(out) < limit {
			out = append(out, models.Calendar{ID: c, Kind: ck, ContainerKind: ck, ContainerID: cid})
		}
	}
	return out, nil
}

func (m *memStore) rangeOnCalendars(_ context.Context, calendars []uuid.UUID, w models.Window, limit int) ([]*models.Event, error) {
	return m.sorted(func(e *models.Event) bool {
		return e.DeletedAt == nil && e.SeriesID == nil && slices.Contains(calendars, e.CalendarID) && (inWindow(e, w) || e.RRule != "")
	}, limit), nil
}

func (m *memStore) importedCalendar(_ context.Context, owner uuid.UUID, name, _ string) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.imported == nil {
		m.imported = map[string]uuid.UUID{}
	}
	k := owner.String() + "/" + name
	if c, ok := m.imported[k]; ok {
		return c, nil
	}
	c := uuid.New()
	m.imported[k] = c
	return c, nil
}

func (m *memStore) get(_ context.Context, id uuid.UUID) (*models.Event, error) {
	m.countRead("get")
	return m.event(id), nil
}

// event is a copy of one event, or nil, read without counting.
func (m *memStore) event(id uuid.UUID) *models.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.events[id]; e != nil {
		return copyEvent(e)
	}
	return nil
}

func (m *memStore) getMany(_ context.Context, ids []uuid.UUID) ([]*models.Event, error) {
	m.countRead("getMany")
	var out []*models.Event
	for _, id := range ids {
		if e := m.event(id); e != nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memStore) memberGuests(_ context.Context, events []uuid.UUID, perEvent int) (map[uuid.UUID][]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uuid.UUID][]uuid.UUID{}
	for _, id := range events {
		for _, g := range m.guests[id] {
			if g.UserID != nil && g.Response != string(Declined) && len(out[id]) < perEvent {
				out[id] = append(out[id], *g.UserID)
			}
		}
	}
	return out, nil
}

func (m *memStore) seriesAfter(_ context.Context, after uuid.UUID, skip []string, subscribed bool, limit int) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []uuid.UUID
	for id, e := range m.events {
		if e.SeriesID == nil && (subscribed || e.Source != sourceSubscribed) && !slices.Contains(skip, e.Kind) && strings.Compare(id.String(), after.String()) > 0 {
			out = append(out, id)
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out[:min(limit, len(out))], nil
}

func (m *memStore) byICalUIDs(_ context.Context, uids []string) ([]*models.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*models.Event
	for _, e := range m.events {
		if e.SeriesID == nil && slices.Contains(uids, e.ICalUID) {
			out = append(out, copyEvent(e))
		}
	}
	return out, nil
}

func (m *memStore) attendees(_ context.Context, events []uuid.UUID) (map[uuid.UUID][]models.Attendee, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uuid.UUID][]models.Attendee{}
	for _, id := range events {
		if g := m.guests[id]; len(g) > 0 {
			out[id] = slices.Clone(g)
		}
	}
	return out, nil
}

// reaches is the window test of models' overlaps: a single event touching
// it, bounds counted in, or a series that could have an instance in it.
func reaches(e *models.Event, w models.Window) bool {
	if e.RRule == "" {
		return touches(e, w)
	}
	if e.AllDay() {
		if e.StartDate > w.ToDate {
			return false
		}
	} else if e.StartAt.After(w.To) {
		return false
	}
	if e.UntilAt == nil {
		return true
	}
	_, _, dur := seriesTime(e)
	return !e.UntilAt.Before(w.From.Add(-(dur + 24*time.Hour)))
}

// movedInto is whether a series has a live instance moved by hand into w, as
// models' movedInto says.
func (m *memStore) movedInto(e *models.Event, w models.Window) bool {
	if e.RRule == "" {
		return false
	}
	for _, o := range m.events {
		if o.SeriesID != nil && *o.SeriesID == e.ID && o.DeletedAt == nil && o.Status != statusCancelled && touches(o, w) {
			return true
		}
	}
	return false
}

// touches is a single event's window test: bounds counted in.
func touches(e *models.Event, w models.Window) bool {
	if e.AllDay() {
		return e.StartDate <= w.ToDate && e.EndDate >= w.FromDate
	}
	return !e.StartAt.After(w.To) && !e.EndAt.Before(w.From)
}

func (m *memStore) on(e *models.Event, person uuid.UUID) bool {
	if e.CreatedBy == person {
		return true
	}
	for _, a := range m.guests[e.ID] {
		if a.UserID != nil && *a.UserID == person {
			return true
		}
	}
	return false
}

func (m *memStore) sorted(keep func(*models.Event) bool, limit int) []*models.Event {
	var out []*models.Event
	for _, e := range m.events {
		if keep(e) {
			out = append(out, copyEvent(e))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := startOf(out[i]), startOf(out[j])
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func startOf(e *models.Event) time.Time {
	if e.AllDay() {
		return utcMidnightOf(e.StartDate)
	}
	return *e.StartAt
}

func (m *memStore) rangeOf(_ context.Context, person uuid.UUID, w models.Window, limit int) ([]*models.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sorted(func(e *models.Event) bool {
		return e.DeletedAt == nil && e.SeriesID == nil && m.on(e, person) && (reaches(e, w) || m.movedInto(e, w))
	}, limit), nil
}

func (m *memStore) trashOf(_ context.Context, person uuid.UUID, before *time.Time, limit int) ([]*models.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*models.Event
	for _, e := range m.events {
		if e.CreatedBy == person && e.DeletedAt != nil && (before == nil || e.DeletedAt.Before(*before)) {
			out = append(out, copyEvent(e))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeletedAt.After(*out[j].DeletedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memStore) oooRules(_ context.Context, events []uuid.UUID) (map[uuid.UUID]models.OOORule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uuid.UUID]models.OOORule{}
	for _, id := range events {
		if r, ok := m.ooo[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

func (m *memStore) setOOORule(_ context.Context, event uuid.UUID, r models.OOORule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ooo == nil {
		m.ooo = map[uuid.UUID]models.OOORule{}
	}
	if r.Decline == models.OOODeclineNone {
		delete(m.ooo, event)
	} else {
		m.ooo[event] = r
	}
	return nil
}

func (m *memStore) awayOf(_ context.Context, people []uuid.UUID, w models.Window, limit int) ([]*models.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sorted(func(e *models.Event) bool {
		return slices.Contains(people, e.CreatedBy) && e.Kind == string(EventAway) && e.DeletedAt == nil &&
			e.Status != statusCancelled && e.SeriesID == nil && reaches(e, w)
	}, limit), nil
}

func (m *memStore) placesOf(_ context.Context, people []uuid.UUID, w models.Window, limit int) ([]*models.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sorted(func(e *models.Event) bool {
		return slices.Contains(people, e.CreatedBy) && e.Kind == string(EventWorkingLocation) && e.DeletedAt == nil &&
			e.Status != statusCancelled && e.SeriesID == nil && reaches(e, w)
	}, limit), nil
}

func (m *memStore) zoneHints(_ context.Context, people []uuid.UUID) (map[uuid.UUID]models.ZoneHints, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uuid.UUID]models.ZoneHints{}
	for _, p := range people {
		out[p] = m.hints[p]
	}
	return out, nil
}

// known is everyone but those the test marks as having no account.
func (m *memStore) known(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if !m.noAccount[id] {
			out[id] = true
		}
	}
	return out, nil
}

// membersNow is everyone with an account but those the test marks as no
// members now.
func (m *memStore) membersNow(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if !m.noAccount[id] && !m.notMembers[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (m *memStore) create(_ context.Context, e *models.Event, guests []models.Attendee) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	m.events[e.ID] = copyEvent(e)
	m.guests[e.ID] = slices.Clone(guests)
	return nil
}

func (m *memStore) update(_ context.Context, e *models.Event, expect *int, w guestWrites) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	have := m.events[e.ID]
	if have == nil {
		return 0, models.ErrNotFound
	}
	if expect != nil && have.Sequence != *expect {
		return 0, models.ErrStale
	}
	m.writes++
	next := copyEvent(e)
	next.Sequence, next.DeletedAt, next.DeletedBy = have.Sequence+1, have.DeletedAt, have.DeletedBy
	next.CreatedBy, next.CalendarID, next.CreatedAt, next.Source = have.CreatedBy, have.CalendarID, have.CreatedAt, have.Source
	m.events[e.ID] = next
	m.dropGuests(e.ID, w.remove, w.removeEmails)
	m.addGuests(w.add)
	m.setRoles(w.roles)
	return next.Sequence, nil
}

// setRoles puts guests on their events in the role each names. Called with
// m.mu held.
func (m *memStore) setRoles(guests []models.Attendee) {
	for _, g := range guests {
		k := g.Key()
		for i := range m.guests[g.EventID] {
			if k.Is(m.guests[g.EventID][i]) {
				m.guests[g.EventID][i].Role = g.Role
			}
		}
	}
}

// dropGuests takes people and guests by email off an event, with their
// answers for its instances alone. Called with m.mu held.
func (m *memStore) dropGuests(event uuid.UUID, people []uuid.UUID, emails []string) {
	gone := func(k models.GuestKey) bool {
		if k.UserID != nil {
			return slices.Contains(people, *k.UserID)
		}
		return slices.ContainsFunc(emails, func(e string) bool { return strings.EqualFold(e, k.Email) })
	}
	m.guests[event] = slices.DeleteFunc(m.guests[event], func(a models.Attendee) bool { return gone(a.Key()) })
	m.answered = slices.DeleteFunc(m.answered, func(a models.InstanceAnswer) bool { return a.EventID == event && gone(a.Key()) })
}

// addGuests adds guests not on their event yet. Called with m.mu held.
func (m *memStore) addGuests(guests []models.Attendee) {
	for _, g := range guests {
		k := g.Key()
		if !slices.ContainsFunc(m.guests[g.EventID], func(a models.Attendee) bool { return k.Is(a) }) {
			m.guests[g.EventID] = append(m.guests[g.EventID], g)
		}
	}
}

func (m *memStore) setDeleted(_ context.Context, id uuid.UUID, at *time.Time, by *uuid.UUID, now time.Time, expect *int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	have := m.events[id]
	if have == nil {
		return 0, models.ErrNotFound
	}
	if expect != nil && have.Sequence != *expect {
		return 0, models.ErrStale
	}
	m.writes++
	have.DeletedAt, have.DeletedBy, have.UpdatedAt = at, by, now
	have.Sequence++
	return have.Sequence, nil
}

func (m *memStore) removeGuest(_ context.Context, event, person uuid.UUID, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := len(m.guests[event])
	m.dropGuests(event, []uuid.UUID{person}, nil)
	if len(m.guests[event]) == before {
		return false, nil
	}
	m.writes++
	m.events[event].Sequence++
	m.events[event].UpdatedAt = now
	return true, nil
}

func (m *memStore) overridesOf(_ context.Context, series []uuid.UUID, w models.Window) ([]*models.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*models.Event
	for _, e := range m.events {
		if e.SeriesID == nil || !slices.Contains(series, *e.SeriesID) {
			continue
		}
		near := !e.RecurrenceID.Before(w.From.Add(-48*time.Hour)) && !e.RecurrenceID.After(w.To.Add(48*time.Hour))
		if near || touches(e, w) {
			out = append(out, copyEvent(e))
		}
	}
	return out, nil
}

func (m *memStore) overridesOfSeries(_ context.Context, series uuid.UUID) ([]*models.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*models.Event
	for _, e := range m.events {
		if e.SeriesID != nil && *e.SeriesID == series {
			out = append(out, copyEvent(e))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RecurrenceID.Before(*out[j].RecurrenceID) })
	return out, nil
}

func (m *memStore) overrideAt(_ context.Context, series uuid.UUID, recurrence time.Time) (*models.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if e.SeriesID != nil && *e.SeriesID == series && e.RecurrenceID.Equal(recurrence) {
			return copyEvent(e), nil
		}
	}
	return nil, nil
}

// apply checks the guard and every sequence first, as the transaction would
// roll back, then writes as the model does.
func (m *memStore) apply(_ context.Context, b batch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g := b.guard; g != nil {
		have := m.events[g.id]
		if have == nil {
			return models.ErrNotFound
		}
		overrides := 0
		for _, e := range m.events {
			if e.SeriesID != nil && *e.SeriesID == g.id {
				overrides++
			}
		}
		if (g.sequence != nil && have.Sequence != *g.sequence) || (g.overrides >= 0 && overrides != g.overrides) {
			return models.ErrStale
		}
	}
	for _, e := range b.inserts {
		if m.events[e.ID] != nil {
			return errors.New("memStore: duplicate id")
		}
		for _, o := range m.events {
			if e.SeriesID != nil && o.SeriesID != nil && *o.SeriesID == *e.SeriesID && o.RecurrenceID.Equal(*e.RecurrenceID) {
				return errors.New("memStore: duplicate override")
			}
		}
	}
	for _, u := range b.updates {
		have := m.events[u.e.ID]
		if have == nil && !slices.ContainsFunc(b.inserts, func(e *models.Event) bool { return e.ID == u.e.ID }) {
			return models.ErrNotFound
		}
		if have != nil && u.expect != nil && have.Sequence != *u.expect {
			return models.ErrStale
		}
	}
	for _, e := range b.inserts {
		m.events[e.ID] = copyEvent(e)
		m.writes++
	}
	for _, u := range b.updates {
		have := m.events[u.e.ID]
		next := copyEvent(u.e)
		next.Sequence, next.DeletedAt, next.DeletedBy = have.Sequence+1, have.DeletedAt, have.DeletedBy
		next.CreatedBy, next.CalendarID, next.CreatedAt, next.Source = have.CreatedBy, have.CalendarID, have.CreatedAt, have.Source
		m.events[u.e.ID] = next
		m.writes++
	}
	m.addGuests(b.guests)
	for event, people := range b.drop {
		m.dropGuests(event, people, nil)
	}
	for event, emails := range b.dropEmails {
		m.dropGuests(event, nil, emails)
	}
	m.setRoles(b.roles)
	for _, id := range b.trash {
		if e := m.events[id]; e != nil {
			now := b.now
			e.DeletedAt, e.DeletedBy, e.UpdatedAt = &now, b.by, b.now
			e.Sequence++
		}
	}
	for _, id := range b.restore {
		if e := m.events[id]; e != nil {
			e.DeletedAt, e.DeletedBy, e.UpdatedAt = nil, nil, b.now
			e.Sequence++
		}
	}
	for _, id := range b.folded {
		if e := m.events[id]; e != nil && e.SeriesID != nil {
			delete(m.events, id)
		}
	}
	return nil
}

func (m *memStore) setCallRoom(_ context.Context, id uuid.UUID, room, was string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.events[id]
	if e == nil || e.DeletedAt != nil || e.CallRoom != was {
		return false, nil
	}
	e.CallRoom, e.UpdatedAt, e.Sequence = room, now, e.Sequence+1
	for _, o := range m.events {
		if o.SeriesID != nil && *o.SeriesID == id {
			o.CallRoom = room
		}
	}
	return true, nil
}

func (m *memStore) setGoogleID(_ context.Context, id uuid.UUID, googleID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.events[id]; e != nil {
		e.GoogleEventID = googleID
	}
	return nil
}

func (m *memStore) answer(_ context.Context, event uuid.UUID, rid *time.Time, k models.GuestKey, response, note string, at time.Time, p *models.Proposal) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.IndexFunc(m.guests[event], func(a models.Attendee) bool { return k.Is(a) })
	if i < 0 {
		return false, nil
	}
	mine := func(a models.InstanceAnswer) bool {
		ak := a.Key()
		return a.EventID == event && ((k.UserID != nil && ak.UserID != nil && *k.UserID == *ak.UserID) || (k.UserID == nil && ak.UserID == nil && strings.EqualFold(k.Email, ak.Email)))
	}
	if rid == nil {
		g := &m.guests[event][i]
		g.Response, g.Note, g.RespondedAt, g.Proposal = response, note, &at, p
		m.answered = slices.DeleteFunc(m.answered, mine)
		return true, nil
	}
	m.answered = slices.DeleteFunc(m.answered, func(a models.InstanceAnswer) bool { return mine(a) && a.RecurrenceID.Equal(*rid) })
	m.answered = append(m.answered, models.InstanceAnswer{EventID: event, RecurrenceID: *rid, UserID: k.UserID, Email: k.Email,
		Response: response, Note: note, RespondedAt: at, Proposal: p})
	return true, nil
}

func (m *memStore) clearProposal(_ context.Context, event uuid.UUID, rid *time.Time, k models.GuestKey) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rid == nil {
		for i := range m.guests[event] {
			if g := &m.guests[event][i]; k.Is(*g) && g.Proposal != nil {
				g.Proposal = nil
				return true, nil
			}
		}
		return false, nil
	}
	for i := range m.answered {
		a := &m.answered[i]
		if a.EventID == event && a.RecurrenceID.Equal(*rid) && a.Key().String() == k.String() && a.Proposal != nil {
			a.Proposal = nil
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) instanceAnswers(_ context.Context, series []uuid.UUID, rids []time.Time) ([]models.InstanceAnswer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.InstanceAnswer
	for _, a := range m.answered {
		for i := range series {
			if a.EventID == series[i] && a.RecurrenceID.Equal(rids[i]) {
				out = append(out, a)
				break
			}
		}
	}
	return out, nil
}

func (m *memStore) guestByLink(_ context.Context, hash []byte) (*models.Attendee, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, gs := range m.guests {
		for _, g := range gs {
			if len(g.LinkHash) > 0 && bytes.Equal(g.LinkHash, hash) {
				c := g
				return &c, nil
			}
		}
	}
	return nil, nil
}

// emailGuest is the guest by email on event, or nil. Called with m.mu held.
func (m *memStore) emailGuest(event uuid.UUID, email string) *models.Attendee {
	for i, g := range m.guests[event] {
		if g.UserID == nil && strings.EqualFold(g.Email, email) {
			return &m.guests[event][i]
		}
	}
	return nil
}

func (m *memStore) setInviteState(_ context.Context, event uuid.UUID, email, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g := m.emailGuest(event, email); g != nil {
		g.InviteState = state
	}
	return nil
}

func (m *memStore) setLink(_ context.Context, event uuid.UUID, email string, hash []byte, sealed string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g := m.emailGuest(event, email); g != nil {
		g.LinkHash, g.LinkSealed = hash, sealed
	}
	return nil
}

func (m *memStore) setNudged(_ context.Context, event uuid.UUID, people []uuid.UUID, emails []string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, g := range m.guests[event] {
		if (g.UserID != nil && slices.Contains(people, *g.UserID)) || (g.UserID == nil && slices.ContainsFunc(emails, func(e string) bool { return strings.EqualFold(e, g.Email) })) {
			t := at
			m.guests[event][i].NudgedAt = &t
		}
	}
	return nil
}

func (m *memStore) peopleByEmail(_ context.Context, emails []string) (map[string]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]uuid.UUID{}
	for _, e := range emails {
		if id, ok := m.addresses[strings.ToLower(e)]; ok {
			out[strings.ToLower(e)] = id
		}
	}
	return out, nil
}
