package Calendar

// Two-way sync with Google (calendar B6): once a move has finished, every
// minute each of the link's calendars is asked for what changed since its
// cursor, and Google's changes are written here; then what changed here
// since it was last in step (sync's ledger) is sent to Google. Both ways
// carry series and the instances changed on their own, so a team can keep
// working in either while it switches.
//
//   - Nothing echoes: what was just written to match one side is stamped as
//     in step (models.KeepSyncRefs), a change that leaves an event as it was
//     writes nothing, and Google's copy of what OneCamp sent comes back
//     with the version OneCamp kept.
//   - When both sides changed one event, the later change wins.
//   - Only the person's own events are sent (they organise them); an event
//     they're a guest on is the organiser's to change.
//   - Durable without a queue: what is still to send is read from the store
//     (an event changed after its ref), so a crash or a Google outage only
//     delays it to the next minute.
//   - Nothing holds the rest up (C9): a change Google didn't take waits, for
//     a while (syncRetries) or, refused for good, until the event changes;
//     nothing is sent to a calendar the person may only read; and when Google
//     stops answering for the account, the link is broken until they connect
//     Google again.

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"time"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	workGraph "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// How much one sweep does: links, and events sent per link.
const (
	syncLinksPerSweep = 50
	syncPushPerLink   = 200
	syncEvery         = time.Minute
)

// syncStaleAfter is how long a synced link may go without a pass that read
// all of its calendars before its status says it stopped following the
// provider (SyncStatus.StaleSince): thirty sweeps. A provider's passing
// trouble (a rate limit's wait, a few 5xx) clears within a few, and a
// sweep reaches syncLinksPerSweep links per provider, so up to 1,500 links
// a provider each come round well inside it.
const syncStaleAfter = 30 * syncEvery

// syncRetries is how long a change Google didn't take waits before it is
// sent again, by how many times it has failed (helpers.ScheduleWait, with
// syncRetryJitter more at random).
var syncRetries = []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour}

const (
	syncRetryJitter = 0.25
	// syncRefusedTries is how many times a change Google refused is sent
	// before it waits until the event changes.
	syncRefusedTries = 3
)

// syncBrokenWhy is what a person reads of a link Google stopped answering
// for; syncBrokenWhyMicrosoft of one Microsoft did.
const (
	syncBrokenWhy          = "Google stopped answering for this account. Connect it again."
	syncBrokenWhyMicrosoft = "The Microsoft account isn't linked any more: connect it again from Calendars."
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "calendar.sync.google", Every: syncEvery, Run: func(ctx context.Context) error {
		if !helpers.CalendarInPostgres() {
			return nil
		}
		return stored.syncAll(ctx)
	}})
}

// syncAll keeps every finished move's link in step with its provider: each
// provider that syncs both ways (Google, Outlook), through its adapter.
func (s *service) syncAll(ctx context.Context) error {
	if err := s.ready(ctx); err != nil {
		return nil // the store isn't in use yet: nothing to keep in step
	}
	var errs []error
	for _, name := range calendarSyncBusiness.Adapters.TwoWay() {
		provider, ok := syncProvider(name)
		if !ok {
			continue
		}
		links, err := s.ledger.syncedLinks(ctx, name, syncLinksPerSweep)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, l := range links {
			if err := s.syncLink(ctx, l, provider); err != nil {
				helpers.LogErrorWithContext(ctx, "business/Calendar: %s sync for link %s: %v", name, l.ID, err)
			}
		}
	}
	return errors.Join(errs...)
}

