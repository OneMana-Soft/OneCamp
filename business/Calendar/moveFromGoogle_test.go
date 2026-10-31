package Calendar

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// memLedger is sync's ledger in memory, over the store in memory.
type memLedger struct {
	mu        sync.Mutex
	store     *memStore
	now       func() time.Time
	links     map[string]*models.SyncLink
	calendars []models.SyncCalendar
	refs      []models.SyncRef
	progress  []int
	synced    int
	// subs are calendars read from addresses (subscriptions.go).
	subs []models.Subscription
	// refusals are the changes Google didn't take, with their stamps.
	refusals []memRefusal
	// keepCalls and dropCalls count the ledger's ref writes.
	keepCalls, dropCalls int
}

// memRefusal is a refusal as calendar_sync_refusals keeps it.
type memRefusal struct {
	models.SyncRefusal
	at time.Time
}

func newMemLedger(store *memStore, now func() time.Time) *memLedger {
	return &memLedger{store: store, now: now, links: map[string]*models.SyncLink{}}
}

// lastChange is an event's last change, or an instance's of it.
func (m *memLedger) lastChange(id uuid.UUID) time.Time {
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	var last time.Time
	for _, e := range m.store.events {
		if (e.ID == id || (e.SeriesID != nil && *e.SeriesID == id)) && e.UpdatedAt.After(last) {
			last = e.UpdatedAt
		}
	}
	return last
}

func (m *memLedger) syncedLinks(_ context.Context, provider string, limit int) ([]*models.SyncLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*models.SyncLink
	for _, l := range m.links {
		if l.Provider == provider && l.State == models.SyncSynced && len(out) < limit {
			c := *l
			out = append(out, &c)
		}
	}
	return out, nil
}

func (m *memLedger) markSynced(context.Context, uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.synced++
	return nil
}

func (m *memLedger) calendarsOf(_ context.Context, link uuid.UUID) ([]models.SyncCalendar, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.SyncCalendar
	for _, c := range m.calendars {
		if c.LinkID == link {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memLedger) refsOf(_ context.Context, link uuid.UUID) (map[uuid.UUID]models.SyncRef, error) {
	m.store.mu.Lock()
	there := map[uuid.UUID]bool{}
	for id := range m.store.events {
		there[id] = true
	}
	m.store.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uuid.UUID]models.SyncRef{}
	for _, r := range m.refs {
		// An event deleted takes its ref with it, as Postgres cascades.
		if r.LinkID == link && there[r.EventID] {
			out[r.EventID] = r
		}
	}
	return out, nil
}

func (m *memLedger) dropRefs(_ context.Context, link uuid.UUID, events []uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropCalls++
	m.refs = slices.DeleteFunc(m.refs, func(r models.SyncRef) bool { return r.LinkID == link && slices.Contains(events, r.EventID) })
	m.refusals = slices.DeleteFunc(m.refusals, func(r memRefusal) bool { return r.LinkID == link && slices.Contains(events, r.EventID) })
	return nil
}

// changedSince is models.ChangedSince's query, over the store in memory.
func (m *memLedger) changedSince(_ context.Context, link, person uuid.UUID, calendars []uuid.UUID, now time.Time, limit int) ([]*models.Event, error) {
	refs, _ := m.refsOf(context.Background(), link)
	m.store.mu.Lock()
	var rows []*models.Event
	for _, e := range m.store.events {
		if e.SeriesID == nil && e.CreatedBy == person && slices.Contains(calendars, e.CalendarID) {
			rows = append(rows, copyEvent(e))
		}
	}
	m.store.mu.Unlock()
	m.mu.Lock()
	refused := map[uuid.UUID]memRefusal{}
	for _, f := range m.refusals {
		if f.LinkID == link {
			refused[f.EventID] = f
		}
	}
	m.mu.Unlock()
	var out []*models.Event
	for _, e := range rows {
		r, has := refs[e.ID]
		f, hasRefusal := refused[e.ID]
		last := m.lastChange(e.ID)
		due := (!has && e.DeletedAt == nil) || (has && last.After(r.UpdatedAt)) || (hasRefusal && f.RetryAt != nil && !f.RetryAt.After(now))
		waits := hasRefusal && (f.RetryAt == nil || f.RetryAt.After(now)) && !last.After(f.at)
		if due && !waits {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b *models.Event) int {
		if c := a.UpdatedAt.Compare(b.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID.String(), b.ID.String())
	})
	return out[:min(len(out), limit)], nil
}

func (m *memLedger) refusalsOf(_ context.Context, link uuid.UUID, events []uuid.UUID) (map[uuid.UUID]models.SyncRefusal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uuid.UUID]models.SyncRefusal{}
	for _, f := range m.refusals {
		if f.LinkID == link && slices.Contains(events, f.EventID) {
			out[f.EventID] = f.SyncRefusal
		}
	}
	return out, nil
}

