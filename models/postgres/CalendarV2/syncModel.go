package models

// Calendar sync's ledger (calendar B6, the calendar_sync migration): a
// person's link to a provider and where its move stands, each of its
// calendars with the OneCamp calendar it comes into and the provider's
// cursor, and each event's id and version at the provider.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// The states of a sync link. A broken one is a link the provider stopped
// answering for: it isn't synced until the person connects it again.
const (
	SyncMoving = "moving"
	SyncSynced = "synced"
	SyncFailed = "failed"
	SyncBroken = "broken"
)

// SyncLink is a person's link to a provider, and where its move stands.
type SyncLink struct {
	ID           uuid.UUID
	PersonID     uuid.UUID
	Provider     string
	Account      string
	State        string
	Progress     int
	Moved        int
	Unchanged    int
	Skipped      json.RawMessage
	Error        string
	LastSyncedAt *time.Time
	UpdatedAt    time.Time
}

const syncLinkColumns = `id, person_id, provider, account, state, progress, moved, unchanged, skipped, error, last_synced_at, updated_at`

func scanSyncLink(s pgexec.Row) (*SyncLink, error) {
	var l SyncLink
	if err := s.Scan(&l.ID, &l.PersonID, &l.Provider, &l.Account, &l.State, &l.Progress, &l.Moved, &l.Unchanged, &l.Skipped, &l.Error, &l.LastSyncedAt, &l.UpdatedAt); err != nil {
		return nil, err
	}
	return &l, nil
}

// SyncLinkOf is person's link to provider, or nil.
func SyncLinkOf(ctx context.Context, person uuid.UUID, provider string) (*SyncLink, error) {
	l, _, err := pgexec.QueryOne(ctx, `SELECT `+syncLinkColumns+` FROM calendar_sync_links WHERE person_id = $1 AND provider = $2`, scanSyncLink, person, provider)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SyncLinkOf err: %+v", err)
		return nil, err
	}
	return l, nil
}

// StartMove marks person's link to provider as moving, made with newID the
// first time, and says whether this call started it: a move already under
// way, and not stuck past staleAfter, isn't started twice.
func StartMove(ctx context.Context, newID, person uuid.UUID, provider, account string, staleAfter time.Duration) (*SyncLink, bool, error) {
	l, started, err := pgexec.QueryOne(ctx, `INSERT INTO calendar_sync_links (id, person_id, provider, account, state, progress, updated_at)
		VALUES ($1, $2, $3, $4, 'moving', 0, NOW())
		ON CONFLICT (person_id, provider) DO UPDATE SET account = EXCLUDED.account, state = 'moving', progress = 0, moved = 0, unchanged = 0,
			skipped = '[]', error = '', updated_at = NOW()
		WHERE calendar_sync_links.state <> 'moving' OR calendar_sync_links.updated_at < NOW() - $5 * interval '1 second'
		RETURNING `+syncLinkColumns, scanSyncLink, newID, person, provider, account, int(staleAfter/time.Second))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.StartMove err: %+v", err)
		return nil, false, err
	}
	if !started {
		have, err := SyncLinkOf(ctx, person, provider)
		return have, false, err
	}
	return l, true, nil
}

// SetSyncProgress records how far a move has got (0–100).
func SetSyncProgress(ctx context.Context, id uuid.UUID, progress int) error {
	_, err := pgexec.Exec(ctx, `UPDATE calendar_sync_links SET progress = $2, updated_at = NOW() WHERE id = $1`, id, progress)
	return err
}

// FinishMove records a move's end: synced, with what it moved, found there
// already and left out; or failed, with why.
func FinishMove(ctx context.Context, id uuid.UUID, state string, moved, unchanged int, skipped json.RawMessage, why string) error {
	if len(skipped) == 0 {
		skipped = json.RawMessage("[]")
	}
	_, err := pgexec.Exec(ctx, `UPDATE calendar_sync_links SET state = $2::varchar, progress = CASE WHEN $2::varchar = 'synced' THEN 100 ELSE progress END,
		moved = $3, unchanged = $4, skipped = $5::jsonb, error = $6, last_synced_at = CASE WHEN $2::varchar = 'synced' THEN NOW() ELSE last_synced_at END, updated_at = NOW()
		WHERE id = $1`, id, state, moved, unchanged, string(skipped), why)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.FinishMove err: %+v", err)
	}
	return err
}