// syncLink brings Google's changes in, then sends OneCamp's out. When Google
// no longer answers for the account, the link breaks and its pass ends.
// last_synced_at moves when the reads worked, even if what was read couldn't
// all land here (applyPulled holds that calendar for the next pass).
func (s *service) syncLink(ctx context.Context, link *models.SyncLink, provider calendarSyncBusiness.Adapter) error {
	who := &model.UserInfo{UserPostgresInfo: model.User{Id: link.PersonID}, UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: link.PersonID.String()}}
	markLinked(link.PersonID, true) // a sweep's link is in step (R384-N1)
	l := adapterLink(link)
	cals, err := s.ledger.calendarsOf(ctx, link.ID)
	if err != nil {
		return err
	}
	refs, err := s.ledger.refsOf(ctx, link.ID)
	if err != nil {
		return err
	}
	byExternal := map[string]models.SyncRef{}
	for _, r := range refs {
		byExternal[r.RemoteID+"\x00"+r.ExternalID] = r
	}
	// Google's first, so a change made on both sides is seen before
	// OneCamp's is sent. A calendar without a cursor is read whole (after a
	// move, only once migration 577 asked for it): until that read has
	// worked and its privacy has landed here, nothing is sent to it (held),
	// so what OneCamp has of an event it hasn't read again never overwrites
	// the provider's, and it keeps no cursor, so the next sweep reads it
	// whole again.
	held := map[string]bool{}
	readErr := "" // why a calendar couldn't be read on this pass, the first's
	for _, c := range cals {
		remote := remoteOf(l, c)
		whole := unread(c)
		changes, cursor, err := provider.Pull(ctx, remote, c.Cursor)
		if errors.Is(err, calendarSyncBusiness.ErrFullPull) {
			changes, cursor, err = provider.Pull(ctx, remote, "")
		}
		if errors.Is(err, calendarSyncBusiness.ErrRevoked) {
			return s.breakLink(ctx, link, err)
		}
		if err != nil {
			held[c.RemoteID] = whole
			if readErr == "" {
				readErr = syncReadError(err)
			}
			helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't read %q: %v", c.RemoteID, err)
			continue
		}
		if !s.applyPulled(ctx, who, link.ID, c, changes, byExternal, whole) {
			held[c.RemoteID] = true
			continue
		}
		if cursor != "" && cursor != c.Cursor {
			c.Cursor = cursor
			_ = s.ledger.keepCalendar(ctx, c)
		}
	}
	// Then what changed here.
	remotes, calendars := s.remotesOf(link, cals, held, s.personalOf(ctx, link.PersonID))
	// A sink sending to the link now (runSyncSink) leaves its changes to
	// the next pass: two pushes at once would both create a new event.
	if pushing.Acquire(link.ID.String(), 1) {
		changed, err := s.ledger.changedSince(ctx, link.ID, link.PersonID, calendars, s.now(), syncPushPerLink)
		if err == nil {
			err = s.pushChanged(ctx, provider, link, remotes, refs, changed, false)
		}
		pushing.Release(link.ID.String())
		switch {
		case errors.Is(err, calendarSyncBusiness.ErrRevoked):
			return s.breakLink(ctx, link, err)
		case err != nil:
			return err
		}
	}
	return s.ledger.markSynced(ctx, link.ID, readErr)
}

// sendWaits is a change a passing failure kept from the provider (the
// network, a 5xx, a 429): it is sent again, by the sweep after its wait or by
// the outbox after its backoff. It reads as its cause, and is one.
type sendWaits struct{ cause error }

func (w sendWaits) Error() string { return w.cause.Error() }
func (w sendWaits) Unwrap() error { return w.cause }

// unread is whether c hasn't been read whole here yet (no cursor: a new
// link, or a move that asked for a whole read). Until that read has worked,
// nothing is sent to it: the sweep holds it on a pass whose read failed, and
// the sink (which doesn't read) always. Pure.
func unread(c models.SyncCalendar) bool { return c.Cursor == "" }

// adapterLink is a link as its adapter takes it. Pure.
func adapterLink(link *models.SyncLink) calendarSyncBusiness.Link {
	return calendarSyncBusiness.Link{ID: link.ID, PersonUUID: link.PersonID.String(), Provider: link.Provider, Account: link.Account}
}

