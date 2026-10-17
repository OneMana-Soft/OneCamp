package models

import (
	"context"
	"encoding/json"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Notice is one inbox item for one person: what happened to which thing, who
// did it, and why they were told. Ids only: the title is read when the inbox is.
type Notice struct {
	ID           uuid.UUID         `json:"id"`
	UserUUID     uuid.UUID         `json:"-"`
	Kind         string            `json:"kind"`
	ResourceKind string            `json:"resource_kind"`
	ResourceUUID string            `json:"resource_uuid"`
	ActorUUID    string            `json:"actor_uuid,omitempty"`
	AgentUUID    string            `json:"agent_uuid,omitempty"`
	Reason       string            `json:"reason"`
	Data         map[string]string `json:"data,omitempty"`
	DedupKey     string            `json:"-"`
	CreatedAt    time.Time         `json:"created_at"`
}

// ReasonListed is a notice kept only so a person is told once: their feed
// lists what it is about already (a comment on what they made, a mention of
// them), so the inbox never shows it and it is never counted unread.
const ReasonListed = "listed"

// maxDedupKey is the column's width; a longer key is shortened to a prefix and
// a hash, which stays unique.
const maxDedupKey = 255

// InsertNotices writes notices, in one statement, leaving out each whose dedup
// key exists, and says whose were written (by person). Only a notice that was
// written is pushed or emailed, so an event delivered twice tells nobody
// twice. A telling wrote one row a statement, a round trip per person told.
func InsertNotices(ctx context.Context, rows []*Notice) (map[uuid.UUID]bool, error) {
	written := map[uuid.UUID]bool{}
	if len(rows) == 0 {
		return written, nil
	}
	cols := make([][]string, 11)
	for _, n := range rows {
		if n.ID == uuid.Nil {
			n.ID = uuid.New()
		}
		if n.CreatedAt.IsZero() {
			n.CreatedAt = time.Now()
		}
		key := n.DedupKey
		if len(key) > maxDedupKey {
			key = key[:120] + "|" + helpers.HashString(key)
		}
		data := n.Data
		if data == nil {
			data = map[string]string{}
		}
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		for i, v := range []string{n.ID.String(), n.UserUUID.String(), n.Kind, n.ResourceKind, n.ResourceUUID,
			uuidOrEmpty(n.ActorUUID), uuidOrEmpty(n.AgentUUID), n.Reason, string(raw), key, n.CreatedAt.UTC().Format(time.RFC3339Nano)} {
			cols[i] = append(cols[i], v)
		}
	}
	args := make([]any, len(cols))
	for i, c := range cols {
		args[i] = pq.Array(c)
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`INSERT INTO resource_notices (id, user_uuid, kind, resource_kind, resource_uuid, actor_uuid, agent_uuid, reason, data, dedup_key, created_at)
		 SELECT r.id::uuid, r.user_uuid::uuid, r.kind, r.resource_kind, r.resource_uuid, NULLIF(r.actor, '')::uuid, NULLIF(r.agent, '')::uuid,
		        r.reason, r.data::jsonb, r.dedup_key, r.created_at::timestamptz
		 FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[])
		      AS r(id, user_uuid, kind, resource_kind, resource_uuid, actor, agent, reason, data, dedup_key, created_at)
		 ON CONFLICT (dedup_key) DO NOTHING
		 RETURNING user_uuid`, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Follow InsertNotices err: %+v", err)
		return nil, err
	}
	defer res.Close()
	for res.Next() {
		var u uuid.UUID
		if err := res.Scan(&u); err != nil {
			return nil, err
		}
		written[u] = true
	}
	return written, res.Err()
}

// uuidOrEmpty is s as a uuid's text, or "" (no one) when it isn't one.
func uuidOrEmpty(s string) string {
	if u, err := uuid.Parse(s); err == nil {
		return u.String()
	}
	return ""
}

// NoticesFor is the person's notices from before `before`, newest first, at
// most limit of them.
func NoticesFor(ctx context.Context, userUUID uuid.UUID, before time.Time, limit int) ([]*Notice, error) {
	if limit <= 0 {
		return nil, nil
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT id, user_uuid, kind, resource_kind, resource_uuid, COALESCE(actor_uuid::text, ''), COALESCE(agent_uuid::text, ''),
		        reason, data, created_at
		   FROM resource_notices WHERE user_uuid = $1 AND created_at < $2 AND reason <> $4
		  ORDER BY created_at DESC LIMIT $3`,
		userUUID, before, limit, ReasonListed)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Follow NoticesFor err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Notice
	for rows.Next() {
		var n Notice
		var raw []byte
		if err := rows.Scan(&n.ID, &n.UserUUID, &n.Kind, &n.ResourceKind, &n.ResourceUUID, &n.ActorUUID, &n.AgentUUID,
			&n.Reason, &raw, &n.CreatedAt); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &n.Data)
		}
		out = append(out, &n)
	}
	return out, rows.Err()
}

// CountNoticesSince is how many notices the person was given after since.
func CountNoticesSince(ctx context.Context, userUUID uuid.UUID, since time.Time) (uint64, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var n uint64
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT COUNT(*) FROM resource_notices WHERE user_uuid = $1 AND created_at > $2 AND reason <> $3`, userUUID, since, ReasonListed).Scan(&n)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Follow CountNoticesSince err: %+v", err)
	}
	return n, err
}

// DeleteNoticesBefore removes up to batch notices written before `before`, and
// says how many went.
func DeleteNoticesBefore(ctx context.Context, before time.Time, batch int) (int64, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`DELETE FROM resource_notices WHERE id IN (
		     SELECT id FROM resource_notices WHERE created_at < $1 ORDER BY created_at LIMIT $2)`,
		before, batch)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Follow DeleteNoticesBefore err: %+v", err)
		return 0, err
	}
	return res.RowsAffected()
}
