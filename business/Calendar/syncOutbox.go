package Calendar

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// Google writes go through the Durable Outbox (gcal-18 H4+H5, #48). A change
// to an event here asks for one "calendar.sync" entry (syncEntriesFor), in
// the same write as its search entries (eventsChanged, searchEntriesChangedIn).
// Its sink (runSyncSink) sends the event at once to each of the owner's
// linked calendars, through the sweep's own push (pushChanged, pushOne):
//   - a refusal (calendarSyncBusiness.ErrRefused, a 4xx not of the account)
//     is kept in calendar_sync_refusals as the sweep keeps it, and the sink
//     is done: it holds until the event changes, never an outbox retry;
//   - a revoked account (calendarSyncBusiness.ErrRevoked) breaks the link,
//     which asks the person to connect it again, and the sink is done;
//   - a passing failure (the network, a 5xx, a 429) is answered, so the
//     outbox backs off; dead after outboxBusiness.MaxAttempts, it is the
//     system check's to count, and the sweep still sends the change.
//
// The sweep stays, for what an entry missed and for CALENDAR_SYNC_OUTBOX=off:
// the refs make what one sent a no-op for the other (changedSince).
const (
	syncTopic       = "calendar.sync"
	syncSink        = "calendar.sync.push"
	kindSyncChanged = "event.changed"
)

// pushing keeps one push to a link at a time, the sweep's or a sink's: two
// at once would both create an event the provider hasn't seen.
var pushing helpers.KeyedLock

// linked is who has a two-way link in step, as this process has seen it
// (R384-N1): an event change asks for a sync entry only when a person it
// names (its actor, its subjects) is among them, so a change by a person
// with no link writes no calendar.sync row and spends no delivery. It costs
// no statement: each sweep's pass of a link in step adds its person
// (syncLink), a move that finishes synced adds its person, and a
// delivery's own read of its owner's links (syncedLinksOf) adds or drops
// the owner. Another instance's new link is seen at this one's next sweep
// of it; until then the minute's sweep sends that person's changes
// (changedSince), so nothing is lost.
var linked = struct {
	sync.RWMutex
	people map[uuid.UUID]bool
}{people: map[uuid.UUID]bool{}}

// markLinked keeps whether person has a two-way link in step.
func markLinked(person uuid.UUID, yes bool) {
	linked.Lock()
	defer linked.Unlock()
	if yes {
		linked.people[person] = true
	} else {
		delete(linked.people, person)
	}
}

// anyLinked is whether any of people (uuids) has a two-way link in step.
func anyLinked(people []string) bool {
	linked.RLock()
	defer linked.RUnlock()
	for _, p := range people {
		if id, err := uuid.Parse(p); err == nil && linked.people[id] {
			return true
		}
	}
	return false
}

// twoWayIn is whether any of links syncs both ways.
func twoWayIn(links []*models.SyncLink) bool {
	return slices.ContainsFunc(links, func(l *models.SyncLink) bool {
		return slices.Contains(calendarSyncBusiness.Adapters.TwoWay(), l.Provider)
	})
}

// syncEntriesFor is one sync entry per event of these keys (searchKeys):
// none with the switch off (helpers.CalendarSyncThroughOutbox).
func syncEntriesFor(keys []string) []outboxBusiness.Entry {
	if !helpers.CalendarSyncThroughOutbox() {
		return nil
	}
	es := make([]outboxBusiness.Entry, 0, len(keys))
	for _, k := range keys {
		es = append(es, outboxBusiness.Entry{
			Topic: syncTopic, Kind: kindSyncChanged, Key: "sync:" + k,
			Entities: map[string]string{"event": k},
			Sinks:    []string{syncSink},
		})
	}
	return es
}

// runSyncSink sends the entry's event, as it is now, to each of its owner's
// links the sweep keeps in step. It acts as the sweep does, for the link's
// person: only that person's events, on the calendars the link sends.
func (s *service) runSyncSink(ctx context.Context, ev outboxBusiness.Event) error {
	id, err := uuid.Parse(ev.Entities["event"])
	if err != nil || !helpers.CalendarInPostgres() || s.ready(ctx) != nil {
		return nil
	}
	e, err := s.store.get(ctx, id)
	if err == nil && e != nil && e.SeriesID != nil {
		e, err = s.store.get(ctx, *e.SeriesID) // an occurrence goes with its series
	}
	if err != nil || e == nil {
		return err
	}
	links, err := s.ledger.syncedLinksOf(ctx, e.CreatedBy)
	if err != nil {
		return err
	}
	markLinked(e.CreatedBy, twoWayIn(links))
	personal := s.personalOf(ctx, e.CreatedBy) // the owner's, read once for all their links
	var errs []error
	for _, link := range links {
		if err := s.pushEventTo(ctx, link, e.ID, personal); err != nil {
			errs = append(errs, fmt.Errorf("%s link %s: %w", link.Provider, link.ID, err))
		}
	}
	return errors.Join(errs...)
}

// pushEventTo sends the event id to one link, if it is the link's to send
// now (changedAmong: as the sweep's changedSince would pick it). personal
// is the link's person's own calendar (personalOf).
func (s *service) pushEventTo(ctx context.Context, link *models.SyncLink, id uuid.UUID, personal func() (uuid.UUID, error)) error {
	if !slices.Contains(calendarSyncBusiness.Adapters.TwoWay(), link.Provider) {
		return nil
	}
	provider, ok := syncProvider(link.Provider)
	if !ok {
		return nil
	}
	if !pushing.Acquire(link.ID.String(), 1) {
		return nil // the sweep is sending this link now: it sends the change (changedSince), no attempt spent
	}
	defer pushing.Release(link.ID.String())
	cals, err := s.ledger.calendarsOf(ctx, link.ID)
	if err != nil {
		return err
	}
	held := map[string]bool{} // what the sweep would hold: not read whole yet
	for _, c := range cals {
		held[c.RemoteID] = unread(c)
	}
	remotes, calendars := s.remotesOf(link, cals, held, personal)
	changed, err := s.ledger.changedAmong(ctx, link.ID, link.PersonID, calendars, []uuid.UUID{id}, s.now())
	if err != nil || len(changed) == 0 {
		return err
	}
	refs, err := s.ledger.refsAmong(ctx, link.ID, []uuid.UUID{id})
	if err != nil {
		return err
	}
	err = s.pushChanged(ctx, provider, link, remotes, refs, changed, true)
	if errors.Is(err, calendarSyncBusiness.ErrRevoked) {
		return s.breakLink(ctx, link, err)
	}
	return err
}