// remotesOf is where each of the person's calendars is sent at the link's
// provider: its linked calendars, but those held on this pass (held, by
// remote id), and the personal calendar to the primary one when it isn't
// linked itself; and those calendars, the ones whose changes are sent.
// personal reads the person's own calendar (personalOf), once however many
// links ask.
func (s *service) remotesOf(link *models.SyncLink, cals []models.SyncCalendar, held map[string]bool, personal func() (uuid.UUID, error)) (map[uuid.UUID]calendarSyncBusiness.Remote, []uuid.UUID) {
	l := adapterLink(link)
	remotes := map[uuid.UUID]calendarSyncBusiness.Remote{}
	var calendars []uuid.UUID
	for _, c := range cals {
		if c.CalendarID != nil && !held[c.RemoteID] {
			remotes[*c.CalendarID] = remoteOf(l, c)
			calendars = append(calendars, *c.CalendarID)
		}
	}
	// What the person makes in their own calendar goes to their own at Google.
	if personal, err := personal(); err == nil {
		if _, ok := remotes[personal]; !ok {
			for _, c := range cals {
				if c.Primary && !held[c.RemoteID] {
					remotes[personal] = remoteOf(l, c)
					calendars = append(calendars, personal)
				}
			}
		}
	}
	return remotes, calendars
}

// personalOf reads person's own calendar, in their zone, at its first call
// only: a delivery's links share it (runSyncSink); the sweep's links are each
// a different person's.
func (s *service) personalOf(ctx context.Context, person uuid.UUID) func() (uuid.UUID, error) {
	return sync.OnceValues(func() (uuid.UUID, error) {
		return s.store.personalCalendar(ctx, person, s.zoneOf(ctx, person))
	})
}

// pushChanged sends each changed event through pushOne, oldest change first,
// to its calendar's remote at the link's provider. A revoked account ends it
// (calendarSyncBusiness.ErrRevoked, the caller's to break the link on). A
// change a passing failure kept (sendWaits) doesn't hold back the ones after
// it: on both paths it is kept in the one refusal ledger to wait its turn
// (syncRetries), as a refusal is, so a later run sends it though a ref was
// kept before it failed (an instance after its series); with answer (the
// outbox sink) the first is answered too, for the outbox's backoff.
func (s *service) pushChanged(ctx context.Context, provider calendarSyncBusiness.Adapter, link *models.SyncLink, remotes map[uuid.UUID]calendarSyncBusiness.Remote, refs map[uuid.UUID]models.SyncRef, changed []*models.Event, answer bool) error {
	if len(changed) == 0 {
		return nil
	}
	evs, err := s.icsOf(ctx, link.PersonID, changed)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, len(changed))
	for i, e := range changed {
		ids[i] = e.ID
	}
	refused, err := s.ledger.refusalsOf(ctx, link.ID, ids)
	if err != nil {
		return err
	}
	var waits error
	for i, e := range changed {
		ref, has := refs[e.ID]
		tries := refused[e.ID].Tries
		switch err := s.pushOne(ctx, provider, remotes[e.CalendarID], link.ID, e, evs[i], ref, has, tries); {
		case errors.Is(err, calendarSyncBusiness.ErrRevoked):
			return err
		case err != nil:
			s.refuse(ctx, link.ID, e.ID, tries, err)
			if answer && waits == nil {
				waits = err
			}
		}
	}
	return waits
}

// Why a provider couldn't be read for now, as a synced link keeps it (its
// error) and the person's status says it (SyncStatus.ReadError): a class,
// never the provider's own words, address or token.
const (
	readUnreachable   = "unreachable"
	readRateLimited   = "rate_limited"
	readProviderError = "provider_error"
)

// syncReadError is a provider's answer to a read as its class, "" for none.
// Google's and Microsoft's are sorted alike: a network that didn't get there
// is unreachable; a calendar the provider refuses for good (deleted or
// unshared there: a 404 or 403, as each adapter's syncErr tells it,
// ErrRefused or ErrReadOnly) is none of the link's, skipped as it was before
// a read said why, so it never holds the link's other calendars back from
// being in step; a rate or quota spent is rate_limited, as
// connectorBusiness.ClassifyAPIError tells it for every connected account;
// anything else the provider answered (a 5xx) is provider_error. The network
// is asked first, as its error carries the address, which may hold anything.
// Pure.
func syncReadError(err error) string {
	var netErr net.Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &netErr), errors.Is(err, context.DeadlineExceeded):
		return readUnreachable
	case errors.Is(err, calendarSyncBusiness.ErrRefused), errors.Is(err, calendarSyncBusiness.ErrReadOnly):
		return ""
	case connectorBusiness.ClassifyAPIError(err) == connectorBusiness.ProblemRateLimited:
		return readRateLimited
	}
	return readProviderError
}

