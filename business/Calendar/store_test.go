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
	// addresses are the accounts' addresses, in lower case.
	addresses map[string]uuid.UUID
	writes    int
}

func newMemStore() *memStore {
	return &memStore{events: map[uuid.UUID]*models.Event{}, guests: map[uuid.UUID][]models.Attendee{},
		calendars: map[uuid.UUID]uuid.UUID{}, hints: map[uuid.UUID]models.ZoneHints{}, noAccount: map[uuid.UUID]bool{},
		addresses: map[string]uuid.UUID{}}
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

func (m *memStore) get(_ context.Context, id uuid.UUID) (*models.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.events[id]; e != nil {
		return copyEvent(e), nil
	}
	return nil, nil
}

func (m *memStore) getMany(ctx context.Context, ids []uuid.UUID) ([]*models.Event, error) {
	var out []*models.Event
	for _, id := range ids {
		if e, _ := m.get(ctx, id); e != nil {
			out = append(out, e)
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

func (m *memStore) awayOf(_ context.Context, people []uuid.UUID, w models.Window, limit int) ([]*models.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sorted(func(e *models.Event) bool {
		return slices.Contains(people, e.CreatedBy) && e.Kind == string(EventAway) && e.DeletedAt == nil &&
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

func (m *memStore) setGoogleID(_ context.Context, id uuid.UUID, googleID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.events[id]; e != nil {
		e.GoogleEventID = googleID
	}
	return nil
}

func (m *memStore) answer(_ context.Context, event uuid.UUID, rid *time.Time, k models.GuestKey, response, note string, at time.Time) (bool, error) {
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
		g.Response, g.Note, g.RespondedAt = response, note, &at
		m.answered = slices.DeleteFunc(m.answered, mine)
		return true, nil
	}
	m.answered = slices.DeleteFunc(m.answered, func(a models.InstanceAnswer) bool { return mine(a) && a.RecurrenceID.Equal(*rid) })
	m.answered = append(m.answered, models.InstanceAnswer{EventID: event, RecurrenceID: *rid, UserID: k.UserID, Email: k.Email,
		Response: response, Note: note, RespondedAt: at})
	return true, nil
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