// SyncCalendar is one of a link's calendars at the provider.
type SyncCalendar struct {
	LinkID     uuid.UUID
	RemoteID   string
	Name       string
	Primary    bool
	CalendarID *uuid.UUID
	Cursor     string
	// ReadOnly is a calendar the link may only read: nothing is sent to it.
	ReadOnly bool
}

// KeepSyncCalendar records a link's calendar, the OneCamp calendar it came
// into, the provider's cursor for it and whether it can only be read.
func KeepSyncCalendar(ctx context.Context, c SyncCalendar) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO calendar_sync_calendars (link_id, remote_id, name, is_primary, calendar_id, cursor, read_only, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (link_id, remote_id) DO UPDATE SET name = EXCLUDED.name, is_primary = EXCLUDED.is_primary,
			calendar_id = EXCLUDED.calendar_id, cursor = EXCLUDED.cursor, read_only = EXCLUDED.read_only, updated_at = NOW()`,
		c.LinkID, c.RemoteID, c.Name, c.Primary, nullUUID(c.CalendarID), c.Cursor, c.ReadOnly)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.KeepSyncCalendar err: %+v", err)
	}
	return err
}

// MarkSyncCalendarReadOnly records that a link's calendar can only be read,
// found out when the provider refused a change to it.
func MarkSyncCalendarReadOnly(ctx context.Context, link uuid.UUID, remote string) error {
	_, err := pgexec.Exec(ctx, `UPDATE calendar_sync_calendars SET read_only = true, updated_at = NOW() WHERE link_id = $1 AND remote_id = $2`, link, remote)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.MarkSyncCalendarReadOnly err: %+v", err)
	}
	return err
}

// SyncRef is where one OneCamp event is at the provider.
type SyncRef struct {
	LinkID     uuid.UUID
	RemoteID   string
	ExternalID string
	EventID    uuid.UUID
	ETag       string
	UpdatedAt  time.Time
}

// syncRefsPerStatement bounds one statement of KeepSyncRefs, so a move's
// thousands of refs stay well inside the statement timeout.
const syncRefsPerStatement = 500

// KeepSyncRefs records where events are at the provider, one statement for
// each syncRefsPerStatement of them. A ref given twice keeps its last, in
// the place of its first: one statement can't change a row twice.
func KeepSyncRefs(ctx context.Context, refs []SyncRef) error {
	type key struct {
		link             uuid.UUID
		remote, external string
	}
	at := map[key]int{}
	keep := make([]SyncRef, 0, len(refs))
	for _, r := range refs {
		k := key{r.LinkID, r.RemoteID, r.ExternalID}
		if i, ok := at[k]; ok {
			keep[i] = r
			continue
		}
		at[k] = len(keep)
		keep = append(keep, r)
	}
	for len(keep) > 0 {
		page := keep[:min(len(keep), syncRefsPerStatement)]
		keep = keep[len(page):]
		links, events := make([]uuid.UUID, len(page)), make([]uuid.UUID, len(page))
		remotes, externals, etags := make([]string, len(page)), make([]string, len(page)), make([]string, len(page))
		for i, r := range page {
			links[i], remotes[i], externals[i], events[i], etags[i] = r.LinkID, r.RemoteID, r.ExternalID, r.EventID, r.ETag
		}
		// Stamped no earlier than the event's own last change (or any of its
		// instances'), so an event just written to match the provider is
		// never taken for OneCamp's own change and sent back: an echo.
		if _, err := pgexec.Exec(ctx, `INSERT INTO calendar_sync_refs (link_id, remote_id, external_id, event_id, etag, updated_at)
			SELECT r.link_id, r.remote_id, r.external_id, r.event_id, r.etag,
				GREATEST(NOW(), (SELECT max(e.updated_at) FROM events e WHERE e.id = r.event_id OR e.series_id = r.event_id))
			FROM unnest($1::uuid[], $2::text[], $3::text[], $4::uuid[], $5::text[]) AS r(link_id, remote_id, external_id, event_id, etag)
			ON CONFLICT (link_id, remote_id, external_id) DO UPDATE SET event_id = EXCLUDED.event_id, etag = EXCLUDED.etag, updated_at = EXCLUDED.updated_at`,
			uuidArray(links), pq.Array(remotes), pq.Array(externals), uuidArray(events), pq.Array(etags)); err != nil {
			helpers.LogErrorWithContext(ctx, "models/CalendarV2.KeepSyncRefs err: %+v", err)
			return err
		}
		// In step now: a change of theirs the provider refused is over.
		if _, err := pgexec.Exec(ctx, `DELETE FROM calendar_sync_refusals f USING unnest($1::uuid[], $2::uuid[]) AS k(link_id, event_id)
			WHERE f.link_id = k.link_id AND f.event_id = k.event_id`, uuidArray(links), uuidArray(events)); err != nil {
			helpers.LogErrorWithContext(ctx, "models/CalendarV2.KeepSyncRefs refusals err: %+v", err)
			return err
		}
	}
	return nil
}

// SyncedLinks are the links whose move has finished, to keep in step, at
// most limit, the longest unsynced first. A deactivated person's links wait:
// nothing of theirs is synced, and they don't hold others' places.
func SyncedLinks(ctx context.Context, provider string, limit int) ([]*SyncLink, error) {
	out, err := pgexec.Query(ctx, `SELECT `+syncLinkColumns+` FROM calendar_sync_links WHERE provider = $1 AND state = 'synced'
		AND EXISTS (SELECT 1 FROM users u WHERE u.id = calendar_sync_links.person_id AND u.deleted_at IS NULL)
		ORDER BY last_synced_at NULLS FIRST LIMIT $2`, scanSyncLink, provider, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SyncedLinks err: %+v", err)
	}
	return out, err
}

// MarkSynced stamps a link as in step now.
func MarkSynced(ctx context.Context, id uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE calendar_sync_links SET last_synced_at = NOW(), updated_at = NOW() WHERE id = $1`, id)
	return err
}