// remoteOf is one of a link's calendars as its adapter takes it. Pure.
func remoteOf(l calendarSyncBusiness.Link, c models.SyncCalendar) calendarSyncBusiness.Remote {
	return calendarSyncBusiness.Remote{Link: l, ID: c.RemoteID, Name: c.Name, Primary: c.Primary, ReadOnly: c.ReadOnly}
}

// breakLink records that Google stopped answering for a link (why is what
// it answered): the link isn't synced until the person connects Google
// again (models.ReconnectSyncLinks).
func (s *service) breakLink(ctx context.Context, link *models.SyncLink, why error) error {
	helpers.LogErrorWithContext(ctx, "business/Calendar: %s sync stops for link %s until it is connected again: %v", link.Provider, link.ID, why)
	return s.ledger.breakLink(ctx, link.ID, syncWithOf(link.Provider).brokenWhy)
}

// pushOne sends one event (a series with its changed instances) to Google,
// and keeps where it is there. An event on a calendar the person may only
// read isn't sent; a change Google refused waits (refuse), and a 403 for
// the calendar's access marks it read-only. tries is how many times this
// change has failed already. It answers an error when Google no longer
// answers for the account (calendarSyncBusiness.ErrRevoked), or a passing
// failure (sendWaits: the network, a 5xx, a 429), for its caller to wait on.
func (s *service) pushOne(ctx context.Context, provider calendarSyncBusiness.Adapter, remote calendarSyncBusiness.Remote, link uuid.UUID, e *models.Event, evs []ICS.Event, ref models.SyncRef, has bool, tries int) error {
	if remote.ID == "" || len(evs) == 0 {
		return nil
	}
	if remote.ReadOnly {
		s.refuse(ctx, link, e.ID, tries, calendarSyncBusiness.ErrReadOnly)
		return nil
	}
	failed := func(what string, err error) error {
		if errors.Is(err, calendarSyncBusiness.ErrRevoked) {
			return err
		}
		helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't %s %s: %v", what, e.ID, err)
		if !errors.Is(err, calendarSyncBusiness.ErrRefused) {
			return sendWaits{err} // passing: its caller says how it waits (pushChanged)
		}
		if errors.Is(err, calendarSyncBusiness.ErrReadOnly) {
			_ = s.ledger.markReadOnly(ctx, link, remote.ID)
		}
		s.refuse(ctx, link, e.ID, tries, err)
		return nil
	}
	if e.DeletedAt != nil {
		if has {
			if _, err := provider.Push(ctx, calendarSyncBusiness.Op{Kind: "delete", Remote: remote, EventUUID: e.ID, ExternalID: ref.ExternalID}); err != nil {
				return failed("delete", err)
			}
			_ = s.ledger.dropRefs(ctx, link, []uuid.UUID{e.ID})
		}
		return nil
	}
	op := calendarSyncBusiness.Op{Kind: "create", Remote: remote, EventUUID: e.ID, Event: evs[0]}
	if has {
		op.Kind, op.ExternalID = "update", ref.ExternalID
	}
	got, err := provider.Push(ctx, op)
	if err != nil {
		return failed("send", err)
	}
	external := got.ExternalID
	if external == "" {
		external = ref.ExternalID
	}
	// An instance Google didn't take sends the event again later, once
	// where the series is there is kept.
	var instanceErr error
	for _, inst := range evs[1:] {
		if _, err := provider.Push(ctx, calendarSyncBusiness.Op{Kind: "update", Remote: remote, EventUUID: e.ID, ExternalID: external, Event: inst}); err != nil {
			instanceErr = err
			if errors.Is(err, calendarSyncBusiness.ErrRevoked) {
				break
			}
		}
	}
	if err := s.ledger.keepRefs(ctx, []models.SyncRef{{LinkID: link, RemoteID: remote.ID, ExternalID: external, EventID: e.ID, ETag: got.ETag}}); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't keep where %s is: %v", e.ID, err)
	}
	if instanceErr != nil {
		return failed("send an instance of", instanceErr)
	}
	return nil
}

