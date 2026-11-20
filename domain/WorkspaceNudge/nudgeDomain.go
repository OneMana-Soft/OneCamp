// Package domain (WorkspaceNudge) is the query text of a member's AI nudges:
// business/AI owns the engine's rules, and models/postgres/WorkspaceNudge runs
// the text through models/pgexec.
package domain

// LastHandled is when each member last dismissed or acted on each of their
// signals: $1 and $2 are pairs (member, dedup key), and there is one row per
// pair they handled, with its latest time. Only each member's own nudges are
// read, so another member's dismissal never holds theirs.
// idx_workspace_nudges_user_open (user_id, status, ...) serves it.
const LastHandled = `SELECT n.user_id, n.dedup_key, max(n.updated_at)
	FROM unnest($1::uuid[], $2::text[]) AS k(user_id, dedup_key)
	JOIN workspace_nudges n ON n.user_id = k.user_id AND n.dedup_key = k.dedup_key
	WHERE n.status IN ('dismissed','acted')
	GROUP BY n.user_id, n.dedup_key`

// UpsertMany writes the engine's pass over everyone's nudges in one
// statement: $1..$10 are its columns, one row per signal (member, kind,
// title, body, cta url, cta text, source type, source id, priority, dedup
// key). Each signal that still holds is a new open nudge, or refreshes the
// copy of the one already open, keeping its updated_at and never raising its
// priority. It answers each row's member, dedup key and id, and whether it
// is new.
const UpsertMany = `INSERT INTO workspace_nudges (
		user_id, kind, title, body, cta_url, cta_text,
		source_type, source_id, status, priority, dedup_key
	)
	SELECT u, k, t, b, NULLIF(cu, ''), NULLIF(ct, ''), st, si, 'open', p, d
	FROM unnest($1::uuid[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::int[], $10::text[])
	     AS x(u, k, t, b, cu, ct, st, si, p, d)
	ON CONFLICT (dedup_key) WHERE status = 'open'
	DO UPDATE SET
		title      = EXCLUDED.title,
		body       = EXCLUDED.body,
		cta_url    = EXCLUDED.cta_url,
		cta_text   = EXCLUDED.cta_text,
		priority   = LEAST(workspace_nudges.priority, EXCLUDED.priority)
	RETURNING user_id, dedup_key, id, (xmax = 0)`

// SupersedeStale closes, in one statement, every open nudge of anyone that
// the pass did not keep: $1 and $2 are the kept pairs (member, dedup key),
// and a member with no pair keeps nothing. It answers each one it closed and
// whose it was.
const SupersedeStale = `UPDATE workspace_nudges n SET status = 'superseded', updated_at = NOW()
	WHERE n.status = 'open'
	  AND NOT EXISTS (
		SELECT 1 FROM unnest($1::uuid[], $2::text[]) AS k(user_id, dedup_key)
		WHERE k.user_id = n.user_id AND k.dedup_key = n.dedup_key)
	RETURNING n.id, n.user_id`