// BreakSyncLink records that the provider stopped answering for a synced
// link, and why, as the person reads it: it isn't synced until they connect
// the account again (ReconnectSyncLinks).
func BreakSyncLink(ctx context.Context, id uuid.UUID, why string) error {
	_, err := pgexec.Exec(ctx, `UPDATE calendar_sync_links SET state = 'broken', error = $2, updated_at = NOW() WHERE id = $1 AND state = 'synced'`, id, why)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.BreakSyncLink err: %+v", err)
	}
	return err
}

// ReconnectSyncLinks syncs a person's broken link to a provider again once
// they have connected its account again. account is the address just
// connected: only the link of that account (in any case) carries on, so
// connecting another account never syncs the link with that account's
// token. A link kept before its address was takes the one connected.
func ReconnectSyncLinks(ctx context.Context, person uuid.UUID, provider, account string) error {
	_, err := pgexec.Exec(ctx, `UPDATE calendar_sync_links SET state = 'synced', error = '', updated_at = NOW(),
			account = CASE WHEN account = '' THEN $3 ELSE account END
		WHERE person_id = $1 AND provider = $2 AND state = 'broken' AND (lower(account) = lower($3) OR account = '')`, person, provider, account)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.ReconnectSyncLinks err: %+v", err)
	}
	return err
}

// SyncCalendarsOf are a link's calendars.
func SyncCalendarsOf(ctx context.Context, link uuid.UUID) ([]SyncCalendar, error) {
	out, err := pgexec.Query(ctx, `SELECT link_id, remote_id, name, is_primary, calendar_id, cursor, read_only FROM calendar_sync_calendars WHERE link_id = $1 ORDER BY is_primary DESC, remote_id`,
		func(r pgexec.Row) (SyncCalendar, error) {
			var c SyncCalendar
			var cal uuid.NullUUID
			if err := r.Scan(&c.LinkID, &c.RemoteID, &c.Name, &c.Primary, &cal, &c.Cursor, &c.ReadOnly); err != nil {
				return c, err
			}
			if cal.Valid {
				id := cal.UUID
				c.CalendarID = &id
			}
			return c, nil
		}, link)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SyncCalendarsOf err: %+v", err)
	}
	return out, err
}