// refuse records that Google didn't take an event's change for the
// (tries+1)th time. It waits until the event changes when the calendar can
// only be read, or when Google refused it syncRefusedTries times; anything
// else (a rate limit, an outage) is sent again after its wait.
func (s *service) refuse(ctx context.Context, link, event uuid.UUID, tries int, why error) {
	r := models.SyncRefusal{LinkID: link, EventID: event, Tries: tries + 1, Why: why.Error()}
	forGood := errors.Is(why, calendarSyncBusiness.ErrReadOnly) || (errors.Is(why, calendarSyncBusiness.ErrRefused) && r.Tries >= syncRefusedTries)
	now := s.now()
	if !forGood {
		at := now.Add(helpers.ScheduleWait(syncRetries, r.Tries, syncRetryJitter))
		r.RetryAt = &at
	}
	if _, err := s.ledger.keepRefusal(ctx, r, now); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't keep that %s wasn't taken: %v", event, err)
	}
}

// roomTakenAtGoogle is whether err refused Google's change to e as it moves
// e onto a time its room is booked (*RoomTakenError). OneCamp keeps its own
// version and the refusal (it waits, unsent, until e changes here), and tells
// e's organiser only when the refusal is first kept: a later poll that meets
// the same change tells nobody again. The ledger keeps one refusal per
// (link, event), so one already kept for another reason (a push backing
// off) answers made false and that rare room refusal tells nobody.
func (s *service) roomTakenAtGoogle(ctx context.Context, link uuid.UUID, e *models.Event, err error) bool {
	var taken *RoomTakenError
	if !errors.As(err, &taken) {
		return false
	}
	made, kerr := s.ledger.keepRefusal(ctx, models.SyncRefusal{LinkID: link, EventID: e.ID, Why: taken.Error()}, s.now())
	if kerr != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't keep that %s's room was booked: %v", e.ID, kerr)
		return true
	}
	if made {
		tellRoomTakenAtGoogle(ctx, e, taken)
	}
	return true
}