func (m *memLedger) keepRefusal(_ context.Context, r models.SyncRefusal, now time.Time) error {
	at := now
	if last := m.lastChange(r.EventID); last.After(at) {
		at = last
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refusals = slices.DeleteFunc(m.refusals, func(f memRefusal) bool { return f.LinkID == r.LinkID && f.EventID == r.EventID })
	m.refusals = append(m.refusals, memRefusal{SyncRefusal: r, at: at})
	return nil
}

func (m *memLedger) breakLink(_ context.Context, id uuid.UUID, why string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.byID(id); l != nil && l.State == models.SyncSynced {
		l.State, l.Error = models.SyncBroken, why
	}
	return nil
}

func (m *memLedger) markReadOnly(_ context.Context, link uuid.UUID, remote string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.calendars {
		if c.LinkID == link && c.RemoteID == remote {
			m.calendars[i].ReadOnly = true
		}
	}
	return nil
}

func (m *memLedger) linkOf(_ context.Context, person uuid.UUID, provider string) (*models.SyncLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.links[person.String()+provider]; l != nil {
		c := *l
		return &c, nil
	}
	return nil, nil
}

func (m *memLedger) startMove(_ context.Context, person uuid.UUID, provider, account string) (*models.SyncLink, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := person.String() + provider
	if l := m.links[k]; l != nil && l.State == models.SyncMoving {
		c := *l
		return &c, false, nil
	}
	l := &models.SyncLink{ID: uuid.New(), PersonID: person, Provider: provider, Account: account, State: models.SyncMoving}
	m.links[k] = l
	c := *l
	return &c, true, nil
}

func (m *memLedger) byID(id uuid.UUID) *models.SyncLink {
	for _, l := range m.links {
		if l.ID == id {
			return l
		}
	}
	return nil
}

func (m *memLedger) setProgress(_ context.Context, id uuid.UUID, p int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID(id).Progress = p
	m.progress = append(m.progress, p)
	return nil
}

func (m *memLedger) finish(_ context.Context, id uuid.UUID, state string, moved, unchanged int, skipped []contentIOBusiness.Skipped, why string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.byID(id)
	l.State, l.Moved, l.Unchanged, l.Error = state, moved, unchanged, why
	if state == models.SyncSynced {
		l.Progress = 100
	}
	l.Skipped, _ = json.Marshal(skipped)
	return nil
}

func (m *memLedger) keepCalendar(_ context.Context, c models.SyncCalendar) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, h := range m.calendars {
		if h.LinkID == c.LinkID && h.RemoteID == c.RemoteID {
			m.calendars[i] = c
			return nil
		}
	}
	m.calendars = append(m.calendars, c)
	return nil
}

func (m *memLedger) ensureLink(_ context.Context, person uuid.UUID, provider string) (*models.SyncLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := person.String() + provider
	if m.links[k] == nil {
		m.links[k] = &models.SyncLink{ID: uuid.New(), PersonID: person, Provider: provider, State: models.SyncSynced, Progress: 100}
	}
	c := *m.links[k]
	return &c, nil
}

func (m *memLedger) sub(link uuid.UUID, remote string) *models.Subscription {
	for i := range m.subs {
		if m.subs[i].LinkID == link && m.subs[i].RemoteID == remote {
			return &m.subs[i]
		}
	}
	return nil
}

