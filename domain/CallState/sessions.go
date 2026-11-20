package domain

// A call's record as the call statements read it, and the statements that
// read the calls a table links to and a table's picker offers. The text lives
// here; models/postgres/CallState runs it through pgexec (linkModel.go) and
// reads each row in SessionColumns' order (scanSession).

// SessionColumns is a call's record, in the order scanSession reads it.
const SessionColumns = `id, room, COALESCE(room_sid, ''), kind, resource_kind, resource_id, post_uuid,
	COALESCE(topic, ''), started_by, started_at, ended_at, COALESCE(ended_reason, ''), locked_at,
	COALESCE(locked_by, ''), COALESCE(spotlight, ''), mute_on_entry, held, qa_open, qa_shown, qa_anonymous,
	board_uuid, COALESCE(board_by, ''), board_draw, board_guests_draw, COALESCE(board_slide, ''),
	parent_session_id, breakouts_open_at, breakouts_choose, breakouts_ends_at, breakouts_closing_at`

const (
	// SessionsByIDsQuery is the calls of ids ($1, uuid[]).
	SessionsByIDsQuery = `SELECT ` + SessionColumns + ` FROM call_sessions WHERE id = ANY($1::uuid[])`
	// SessionsBeforeQuery is a page of the calls of the conversations
	// reader ($1) is in, before the cursor (started_at $2, id $3), size $4.
	// The calls of the conversations reader is in (the conversations index,
	// conversation_members_user_live_idx), newest first, read through
	// call_sessions_resource_idx (resource_kind, resource_id, started_at DESC):
	// for each conversation, edge is the start of its size-th newest call
	// before the cursor, so its rows are read from there alone (size, and any
	// that started at that same instant) and ordered by start and id; then the
	// newest size of them all. A breakout is its call's, so the picker offers
	// the call alone. A conversation's membership is the candidate set only:
	// whether the reader may read a call is decided by the call rule after.
	SessionsBeforeQuery = `SELECT s.* FROM conversation_members m
		JOIN conversations c ON c.id = m.conversation_id
		CROSS JOIN LATERAL (
			SELECT COALESCE((SELECT e.started_at FROM call_sessions e
				WHERE e.resource_kind = c.kind AND e.resource_id = c.ref AND e.parent_session_id IS NULL
					AND e.started_at <= $2 AND (e.started_at, e.id) < ($2, $3)
				ORDER BY e.started_at DESC OFFSET $4::int - 1 LIMIT 1), '-infinity'::timestamptz) AS at
		) edge
		CROSS JOIN LATERAL (
			SELECT ` + SessionColumns + ` FROM call_sessions x
			WHERE x.resource_kind = c.kind AND x.resource_id = c.ref AND x.parent_session_id IS NULL
				AND x.started_at <= $2 AND x.started_at >= edge.at AND (x.started_at, x.id) < ($2, $3)
			ORDER BY x.started_at DESC, x.id DESC LIMIT $4
		) s
		WHERE m.user_id = $1 AND m.left_at IS NULL
		ORDER BY s.started_at DESC, s.id DESC LIMIT $4`
)