// applyPulled writes Google's changes to one calendar here: a new event
// through the import path, a changed one onto its row, an instance changed
// or cancelled on its own onto its series, and a deleted one into the trash.
// Read whole, every event is taken again, whatever its version, and one
// private on either side is made private on both: before calendar B7
// neither side told the other, so a version unchanged since says nothing of
// it. It says whether a whole read's privacy landed here (a read from a
// cursor always does): when not, nothing is sent to the calendar and it is
// read whole again (syncLink).
func (s *service) applyPulled(ctx context.Context, who *model.UserInfo, link uuid.UUID, c models.SyncCalendar, changes []calendarSyncBusiness.Change, byExternal map[string]models.SyncRef, whole bool) bool {
	me := personOf(who)
	now := s.now()
	// Where the page's events are at Google is kept, and the deleted ones'
	// dropped, once for the page, after its writes: each ref is stamped after
	// its event's last change. A series kept twice keeps its newest ETag.
	kept := map[string]models.SyncRef{}
	var keep []string
	keepRef := func(ref models.SyncRef) {
		k := ref.RemoteID + "\x00" + ref.ExternalID
		if _, ok := kept[k]; !ok {
			keep = append(keep, k)
		}
		kept[k] = ref
	}
	var drop []uuid.UUID
	// madePrivate are the events a whole read makes private here, written in
	// one batch after the page; inStepOnce, the refs of those Google has
	// private already, in step once that batch lands. send are the refs of
	// series with an instance to send, kept for none of the page's changes.
	var madePrivate []rowUpdate
	var inStepOnce []models.SyncRef
	send := map[string]bool{}
	landed := true
	var fresh []calendarSyncBusiness.Change
	freshUIDs := map[string]bool{}
	// touched is the events written here, for the work graph: Google's
	// writes announce nothing (graphNodes.go). New ones go through the
	// import, which tells it of its own.
	var touched []string
	for _, ch := range changes {
		if ch.SeriesID != "" {
			continue // instances after their series, below
		}
		ref, has := byExternal[c.RemoteID+"\x00"+ch.ExternalID]
		if !has {
			if !ch.Deleted && !cancelled(ch.Event) {
				fresh = append(fresh, ch)
				freshUIDs[ch.Event.UID] = true
			}
			continue
		}
		if !whole && ref.ETag != "" && ref.ETag == ch.ETag {
			continue // what OneCamp sent, back
		}
		e, err := s.store.get(ctx, ref.EventID)
		if err != nil || e == nil {
			continue
		}
		if ch.Deleted || cancelled(ch.Event) {
			if e.DeletedAt == nil {
				if _, err := s.store.setDeleted(ctx, e.ID, &now, &me, now, nil); err != nil {
					helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't trash %s: %v", e.ID, err)
					continue
				}
			}
			drop = append(drop, e.ID)
			touched = append(touched, e.ID.String())
			continue
		}
		ev := ch.Event
		privateThere := visibilityOfClass(ev.Class) == visibilityPrivate
		private := whole && (privateThere || e.Visibility == visibilityPrivate)
		if private {
			ev.Class = classOf(visibilityPrivate)
		}
		// Both sides changed it: the later change wins, and is sent private
		// when either side had it so.
		if e.UpdatedAt.After(ref.UpdatedAt) && !ev.Stamp.IsZero() && ev.Stamp.Before(e.UpdatedAt) {
			if private && e.Visibility != visibilityPrivate {
				madePrivate = append(madePrivate, privateNow(e, now))
				touched = append(touched, e.ID.String())
			}
			continue
		}
		changed, err := s.takeRemote(ctx, e, ev)
		// Google moved it onto a time its room is booked: OneCamp keeps its
		// own version, its ref stays (so the change isn't taken as in step,
		// which would end the refusal), and its organiser is told once.
		roomTaken := s.roomTakenAtGoogle(ctx, link, e, err)
		if changed {
			touched = append(touched, e.ID.String())
		}
		// Private here only: it is sent so, so it isn't in step yet.
		if private && !privateThere {
			if !changed {
				madePrivate = append(madePrivate, privateNow(e, now))
			}
			continue
		}
		// Private at Google, and Google's version not taken (refused, or its
		// write failed): private here all the same.
		if private && !changed && e.Visibility != visibilityPrivate {
			madePrivate = append(madePrivate, privateNow(e, now))
			touched = append(touched, e.ID.String())
			if !roomTaken {
				ref.ETag = ch.ETag
				inStepOnce = append(inStepOnce, ref)
			}
			continue
		}
		if !roomTaken && (changed || ref.ETag != ch.ETag) {
			ref.ETag = ch.ETag
			keepRef(ref)
		}
	}
	if len(madePrivate) > 0 {
		if err := s.store.apply(ctx, batch{updates: madePrivate, now: now}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't make %q's events private: %v", c.RemoteID, err)
			landed = false
		} else {
			for _, ref := range inStepOnce {
				keepRef(ref)
			}
		}
	}
	// New events come in as an import does, with their instances.
	if len(fresh) > 0 {
		cal := ICS.Calendar{Name: c.Name}
		if c.Primary {
			cal.Name = ""
		}
		external := map[string]calendarSyncBusiness.Change{}
		for _, ch := range fresh {
			cal.Events = append(cal.Events, ch.Event)
			external[ch.Event.UID] = ch
		}
		for _, ch := range changes {
			if ch.SeriesID != "" && freshUIDs[ch.Event.UID] {
				cal.Events = append(cal.Events, ch.Event)
			}
		}
		made, err := s.importPulled(ctx, who, []ICS.Calendar{cal}, func(int, int) {})
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't bring in new events: %v", err)
		} else {
			s.keepRefsOf(ctx, link, c.RemoteID, made.Created, external)
		}
	}
	// Instances changed or cancelled on their own, of series already here.
	for _, ch := range changes {
		if ch.SeriesID == "" || ch.Event.RecurrenceID == nil || freshUIDs[ch.Event.UID] {
			continue
		}
		ref, has := byExternal[c.RemoteID+"\x00"+ch.SeriesID]
		if !has {
			continue
		}
		series, err := s.store.get(ctx, ref.EventID)
		if err != nil || series == nil || series.RRule == "" || series.DeletedAt != nil {
			continue
		}
		switch s.takeRemoteInstance(ctx, link, series, ch.Event, now, whole) {
		case instanceInStep:
			// In step with Google now: nothing of this to send back.
			if newer, ok := kept[ref.RemoteID+"\x00"+ref.ExternalID]; ok {
				ref = newer
			}
			keepRef(ref)
		case instanceToSend:
			send[ref.RemoteID+"\x00"+ref.ExternalID] = true
		case instanceFailed:
			landed = false
		}
	}
	keep = slices.DeleteFunc(keep, func(k string) bool { return send[k] })
	if len(keep) > 0 {
		refs := make([]models.SyncRef, len(keep))
		for i, k := range keep {
			refs[i] = kept[k]
		}
		if err := s.ledger.keepRefs(ctx, refs); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't keep where %q's events are: %v", c.RemoteID, err)
		}
	}
	if len(drop) > 0 {
		if err := s.ledger.dropRefs(ctx, link, drop); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't forget %q's deleted events: %v", c.RemoteID, err)
		}
	}
	nodesChanged(ctx, workGraph.KindEvent, touched)
	SearchEntriesChanged(ctx, touched)
	return landed
}