// SyncRefsOf are where a link's events are at the provider, by event.
func SyncRefsOf(ctx context.Context, link uuid.UUID) (map[uuid.UUID]SyncRef, error) {
	refs, err := pgexec.Query(ctx, `SELECT link_id, remote_id, external_id, event_id, etag, updated_at FROM calendar_sync_refs WHERE link_id = $1`,
		func(row pgexec.Row) (SyncRef, error) {
			var r SyncRef
			err := row.Scan(&r.LinkID, &r.RemoteID, &r.ExternalID, &r.EventID, &r.ETag, &r.UpdatedAt)
			return r, err
		}, link)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SyncRefsOf err: %+v", err)
		return nil, err
	}
	out := make(map[uuid.UUID]SyncRef, len(refs))
	for _, r := range refs {
		out[r.EventID] = r
	}
	return out, nil
}

// DropSyncRefs forgets where these events were at the provider, and any
// change of theirs it refused.
func DropSyncRefs(ctx context.Context, link uuid.UUID, events []uuid.UUID) error {
	if len(events) == 0 {
		return nil
	}
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `DELETE FROM calendar_sync_refs WHERE link_id = $1 AND event_id = ANY($2::uuid[])`, link, uuidArray(events)); err != nil {
			return err
		}
		_, err := pgexec.Exec(ctx, `DELETE FROM calendar_sync_refusals WHERE link_id = $1 AND event_id = ANY($2::uuid[])`, link, uuidArray(events))
		return err
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.DropSyncRefs err: %+v", err)
	}
	return err
}

// SyncRefusal is a change of OneCamp's the provider didn't take: how many
// times it was sent, when to send it again (nil: not until it changes) and
// why it wasn't taken.
type SyncRefusal struct {
	LinkID  uuid.UUID
	EventID uuid.UUID
	Tries   int
	RetryAt *time.Time
	Why     string
}

// syncRefusalWhy bounds what a refusal keeps of the provider's answer.
const syncRefusalWhy = 500

