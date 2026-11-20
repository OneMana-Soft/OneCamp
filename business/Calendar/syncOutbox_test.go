package Calendar

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	outboxModels "github.com/akashc777/OneCamp/models/postgres/Outbox"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// syncDelivery is the outbox event a change to the event id asks for, as
// the drain hands it to the sync sink.
func syncDelivery(t *testing.T, id string) outboxBusiness.Event {
	t.Helper()
	es := syncEntriesFor([]string{id})
	if len(es) != 1 {
		t.Fatalf("a change to one event asks for %d sync entries, want 1", len(es))
	}
	e := es[0]
	return outboxBusiness.Event{ID: 1, EventID: uuid.NewString(), Topic: e.Topic, Kind: e.Kind, Key: e.Key, Entities: e.Entities, Attempt: 1}
}

// googleLinkOf is who's Google link in the ledger.
func googleLinkOf(t *testing.T, ledger *memLedger, who *model.UserInfo) *models.SyncLink {
	t.Helper()
	l, err := ledger.linkOf(context.Background(), who.UserPostgresInfo.Id, "google")
	if err != nil || l == nil {
		t.Fatalf("link %+v %v", l, err)
	}
	return l
}

// The sync sink sends a changed event to the owner's linked calendar at
// once, through the sweep's own push; delivered again, it sends nothing, and
// the sweep after it sends nothing either: the refs say it is in step.
func TestTheSyncSinkSendsAChangedEventOnceAndTheSweepDoesntSendItAgain(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	_, sweep := syncWithGoogle(t, h, sam, g)
	e := h.create(sam, "Pricing review")
	ev := syncDelivery(t, e.ID)
	if err := h.s.runSyncSink(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if p := g.pushed(); !slices.Equal(p, []string{"create Pricing review"}) {
		t.Fatalf("the sink pushed %v", p)
	}
	if err := h.s.runSyncSink(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("delivered again, the sink pushed %v", p)
	}
	sweep()
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("the sweep pushed again what the sink pushed: %v", p)
	}
	title, seq := "Pricing review, moved", e.Sequence
	if _, err := h.s.Update(context.Background(), sam, e.ID, EventPatch{Title: &title, Sequence: &seq}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.runSyncSink(context.Background(), syncDelivery(t, e.ID)); err != nil {
		t.Fatal(err)
	}
	if p := g.pushed(); !slices.Equal(p, []string{"update Pricing review, moved"}) {
		t.Fatalf("after a change, the sink pushed %v", p)
	}
}

// Rule (a): a change Google refuses is kept as refused, as the sweep keeps
// it, and the sink is done with it: no outbox retry, never dead.
func TestTheSyncSinkKeepsAGoogleRefusalAndIsDone(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	g.refuse = func(calendarSyncBusiness.Op) error {
		return fmt.Errorf("%w: 400 bad request", calendarSyncBusiness.ErrRefused)
	}
	ledger, _ := syncWithGoogle(t, h, sam, g)
	e := h.create(sam, "Pricing review (draft)")
	if err := h.s.runSyncSink(context.Background(), syncDelivery(t, e.ID)); err != nil {
		t.Fatalf("a refusal asked the outbox to retry: %v", err)
	}
	link := googleLinkOf(t, ledger, sam)
	id := uuid.MustParse(e.ID)
	refused, err := ledger.refusalsOf(context.Background(), link.ID, []uuid.UUID{id})
	if err != nil || refused[id].Tries != 1 {
		t.Fatalf("refusals %+v %v", refused, err)
	}
	if link.State != models.SyncSynced {
		t.Fatalf("a refusal broke the link: %+v", link)
	}
}

// Rule (b): a revoked account breaks the link, which asks the person to
// connect it again, and the sink is done: it never dead-letters.
func TestTheSyncSinkBreaksARevokedLinkAndIsDone(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	syncWithGoogle(t, h, sam, g)
	e := h.create(sam, "Pricing review")
	g.revoked = true
	if err := h.s.runSyncSink(context.Background(), syncDelivery(t, e.ID)); err != nil {
		t.Fatalf("a revoked account asked the outbox to retry: %v", err)
	}
	st, err := h.s.SyncStatus(context.Background(), sam, "google")
	if err != nil || st.State != models.SyncBroken || st.Error != syncBrokenWhy {
		t.Fatalf("status %+v %v", st, err)
	}
}

// Rule (c): a passing failure (a 5xx, the network, a 429) asks the outbox to
// try again with its backoff; the sweep still has the change, so a dead row
// never loses it.
func TestTheSyncSinkRetriesAPassingFailure(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	down := true
	g.refuse = func(calendarSyncBusiness.Op) error {
		if down {
			return errors.New("503 backend error")
		}
		return nil
	}
	ledger, sweep := syncWithGoogle(t, h, sam, g)
	e := h.create(sam, "Pricing review")
	if err := h.s.runSyncSink(context.Background(), syncDelivery(t, e.ID)); err == nil {
		t.Fatal("a 503 didn't ask the outbox to retry")
	}
	if l := googleLinkOf(t, ledger, sam); l.State != models.SyncSynced {
		t.Fatalf("a 503 broke the link: %+v", l)
	}
	g.pushed()
	down = false
	h.clock = h.clock.Add(syncRetries[0] + syncRetries[0]/4)
	sweep()
	if p := g.pushed(); !slices.Equal(p, []string{"create Pricing review"}) {
		t.Fatalf("the sweep, after the change's wait, pushed %v", p)
	}
}

// One sync entry per changed event (not per link, not per page), beside its
// search entries in the same enqueue; none with the switch off. A change
// read from a provider or an address asks for none: it came from there.
func TestAChangedEventAsksForOneSyncEntryUnlessTheSwitchIsOff(t *testing.T) {
	var asked []outboxBusiness.Entry
	prev := enqueueSearch
	enqueueSearch = func(_ context.Context, es ...outboxBusiness.Entry) error {
		asked = append(asked, es...)
		return nil
	}
	t.Cleanup(func() { enqueueSearch = prev })
	a, b := uuid.NewString(), uuid.NewString()
	owner := uuid.New() // with a two-way link in step (R384-N1)
	markLinked(owner, true)
	t.Cleanup(func() { markLinked(owner, false) })
	by := []string{owner.String()}
	syncOf := func() (n []string) {
		for _, e := range asked {
			if e.Topic == syncTopic {
				if e.Key != "sync:"+e.Entities["event"] || !slices.Equal(e.Sinks, []string{syncSink}) {
					t.Errorf("sync entry %+v", e)
				}
				n = append(n, e.Entities["event"])
			}
		}
		return n
	}

	eventsChanged(context.Background(), []string{a, strings.ToUpper(a), b}, by)
	if got := syncOf(); !slices.Equal(got, []string{a, b}) {
		t.Fatalf("sync entries for %v, want one each for %v", got, []string{a, b})
	}
	if len(asked) != 3 {
		t.Fatalf("asked %d entries, want 1 search + 2 sync", len(asked))
	}

	asked = nil
	SearchEntriesChanged(context.Background(), []string{a})
	if got := syncOf(); len(got) != 0 {
		t.Fatalf("a change read from a provider asked for sync entries %v", got)
	}

	asked = nil
	t.Setenv(helpers.CalendarSyncOutboxEnv, "off")
	eventsChanged(context.Background(), []string{a, b}, by)
	if got := syncOf(); len(got) != 0 || len(asked) != 1 {
		t.Fatalf("with the switch off, asked %+v", asked)
	}
}

// A purge (retention's removal, searchEntriesChangedIn) adds its events'
// search entries in its own transaction, through AddMany, never in a write of
// their own, and asks for no sync entry: the purge hard-deletes the events,
// so a sync delivery would find nothing to send (CR-384 R2-F1).
func TestAPurgesTransactionAddsItsSearchEntriesAndNoSyncEntry(t *testing.T) {
	var enqueued, added []outboxBusiness.Entry
	var on any
	prevE, prevA := enqueueSearch, addSearch
	enqueueSearch = func(_ context.Context, es ...outboxBusiness.Entry) error {
		enqueued = append(enqueued, es...)
		return nil
	}
	addSearch = func(_ context.Context, ex outboxModels.Execer, es []outboxBusiness.Entry) ([]int64, error) {
		on, added = ex, append(added, es...)
		ids := make([]int64, len(es))
		for i := range ids {
			ids[i] = int64(i + 1)
		}
		return ids, nil
	}
	t.Cleanup(func() { enqueueSearch, addSearch = prevE, prevA })
	tx := &sql.Tx{}
	a := uuid.NewString()
	if _, err := searchEntriesChangedIn(pgexec.WithTx(context.Background(), tx), []string{a}); err != nil {
		t.Fatal(err)
	}
	search, sync := 0, 0
	for _, e := range added {
		switch {
		case e.Topic == searchTopic && e.Entities["ids"] == a:
			search++
		case e.Topic == syncTopic:
			sync++
		}
	}
	if search != 1 || on != any(tx) {
		t.Fatalf("added %d search entries on %v, want 1 in the purge's transaction", search, on)
	}
	if sync != 0 {
		t.Fatalf("a purge asked for %d sync entries, want none: its events are gone", sync)
	}
	if len(added) != 1 {
		t.Fatalf("a purge added %+v, want its one search entry", added)
	}
	if len(enqueued) != 0 {
		t.Fatalf("enqueued apart from the transaction: %+v", enqueued)
	}
}

// calls counts what a delivery asks of the store and the sync ledger: the
// counting seam over memStore and memLedger (R384-F2).
type calls map[string]int

type countingStore struct {
	store
	n calls
}

func (c countingStore) get(ctx context.Context, id uuid.UUID) (*models.Event, error) {
	c.n["get"]++
	return c.store.get(ctx, id)
}
func (c countingStore) getMany(ctx context.Context, ids []uuid.UUID) ([]*models.Event, error) {
	c.n["getMany"]++
	return c.store.getMany(ctx, ids)
}
func (c countingStore) personalCalendar(ctx context.Context, owner uuid.UUID, tz string) (uuid.UUID, error) {
	c.n["personalCalendar"]++
	return c.store.personalCalendar(ctx, owner, tz)
}
func (c countingStore) zoneHints(ctx context.Context, people []uuid.UUID) (map[uuid.UUID]models.ZoneHints, error) {
	c.n["zoneHints"]++
	return c.store.zoneHints(ctx, people)
}
func (c countingStore) attendees(ctx context.Context, events []uuid.UUID) (map[uuid.UUID][]models.Attendee, error) {
	c.n["attendees"]++
	return c.store.attendees(ctx, events)
}
func (c countingStore) overridesOf(ctx context.Context, series []uuid.UUID, w models.Window) ([]*models.Event, error) {
	c.n["overridesOf"]++
	return c.store.overridesOf(ctx, series, w)
}

type countingLedger struct {
	syncLedger
	n calls
}

func (c countingLedger) syncedLinksOf(ctx context.Context, person uuid.UUID) ([]*models.SyncLink, error) {
	c.n["syncedLinksOf"]++
	return c.syncLedger.syncedLinksOf(ctx, person)
}
func (c countingLedger) calendarsOf(ctx context.Context, link uuid.UUID) ([]models.SyncCalendar, error) {
	c.n["calendarsOf"]++
	return c.syncLedger.calendarsOf(ctx, link)
}
func (c countingLedger) changedAmong(ctx context.Context, link, person uuid.UUID, calendars, events []uuid.UUID, now time.Time) ([]*models.Event, error) {
	c.n["changedAmong"]++
	return c.syncLedger.changedAmong(ctx, link, person, calendars, events, now)
}
func (c countingLedger) changedSince(ctx context.Context, link, person uuid.UUID, calendars []uuid.UUID, now time.Time, limit int) ([]*models.Event, error) {
	c.n["changedSince"]++
	return c.syncLedger.changedSince(ctx, link, person, calendars, now, limit)
}
func (c countingLedger) refsAmong(ctx context.Context, link uuid.UUID, events []uuid.UUID) (map[uuid.UUID]models.SyncRef, error) {
	c.n["refsAmong"]++
	return c.syncLedger.refsAmong(ctx, link, events)
}
func (c countingLedger) refsOf(ctx context.Context, link uuid.UUID) (map[uuid.UUID]models.SyncRef, error) {
	c.n["refsOf"]++
	return c.syncLedger.refsOf(ctx, link)
}
func (c countingLedger) refusalsOf(ctx context.Context, link uuid.UUID, events []uuid.UUID) (map[uuid.UUID]models.SyncRefusal, error) {
	c.n["refusalsOf"]++
	return c.syncLedger.refusalsOf(ctx, link, events)
}
func (c countingLedger) keepRefs(ctx context.Context, refs []models.SyncRef) error {
	c.n["keepRefs"]++
	return c.syncLedger.keepRefs(ctx, refs)
}
func (c countingLedger) keepRefusal(ctx context.Context, r models.SyncRefusal, now time.Time) (bool, error) {
	c.n["keepRefusal"]++
	return c.syncLedger.keepRefusal(ctx, r, now)
}
func (c countingLedger) dropRefs(ctx context.Context, link uuid.UUID, events []uuid.UUID) error {
	c.n["dropRefs"]++
	return c.syncLedger.dropRefs(ctx, link, events)
}
func (c countingLedger) breakLink(ctx context.Context, id uuid.UUID, why string) error {
	c.n["breakLink"]++
	return c.syncLedger.breakLink(ctx, id, why)
}
func (c countingLedger) markReadOnly(ctx context.Context, link uuid.UUID, remote string) error {
	c.n["markReadOnly"]++
	return c.syncLedger.markReadOnly(ctx, link, remote)
}
func (c countingLedger) markSynced(ctx context.Context, id uuid.UUID, readErr string) error {
	c.n["markSynced"]++
	return c.syncLedger.markSynced(ctx, id, readErr)
}

// counted has the service ask the store and ledger through the counting
// seam, and answers the count of one delivery of id.
func counted(t *testing.T, h *harness) func(id string) calls {
	t.Helper()
	n := calls{}
	h.s.store = countingStore{store: h.s.store, n: n}
	h.s.ledger = countingLedger{syncLedger: h.s.ledger, n: n}
	return func(id string) calls {
		t.Helper()
		clear(n)
		if err := h.s.runSyncSink(context.Background(), syncDelivery(t, id)); err != nil {
			t.Fatal(err)
		}
		return maps.Clone(n)
	}
}

// outlookLinkOf gives who a second synced two-way link (Outlook) to the same
// fake, its primary calendar read whole: the personal calendar goes there.
func outlookLinkOf(t *testing.T, ledger *memLedger, who *model.UserInfo) {
	t.Helper()
	ctx := context.Background()
	l, _, err := ledger.startMove(ctx, who.UserPostgresInfo.Id, "microsoft", "sam@example.com")
	if err == nil {
		err = ledger.finish(ctx, l.ID, models.SyncSynced, 0, 0, nil, "")
	}
	if err == nil {
		err = ledger.keepCalendar(ctx, models.SyncCalendar{LinkID: l.ID, RemoteID: "outlook-primary", Primary: true, Cursor: "c1"})
	}
	if err != nil {
		t.Fatal(err)
	}
}

// R384-F2, R384-F3: one delivery's store and ledger calls, pinned. The
// owner's zone and personal calendar are read once a delivery, however many
// links; each two-way link costs its calendars, the change test, its refs
// and refusals, then one write: keepRefs on a send, keepRefusal on a
// refusal. Delivered again, each link stops at the change test; an owner
// with no link stops at their links.
func TestOneSyncDeliveryAsksTheStoreThisMuch(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	ledger, _ := syncWithGoogle(t, h, sam, g)
	outlookLinkOf(t, ledger, sam)
	e := h.create(sam, "Pricing review")
	deliver := counted(t, h)

	want := calls{"get": 1, "syncedLinksOf": 1, "zoneHints": 1, "personalCalendar": 1,
		"calendarsOf": 2, "changedAmong": 2, "refsAmong": 2, "attendees": 2, "overridesOf": 2, "refusalsOf": 2, "keepRefs": 2}
	if got := deliver(e.ID); !maps.Equal(got, want) {
		t.Fatalf("a send to two links asked\n %v\nwant\n %v", got, want)
	}
	if p := g.pushed(); len(p) != 2 {
		t.Fatalf("the sink pushed %v, want a create to each link", p)
	}
	want = calls{"get": 1, "syncedLinksOf": 1, "zoneHints": 1, "personalCalendar": 1, "calendarsOf": 2, "changedAmong": 2}
	if got := deliver(e.ID); !maps.Equal(got, want) {
		t.Fatalf("delivered again, it asked\n %v\nwant\n %v", got, want)
	}

	g.refuse = func(calendarSyncBusiness.Op) error {
		return fmt.Errorf("%w: 400 bad request", calendarSyncBusiness.ErrRefused)
	}
	refused := h.create(sam, "Pricing review (draft)")
	want = calls{"get": 1, "syncedLinksOf": 1, "zoneHints": 1, "personalCalendar": 1,
		"calendarsOf": 2, "changedAmong": 2, "refsAmong": 2, "attendees": 2, "overridesOf": 2, "refusalsOf": 2, "keepRefusal": 2}
	if got := deliver(refused.ID); !maps.Equal(got, want) {
		t.Fatalf("a refusal by two links asked\n %v\nwant\n %v", got, want)
	}

	ana := h.member("Ana")
	hers := h.create(ana, "Ana's 1:1")
	want = calls{"get": 1, "syncedLinksOf": 1}
	if got := deliver(hers.ID); !maps.Equal(got, want) {
		t.Fatalf("an owner with no link: it asked\n %v\nwant\n %v", got, want)
	}
}

// R384-N1: no link, no row. A change by a person with no two-way link in
// step asks for its search entry alone, in the one write it always makes,
// and reads nothing to decide; a change by a person with one asks for one
// sync entry with it.
func TestAnEventChangeAsksForASyncEntryOnlyWhenItsOwnerHasALink(t *testing.T) {
	h := newHarness(t)
	sam, ana := h.member("Sam"), h.member("Ana")
	syncWithGoogle(t, h, sam, newLiveGoogle(func() time.Time { return h.clock }))
	mine, hers := h.create(sam, "Pricing review"), h.create(ana, "Ana's 1:1")
	var asked []outboxBusiness.Entry
	writes := 0
	prev := enqueueSearch
	enqueueSearch = func(_ context.Context, es ...outboxBusiness.Entry) error {
		writes++
		asked = append(asked, es...)
		return nil
	}
	t.Cleanup(func() { enqueueSearch = prev })
	n := calls{}
	h.s.store = countingStore{store: h.s.store, n: n}
	h.s.ledger = countingLedger{syncLedger: h.s.ledger, n: n}
	change := func(who *model.UserInfo, id string) (syncs int) {
		t.Helper()
		asked, writes = nil, 0
		eventChangedSink(context.Background(), resourceEventBusiness.Event{
			Kind: resourceEventBusiness.CalendarEventUpdated, ResourceKind: KindEvent, ResourceUUID: id,
			Actor: resourceEventBusiness.Actor{UserUUID: who.UserPostgresInfo.Id.String()},
		})
		if writes != 1 || len(n) != 0 {
			t.Fatalf("a change wrote %d times and read %v; want its one write and no read", writes, n)
		}
		for _, e := range asked {
			if e.Topic == syncTopic {
				syncs++
			}
		}
		return syncs
	}
	if got := change(ana, hers.ID); got != 0 {
		t.Fatalf("a change by a person with no link asked for %d sync entries, want 0", got)
	}
	if got := change(sam, mine.ID); got != 1 {
		t.Fatalf("a change by a person with a Google link asked for %d sync entries, want 1", got)
	}
}