func cancelled(ev ICS.Event) bool { return ev.Status == "CANCELLED" }

// privateNow is an event made private, changed at now so it is sent: what a
// whole read does with one private on one side only (applyPulled). It is
// written only while the row is as read, so an edit made since fails the
// batch (and the calendar is read whole again) rather than being lost. Pure.
func privateNow(e *models.Event, now time.Time) rowUpdate {
	next := cloneEvent(e)
	next.Visibility, next.UpdatedAt = visibilityPrivate, now
	read := e.Sequence
	return rowUpdate{e: next, expect: &read}
}

// takeRemote writes Google's version of an event onto its row, and says
// whether anything changed: an event already as Google has it is left
// alone, so what was just sent never comes back as a change. err is why the
// write was refused (logged), as a room booked then (*RoomTakenError).
func (s *service) takeRemote(ctx context.Context, e *models.Event, ev ICS.Event) (changed bool, err error) {
	row, why := importedRow(ev, e.TZ)
	if why != "" {
		return false, nil
	}
	next := cloneEvent(e)
	next.Title, next.Notes, next.Location, next.Transparency, next.Visibility = row.Title, row.Notes, row.Location, row.Transparency, row.Visibility
	if e.Source == sourceSubscribed {
		next.Transparency = transparencyTransparent // never busy (calendar B8)
	}
	next.StartAt, next.EndAt, next.StartDate, next.EndDate, next.TZ, next.EndTZ = row.StartAt, row.EndAt, row.StartDate, row.EndDate, row.TZ, row.EndTZ
	if next.RRule != "" || ev.RRule != "" {
		if err := setRule(next, ev.RRule); err != nil {
			return false, nil
		}
		next.Exdates = append([]time.Time{}, ev.ExDates...)
		next.Rdates = append([]time.Time{}, ev.RDates...)
	}
	if len(changesOf(e, next)) == 0 && next.RRule == e.RRule && slices.EqualFunc(next.Exdates, e.Exdates, time.Time.Equal) && slices.EqualFunc(next.Rdates, e.Rdates, time.Time.Equal) {
		return false, nil
	}
	next.UpdatedAt = s.now()
	if err := check(next); err != nil {
		return false, nil
	}
	if _, err := s.store.update(ctx, next, nil, guestWrites{}); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't write %s: %v", e.ID, err)
		return false, err
	}
	return true, nil
}

