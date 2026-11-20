// Package domain (CalendarV2) is the query text of models/postgres/CalendarV2
// (domain/Calendar imports that package, so its text can't live there):
// business/Calendar owns the rules, and the models run the text through
// models/pgexec.
package domain

// EventColumns is every column an event read returns, in the models'
// scanEvent order.
const EventColumns = `e.id, e.calendar_id, e.kind, e.title, e.notes, e.notes_format, e.location, e.call_room,
	e.start_at, e.end_at, to_char(e.start_date, 'YYYY-MM-DD'), to_char(e.end_date, 'YYYY-MM-DD'), e.tz, e.end_tz,
	e.rrule, e.until_at, e.series_id, e.recurrence_id,
	COALESCE(array_to_json(e.exdates), '[]'), COALESCE(array_to_json(e.rdates), '[]'), e.changed,
	e.transparency, e.visibility, e.hue, e.status, e.guests_can_modify, e.guests_can_invite, e.guests_can_see_guests,
	e.created_by, e.sequence, e.ical_uid, e.source, COALESCE(e.google_event_id, ''),
	e.created_at, e.updated_at, e.deleted_at, e.deleted_by, e.email_on_answers, e.task_uuid, e.recap_to_guests`

// SyncLinkColumns is every column a sync link read returns, in the models'
// scanSyncLink order.
const SyncLinkColumns = `id, person_id, provider, account, state, progress, moved, unchanged, skipped, error, last_synced_at, updated_at`

// linkInStep is a link the sweep keeps in step: synced, its person here. The
// one predicate the sweep's read (SyncedLinks) and the outbox sink's read of
// one person's links (SyncedLinksOf) share.
const linkInStep = `state = 'synced'
		AND EXISTS (SELECT 1 FROM users u WHERE u.id = calendar_sync_links.person_id AND u.deleted_at IS NULL)`

// SyncedLinks is a provider's ($1) links in step, at most $2, the longest
// unsynced first; a link whose last pass couldn't read its provider takes its
// turn from that pass (updated_at).
const SyncedLinks = `SELECT ` + SyncLinkColumns + ` FROM calendar_sync_links WHERE provider = $1 AND ` + linkInStep + `
		ORDER BY CASE WHEN error = '' THEN last_synced_at ELSE updated_at END NULLS FIRST LIMIT $2`

// SyncedLinksOf is one person's ($1) links in step, of every provider (the
// calendar.sync outbox sink's read, by its key: calendar_sync_links_one_idx).
const SyncedLinksOf = `SELECT ` + SyncLinkColumns + ` FROM calendar_sync_links WHERE person_id = $1 AND ` + linkInStep + `
		ORDER BY provider`

// syncRefsFrom is a sync ref read, in the models' scan order.
const syncRefsFrom = `SELECT link_id, remote_id, external_id, event_id, etag, updated_at FROM calendar_sync_refs `

// SyncRefsOf is where a link's ($1) events are at the provider.
const SyncRefsOf = syncRefsFrom + `WHERE link_id = $1`

// SyncRefsAmong is SyncRefsOf for these events ($2) only (the calendar.sync
// outbox sink's read, by the refs' key).
const SyncRefsAmong = syncRefsFrom + `WHERE link_id = $1 AND event_id = ANY($2::uuid[])`

// changedToPush is ChangedSince's and ChangedAmong's rows and their test:
// $1 the link, $2 its person, $3 the calendars, $5 now. A person's events
// (series and single ones, in the trash or not) in these calendars that
// changed, or had an instance change, after their ref was last written,
// that have no ref, or whose refused change is due again at now; a refused
// change waits until its retry, or until the event changes after it.
const changedToPush = `SELECT ` + EventColumns + ` FROM events e
		LEFT JOIN calendar_sync_refs r ON r.link_id = $1 AND r.event_id = e.id
		LEFT JOIN calendar_sync_refusals f ON f.link_id = $1 AND f.event_id = e.id
		WHERE e.series_id IS NULL AND e.created_by = $2 AND e.calendar_id = ANY($3::uuid[])
		  AND ((r.event_id IS NULL AND e.deleted_at IS NULL)
		    OR e.updated_at > r.updated_at
		    OR EXISTS (SELECT 1 FROM events o WHERE o.series_id = e.id AND o.updated_at > r.updated_at)
		    OR f.retry_at <= $5)
		  AND NOT (f.event_id IS NOT NULL AND (f.retry_at IS NULL OR f.retry_at > $5)
		    AND f.updated_at >= e.updated_at
		    AND NOT EXISTS (SELECT 1 FROM events o WHERE o.series_id = e.id AND o.updated_at > f.updated_at))`

// ChangedSince is what of a person's is the link's to push, at most $4,
// oldest change first (the sweep's read).
const ChangedSince = changedToPush + `
		ORDER BY e.updated_at, e.id
		LIMIT $4`

// ChangedAmong is ChangedSince narrowed to these events ($4), read by id (the
// calendar.sync outbox sink's read, one event a delivery).
const ChangedAmong = changedToPush + `
		  AND e.id = ANY($4::uuid[])
		ORDER BY e.updated_at, e.id`

// FinishMove is a move's end for link $1: synced ($2), with what it moved
// ($3), found there already ($4) and left out ($5); or failed, with why ($6).
// Only a link still moving ends so: an admin's break during the move stays.
const FinishMove = `UPDATE calendar_sync_links SET state = $2::varchar, progress = CASE WHEN $2::varchar = 'synced' THEN 100 ELSE progress END,
		moved = $3, unchanged = $4, skipped = $5::jsonb, error = $6, last_synced_at = CASE WHEN $2::varchar = 'synced' THEN NOW() ELSE last_synced_at END, updated_at = NOW()
		WHERE id = $1 AND state = 'moving'`