func (m *memLedger) addSubscription(_ context.Context, s models.Subscription) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h := m.sub(s.LinkID, s.RemoteID); h != nil {
		if h.CalendarID != nil {
			return false, nil
		}
		*h = s
		return true, nil
	}
	m.subs = append(m.subs, s)
	return true, nil
}

func (m *memLedger) subscriptionsOf(_ context.Context, person uuid.UUID) ([]models.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.Subscription
	for _, s := range m.subs {
		if s.PersonID == person && s.CalendarID != nil {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b models.Subscription) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (m *memLedger) dueSubscriptions(_ context.Context, before time.Time, limit int) ([]models.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.Subscription
	for _, s := range m.subs {
		if s.CalendarID != nil && (s.ReadAt == nil || s.ReadAt.Before(before)) && len(out) < limit {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *memLedger) claimSubscription(_ context.Context, link uuid.UUID, remote string, now, before time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sub(link, remote)
	if s == nil || (s.ReadAt != nil && !s.ReadAt.Before(before)) {
		return false, nil
	}
	at := now
	s.ReadAt = &at
	return true, nil
}

func (m *memLedger) finishSubscriptionRead(_ context.Context, link uuid.UUID, remote string, cursor *string, why string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sub(link, remote); s != nil {
		if cursor != nil {
			s.Cursor = *cursor
		}
		s.Error = why
	}
	return nil
}

func (m *memLedger) dropSubscription(_ context.Context, link uuid.UUID, remote string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = slices.DeleteFunc(m.subs, func(s models.Subscription) bool { return s.LinkID == link && s.RemoteID == remote })
	m.refs = slices.DeleteFunc(m.refs, func(r models.SyncRef) bool { return r.LinkID == link && r.RemoteID == remote })
	return nil
}

func (m *memLedger) keepRefs(_ context.Context, refs []models.SyncRef) error {
	m.mu.Lock()
	m.keepCalls++
	m.mu.Unlock()
	for _, r := range refs {
		r.UpdatedAt = m.now()
		if last := m.lastChange(r.EventID); last.After(r.UpdatedAt) {
			r.UpdatedAt = last
		}
		m.mu.Lock()
		m.refs = slices.DeleteFunc(m.refs, func(h models.SyncRef) bool {
			return h.LinkID == r.LinkID && h.RemoteID == r.RemoteID && h.ExternalID == r.ExternalID
		})
		m.refs = append(m.refs, r)
		m.refusals = slices.DeleteFunc(m.refusals, func(f memRefusal) bool { return f.LinkID == r.LinkID && f.EventID == r.EventID })
		m.mu.Unlock()
	}
	return nil
}

// fakeGoogle is a Google link with two calendars, Sam's own and a team's.
type fakeGoogle struct {
	pulls int
}

func (*fakeGoogle) Provider() string { return "google" }

func (*fakeGoogle) Calendars(_ context.Context, l calendarSyncBusiness.Link) ([]calendarSyncBusiness.Remote, error) {
	return []calendarSyncBusiness.Remote{{Link: l, ID: "sam@outlook.com", Name: "sam@outlook.com", Primary: true, Zone: "Europe/London"},
		{Link: l, ID: "team@group", Name: "Team offsites"}, {Link: l, ID: "broken", Name: "Broken"}}, nil
}

func (f *fakeGoogle) Pull(_ context.Context, cal calendarSyncBusiness.Remote, cursor string) ([]calendarSyncBusiness.Change, string, error) {
	f.pulls++
	if cal.ID == "broken" {
		return nil, "", errors.New("403")
	}
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	switch cal.ID {
	case "sam@outlook.com":
		return []calendarSyncBusiness.Change{
			{ExternalID: "g1", ETag: "e1", Event: ICS.Event{UID: "standup@google.com", Summary: "Standup", Start: start, End: start.Add(15 * time.Minute), Zone: "Europe/London", RRule: "FREQ=DAILY;COUNT=5",
				Attendees: []ICS.Attendee{{Address: ICS.Address{Email: "maya@example.com"}, PartStat: ICS.Accepted}}}},
			{ExternalID: "g2", Deleted: true, Event: ICS.Event{UID: "gone@google.com"}},
		}, "tok-sam", nil
	default:
		return []calendarSyncBusiness.Change{{ExternalID: "t1", ETag: "e9", Event: ICS.Event{UID: "offsite@google.com", Summary: "Offsite", StartDate: "2026-11-05", EndDate: "2026-11-07"}}}, "tok-team", nil
	}
}

func (*fakeGoogle) Push(context.Context, calendarSyncBusiness.Op) (calendarSyncBusiness.Ref, error) {
	return calendarSyncBusiness.Ref{}, nil
}
func (*fakeGoogle) Watch(context.Context, calendarSyncBusiness.Remote) (calendarSyncBusiness.Watch, error) {
	return calendarSyncBusiness.Watch{}, nil
}
func (*fakeGoogle) Stop(context.Context, calendarSyncBusiness.Watch) error { return nil }

func withGoogle(t *testing.T, h *harness, g *fakeGoogle, linked bool) *memLedger {
	t.Helper()
	ledger := newMemLedger(h.store, func() time.Time { return h.clock })
	h.s.ledger, h.s.moveNow = ledger, true
	prevProvider, prevAccount := syncProvider, googleAccountOf
	syncProvider = func(string) (calendarSyncBusiness.Adapter, bool) { return g, true }
	googleAccountOf = func(context.Context, *model.UserInfo) (string, bool, error) { return "sam@outlook.com", linked, nil }
	t.Cleanup(func() { syncProvider, googleAccountOf = prevProvider, prevAccount })
	return ledger
}

// One click moves every Google calendar: Sam's own into their calendar, a
// team's into one of its name, with where each event is at Google kept; a
// calendar Google won't give is named; asked again, nothing comes twice.
func TestMovingFromGoogleBringsEveryCalendar(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := &fakeGoogle{}
	ledger := withGoogle(t, h, g, true)
	st, err := h.s.MoveFromGoogle(context.Background(), sam)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != models.SyncSynced || st.Moved != 2 || st.Progress != 100 || len(st.Skipped) != 1 || !strings.Contains(st.Skipped[0].What, "Broken") {
		t.Fatalf("status %+v", st)
	}
	personal := h.store.calendars[personOf(sam)]
	team := h.store.imported[personOf(sam).String()+"/Team offsites"]
	var inPersonal, inTeam int
	for _, e := range h.store.events {
		switch e.CalendarID {
		case personal:
			inPersonal++
		case team:
			inTeam++
		}
	}
	if inPersonal != 1 || inTeam != 1 {
		t.Fatalf("own %d, team %d", inPersonal, inTeam)
	}
	if len(ledger.refs) != 2 || len(ledger.calendars) != 2 || ledger.calendars[0].Cursor != "tok-sam" || *ledger.calendars[0].CalendarID != personal {
		t.Fatalf("refs %+v, calendars %+v", ledger.refs, ledger.calendars)
	}
	if len(h.tells.tells) != 0 {
		t.Fatal("a move told people")
	}
	again, err := h.s.MoveFromGoogle(context.Background(), sam)
	if err != nil || again.Moved != 0 || again.Unchanged != 2 {
		t.Fatalf("again %+v %v", again, err)
	}
	read, _ := h.s.SyncStatus(context.Background(), sam)
	if read.State != models.SyncSynced || read.Account != "sam@outlook.com" {
		t.Fatalf("status read %+v", read)
	}
}

func TestAMoveNeedsGoogleConnected(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	withGoogle(t, h, &fakeGoogle{}, false)
	if _, err := h.s.MoveFromGoogle(context.Background(), sam); !errors.Is(err, ErrGoogleNotLinked) {
		t.Fatalf("got %v", err)
	}
	if st, _ := h.s.SyncStatus(context.Background(), sam); st.State != "none" {
		t.Fatalf("status %+v", st)
	}
}
