package domain

// The admin's sync ledger (gcal-8 H1): the text models/postgres/CalendarV2
// (syncLedgerModel.go) runs.

// ledgerProviders are the providers calendars sync with both ways; a link of
// another kind (caldav, ics) holds calendars read from an address.
const ledgerProviders = `provider IN ('google', 'microsoft')`

// SyncLedger is a page of the workspace's Google and Outlook links, of
// people still here: $1 a state and $2 a provider (empty for any), $3 links
// after the first $4, oldest first. Each comes with how many of its changes
// the provider refused, and how many of its person's calendar.sync outbox
// entries died (an entry names its event; the event's owner is the person).
// One statement: the counts are aggregates over the page's links and people
// only, joined to it, never a read per link.
const SyncLedger = `WITH page AS (
		SELECT ` + SyncLinkColumns + `, created_at AS listed_at FROM calendar_sync_links l
		WHERE ` + ledgerProviders + ` AND ($1::text = '' OR state = $1) AND ($2::text = '' OR provider = $2)
		  AND EXISTS (SELECT 1 FROM users u WHERE u.id = l.person_id AND u.deleted_at IS NULL)
		ORDER BY created_at, id LIMIT $3 OFFSET $4),
	refused AS (SELECT f.link_id, count(*) AS n FROM calendar_sync_refusals f
		WHERE f.link_id IN (SELECT id FROM page) GROUP BY f.link_id),
	dead AS (SELECT e.created_by, count(*) AS n FROM outbox o
		JOIN events e ON e.id = CASE WHEN o.topic = 'calendar.sync' THEN (o.entities->>'event')::uuid END
		WHERE o.topic = 'calendar.sync' AND o.state = 'dead' AND e.created_by IN (SELECT person_id FROM page)
		GROUP BY e.created_by)
	SELECT ` + SyncLinkColumns + `, COALESCE(refused.n, 0), COALESCE(dead.n, 0) FROM page
		LEFT JOIN refused ON refused.link_id = page.id
		LEFT JOIN dead ON dead.created_by = page.person_id
		ORDER BY page.listed_at, page.id`

// BreakSyncLinkByAdmin is an admin's break of a Google or Outlook link ($1),
// whatever its state: it is broken, with why ($2) as its person reads it, and
// answered with whether an admin had broken it already (it was broken with
// the same why: a break sent again); no row for an id no such link has. The
// row is locked before its old state is read, so of two breaks at once the
// second sees the first's.
const BreakSyncLinkByAdmin = `UPDATE calendar_sync_links SET state = 'broken', error = $2, updated_at = NOW()
		FROM (SELECT id AS was_id, state AS was_state, error AS was_error FROM calendar_sync_links WHERE id = $1 FOR UPDATE) was
		WHERE id = was.was_id AND ` + ledgerProviders + `
		RETURNING ` + SyncLinkColumns + `, (was.was_state = 'broken' AND was.was_error IS NOT DISTINCT FROM $2)`