// KeepSyncRefusal records a refusal, stamped no earlier than now or the
// event's own last change (or any of its instances'), as KeepSyncRefs
// stamps a ref: the event waits until RetryAt, or until it changes after
// this.
func KeepSyncRefusal(ctx context.Context, r SyncRefusal, now time.Time) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO calendar_sync_refusals (link_id, event_id, tries, retry_at, why, updated_at)
		VALUES ($1, $2, $3, $4, $5, GREATEST($6::timestamptz, (SELECT max(e.updated_at) FROM events e WHERE e.id = $2 OR e.series_id = $2)))
		ON CONFLICT (link_id, event_id) DO UPDATE SET tries = EXCLUDED.tries, retry_at = EXCLUDED.retry_at, why = EXCLUDED.why, updated_at = EXCLUDED.updated_at`,
		r.LinkID, r.EventID, r.Tries, r.RetryAt, helpers.TruncateRunes(r.Why, syncRefusalWhy), now)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.KeepSyncRefusal err: %+v", err)
	}
	return err
}

// SyncRefusalsOf are the refusals of these of a link's events, by event.
func SyncRefusalsOf(ctx context.Context, link uuid.UUID, events []uuid.UUID) (map[uuid.UUID]SyncRefusal, error) {
	out := map[uuid.UUID]SyncRefusal{}
	if len(events) == 0 {
		return out, nil
	}
	rows, err := pgexec.Query(ctx, `SELECT link_id, event_id, tries, retry_at, why FROM calendar_sync_refusals WHERE link_id = $1 AND event_id = ANY($2::uuid[])`,
		func(row pgexec.Row) (SyncRefusal, error) {
			var r SyncRefusal
			err := row.Scan(&r.LinkID, &r.EventID, &r.Tries, &r.RetryAt, &r.Why)
			return r, err
		}, link, uuidArray(events))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.SyncRefusalsOf err: %+v", err)
		return nil, err
	}
	for _, r := range rows {
		out[r.EventID] = r
	}
	return out, nil
}

// ChangedSince are person's events (series and single ones, in the trash or
// not) in these calendars that changed, or had an instance change, after
// their ref was last written, that have no ref, or whose refused change is
// due to be sent again at now: what is OneCamp's to push. A refused change
// waits until its retry, or until the event changes after the refusal, so
// changes the provider won't take never hold back the ones after them. At
// most limit, oldest change first.
func ChangedSince(ctx context.Context, db DB, link, person uuid.UUID, calendars []uuid.UUID, now time.Time, limit int) ([]*Event, error) {
	if len(calendars) == 0 {
		return nil, nil
	}
	return queryEvents(ctx, db, `SELECT `+eventColumns+` FROM events e
		LEFT JOIN calendar_sync_refs r ON r.link_id = $1 AND r.event_id = e.id
		LEFT JOIN calendar_sync_refusals f ON f.link_id = $1 AND f.event_id = e.id
		WHERE e.series_id IS NULL AND e.created_by = $2 AND e.calendar_id = ANY($3::uuid[])
		  AND ((r.event_id IS NULL AND e.deleted_at IS NULL)
		    OR e.updated_at > r.updated_at
		    OR EXISTS (SELECT 1 FROM events o WHERE o.series_id = e.id AND o.updated_at > r.updated_at)
		    OR f.retry_at <= $5)
		  AND NOT (f.event_id IS NOT NULL AND (f.retry_at IS NULL OR f.retry_at > $5)
		    AND f.updated_at >= e.updated_at
		    AND NOT EXISTS (SELECT 1 FROM events o WHERE o.series_id = e.id AND o.updated_at > f.updated_at))
		ORDER BY e.updated_at, e.id
		LIMIT $4`, link, person, uuidArray(calendars), limit, now)
}

// Subscription is a calendar read from an address (calendar B8, the
// calendar_subscriptions migration): one of a person's "ics" link's
// calendars, its address sealed, when it was last read (or tried) and why it
// couldn't be.
type Subscription struct {
	SyncCalendar
	PersonID     uuid.UUID
	SourceSealed string
	ReadAt       *time.Time
	Error        string
}

const subscriptionColumns = `c.link_id, c.remote_id, c.name, c.is_primary, c.calendar_id, c.cursor, l.person_id, c.source_sealed, c.read_at, c.error`

func scanSubscription(s pgexec.Row) (Subscription, error) {
	var out Subscription
	var cal uuid.NullUUID
	if err := s.Scan(&out.LinkID, &out.RemoteID, &out.Name, &out.Primary, &cal, &out.Cursor, &out.PersonID, &out.SourceSealed, &out.ReadAt, &out.Error); err != nil {
		return out, err
	}
	if cal.Valid {
		id := cal.UUID
		out.CalendarID = &id
	}
	return out, nil
}

func querySubscriptions(ctx context.Context, q string, args ...any) ([]Subscription, error) {
	out, err := pgexec.Query(ctx, q, scanSubscription, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.querySubscriptions err: %+v", err)
	}
	return out, err
}

// EnsureSyncLink is person's link to provider, made in step (state
// 'synced') with newID the first time: a link nothing is moved through,
// as subscriptions' is.
func EnsureSyncLink(ctx context.Context, newID, person uuid.UUID, provider string) (*SyncLink, error) {
	if _, err := pgexec.Exec(ctx, `INSERT INTO calendar_sync_links (id, person_id, provider, state, progress, last_synced_at, updated_at)
		VALUES ($1, $2, $3, 'synced', 100, NOW(), NOW()) ON CONFLICT (person_id, provider) DO NOTHING`, newID, person, provider); err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.EnsureSyncLink err: %+v", err)
		return nil, err
	}
	return SyncLinkOf(ctx, person, provider)
}

// AddSubscription records a calendar read from an address, and says whether
// it is new: the same address already read into a calendar of the link's
// isn't added twice (a row whose calendar was deleted is taken again).
func AddSubscription(ctx context.Context, s Subscription) (bool, error) {
	n, err := pgexec.Exec(ctx, `INSERT INTO calendar_sync_calendars (link_id, remote_id, name, is_primary, calendar_id, cursor, source_sealed, read_at, error, updated_at)
		VALUES ($1, $2, $3, false, $4, $5, $6, $7, '', NOW())
		ON CONFLICT (link_id, remote_id) DO UPDATE SET name = EXCLUDED.name, calendar_id = EXCLUDED.calendar_id, cursor = EXCLUDED.cursor,
			source_sealed = EXCLUDED.source_sealed, read_at = EXCLUDED.read_at, error = '', updated_at = NOW()
		WHERE calendar_sync_calendars.calendar_id IS NULL`,
		s.LinkID, s.RemoteID, s.Name, nullUUID(s.CalendarID), s.Cursor, s.SourceSealed, s.ReadAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.AddSubscription err: %+v", err)
		return false, err
	}
	return n == 1, nil
}

// SubscriptionsOf are a person's calendars read from addresses, by name.
func SubscriptionsOf(ctx context.Context, person uuid.UUID) ([]Subscription, error) {
	return querySubscriptions(ctx, `SELECT `+subscriptionColumns+` FROM calendar_sync_calendars c
		JOIN calendar_sync_links l ON l.id = c.link_id
		WHERE l.person_id = $1 AND l.provider = 'ics' AND c.source_sealed <> '' AND c.calendar_id IS NOT NULL
		ORDER BY c.name, c.remote_id`, person)
}

// DueSubscriptions are subscriptions last read (or tried) before before, or
// never: at most limit, the longest unread first.
func DueSubscriptions(ctx context.Context, before time.Time, limit int) ([]Subscription, error) {
	return querySubscriptions(ctx, `SELECT `+subscriptionColumns+` FROM calendar_sync_calendars c
		JOIN calendar_sync_links l ON l.id = c.link_id
		WHERE c.source_sealed <> '' AND c.calendar_id IS NOT NULL AND (c.read_at IS NULL OR c.read_at < $1) AND l.provider = 'ics'
		ORDER BY c.read_at NULLS FIRST LIMIT $2`, before, limit)
}

// ClaimSubscription takes one read of a subscription for the caller, at
// now, when nobody has read it since before: two instances, or a refresh
// and the sweep, never read it at once.
func ClaimSubscription(ctx context.Context, link uuid.UUID, remote string, now, before time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE calendar_sync_calendars SET read_at = $3, updated_at = NOW()
		WHERE link_id = $1 AND remote_id = $2 AND (read_at IS NULL OR read_at < $4)`, link, remote, now, before)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.ClaimSubscription err: %+v", err)
		return false, err
	}
	return n == 1, nil
}

// FinishSubscriptionRead records how a read went: the file's cursor when it
// was read (nil keeps the last), and why it couldn't be ("" when it was).
func FinishSubscriptionRead(ctx context.Context, link uuid.UUID, remote string, cursor *string, why string) error {
	_, err := pgexec.Exec(ctx, `UPDATE calendar_sync_calendars SET cursor = COALESCE($3, cursor), error = $4, updated_at = NOW()
		WHERE link_id = $1 AND remote_id = $2`, link, remote, cursor, why)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.FinishSubscriptionRead err: %+v", err)
	}
	return err
}

// DropSubscription forgets a subscription and where its events were, both
// in one transaction.
func DropSubscription(ctx context.Context, link uuid.UUID, remote string) error {
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `DELETE FROM calendar_sync_refs WHERE link_id = $1 AND remote_id = $2`, link, remote); err != nil {
			return err
		}
		_, err := pgexec.Exec(ctx, `DELETE FROM calendar_sync_calendars WHERE link_id = $1 AND remote_id = $2`, link, remote)
		return err
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.DropSubscription err: %+v", err)
	}
	return err
}