// instanceTake is what takeRemoteInstance did with Google's version of an
// instance.
type instanceTake int

const (
	// instanceAsIs: nothing written (as Google has it already, or not taken).
	instanceAsIs instanceTake = iota
	// instanceInStep: written as Google has it.
	instanceInStep
	// instanceToSend: read whole, private here only, so written to be sent
	// private.
	instanceToSend
	// instanceFailed: read whole, a private instance's write didn't land.
	instanceFailed
)

// takeRemoteInstance writes Google's version of one instance of a series:
// cancelled, it is skipped; changed, its own row says how. One already as
// Google has it is left alone, and read whole, one private here stays so
// and is sent so when Google's isn't: private wins both ways, as for a
// whole event (applyPulled). One with no override here yet comes in as an
// import's does (overrideFromRemote): private when it or its series is.
//
// Moved onto a time its room is booked, it is refused as a whole event is
// (roomTakenAtGoogle on link: OneCamp keeps its version, its organiser is
// told once), keyed on the instance's own row here or, with none yet, on
// its series. A subscription passes no link: it holds no rooms.
func (s *service) takeRemoteInstance(ctx context.Context, link uuid.UUID, series *models.Event, ev ICS.Event, now time.Time, whole bool) instanceTake {
	rid := ev.RecurrenceID.UTC()
	if series.AllDay() {
		rid = time.Date(rid.Year(), rid.Month(), rid.Day(), 0, 0, 0, 0, time.UTC)
	}
	if !isInstance(series, rid) && !slices.ContainsFunc(series.Exdates, rid.Equal) {
		return instanceAsIs
	}
	have, err := s.store.overrideAt(ctx, series.ID, rid)
	if err != nil {
		return instanceAsIs
	}
	if cancelled(ev) {
		if slices.ContainsFunc(series.Exdates, rid.Equal) {
			return instanceAsIs
		}
		next := cloneEvent(series)
		next.Exdates = append(next.Exdates, rid)
		next.UpdatedAt = now
		if _, err := s.store.update(ctx, next, nil, guestWrites{}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't skip an instance of %s: %v", series.ID, err)
			return instanceAsIs
		}
		return instanceInStep
	}
	row, why := importedRow(ev, series.TZ)
	if why != "" || row.AllDay() != series.AllDay() {
		return instanceAsIs
	}
	inst := madeInstance(series, rid)
	if have != nil {
		sendPrivate := false
		if whole && have.Visibility == visibilityPrivate {
			sendPrivate = row.Visibility != visibilityPrivate
			row.Visibility = visibilityPrivate
		}
		next := cloneEvent(have)
		takeChanges(next, row, changesOf(have, row))
		if len(changesOf(have, next)) == 0 && !sendPrivate {
			return instanceAsIs
		}
		next.Changed = changesOf(inst.Event, next)
		next.UpdatedAt = now
		if _, err := s.store.update(ctx, next, nil, guestWrites{}); err != nil {
			if link != uuid.Nil && s.roomTakenAtGoogle(ctx, link, next, err) {
				return instanceAsIs
			}
			helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't change an instance of %s: %v", series.ID, err)
			if whole && row.Visibility == visibilityPrivate {
				return instanceFailed
			}
			return instanceAsIs
		}
		if sendPrivate {
			return instanceToSend
		}
		return instanceInStep
	}
	if !s.overrideFromRemote(row, series, rid, now) {
		return instanceAsIs
	}
	if err := s.store.apply(ctx, batch{inserts: []*models.Event{row}, now: now, by: &series.CreatedBy}); err != nil {
		if link != uuid.Nil && s.roomTakenAtGoogle(ctx, link, series, err) {
			return instanceAsIs
		}
		helpers.LogErrorWithContext(ctx, "business/Calendar: calendar sync couldn't add an instance of %s: %v", series.ID, err)
		return instanceAsIs
	}
	return instanceInStep
}
