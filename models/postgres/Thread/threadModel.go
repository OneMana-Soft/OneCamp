package models

// Postgres access for anchored threads (migrations/*_threads.up.sql): the
// threads table and who takes part in each (thread_participants).
// business/Thread owns the rules; this file only reads and writes rows. Ids
// are made by the caller.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Row is one threads row.
type Row struct {
	ID            uuid.UUID
	ResourceKind  string
	ResourceUUID  string
	AnchorKind    string
	AnchorStart   []byte
	AnchorEnd     []byte
	AnchorBlockID string
	AnchorLineage string
	AnchorQuote   string
	// AnchorMeta is JSON, or nil.
	AnchorMeta []byte
	Status     string
	ResolvedBy *uuid.UUID
	ResolvedAt *time.Time
	TaskUUID   *uuid.UUID
	CreatedBy  uuid.UUID
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Participant is one thread_participants row.
type Participant struct {
	ThreadID   uuid.UUID
	UserUUID   uuid.UUID
	Role       string
	LastReadAt *time.Time
}

const rowColumns = `id, resource_kind, resource_uuid, anchor_kind, anchor_start, anchor_end,
	anchor_block_id, anchor_lineage, anchor_quote, anchor_meta, status, resolved_by, resolved_at,
	task_uuid, created_by, created_at, updated_at`

// Insert writes a new thread. With ifAbsent, a thread that already has the id
// is left as it is and inserted reports false; without it that is an error.
func Insert(ctx context.Context, r *Row, ifAbsent bool) (inserted bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `INSERT INTO threads (` + rowColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`
	if ifAbsent {
		q += ` ON CONFLICT (id) DO NOTHING`
	}
	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q,
		r.ID, r.ResourceKind, r.ResourceUUID, r.AnchorKind, r.AnchorStart, r.AnchorEnd,
		nullText(r.AnchorBlockID), nullText(r.AnchorLineage), nullText(r.AnchorQuote), nullJSON(r.AnchorMeta),
		r.Status, r.ResolvedBy, r.ResolvedAt, r.TaskUUID, r.CreatedBy, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread Insert failed: %+v", err)
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Get is one thread, or nil when there is none.
func Get(ctx context.Context, id uuid.UUID) (*Row, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	r, err := scanRow(postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT `+rowColumns+` FROM threads WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread Get failed: %+v", err)
		return nil, err
	}
	return r, nil
}

// ListByResource is a resource's threads, oldest first. status "" is every
// status.
func ListByResource(ctx context.Context, kind, resourceUUID, status string) ([]*Row, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + rowColumns + ` FROM threads WHERE resource_kind = $1 AND resource_uuid = $2`
	args := []any{kind, resourceUUID}
	if status != "" {
		q += ` AND status = $3`
		args = append(args, status)
	}
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q+` ORDER BY created_at, id`, args...)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread ListByResource failed: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			helpers.LogErrorWithContext(cctx, "models/Thread ListByResource scan failed: %+v", err)
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetStatus resolves a thread (by someone, at a time) or reopens it (by nil)
// when it isn't in that state already, and returns it as it is now. nil means
// nothing changed: it was in that state, or there is no such thread. Two
// people resolving at once change it once.
func SetStatus(ctx context.Context, id uuid.UUID, status string, by *uuid.UUID, at time.Time) (*Row, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var resolvedAt *time.Time
	if by != nil {
		resolvedAt = &at
	}
	r, err := scanRow(postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`UPDATE threads SET status = $2, resolved_by = $3, resolved_at = $4, updated_at = $5
		  WHERE id = $1 AND status <> $2
		  RETURNING `+rowColumns, id, status, by, resolvedAt, at))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread SetStatus failed: %+v", err)
		return nil, err
	}
	return r, nil
}

// SetAnchor moves a thread to a new anchor and returns it as it is now; nil
// when there is no such thread.
func SetAnchor(ctx context.Context, id uuid.UUID, a *Row, at time.Time) (*Row, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	r, err := scanRow(postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`UPDATE threads SET anchor_kind = $2, anchor_start = $3, anchor_end = $4, anchor_block_id = $5,
		        anchor_lineage = $6, anchor_quote = $7, anchor_meta = $8, updated_at = $9
		  WHERE id = $1
		  RETURNING `+rowColumns,
		id, a.AnchorKind, a.AnchorStart, a.AnchorEnd, nullText(a.AnchorBlockID), nullText(a.AnchorLineage),
		nullText(a.AnchorQuote), nullJSON(a.AnchorMeta), at))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread SetAnchor failed: %+v", err)
		return nil, err
	}
	return r, nil
}

// Touch marks a thread changed at a time (a reply), never moving it back.
func Touch(ctx context.Context, id uuid.UUID, at time.Time) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE threads SET updated_at = GREATEST(updated_at, $2) WHERE id = $1`, id, at)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread Touch failed: %+v", err)
	}
	return err
}

// Delete removes a thread and who took part in it.
func Delete(ctx context.Context, id uuid.UUID) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, `DELETE FROM threads WHERE id = $1`, id)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread Delete failed: %+v", err)
	}
	return err
}

// AddParticipant records someone taking part. They keep the highest role
// they have had (author > replier > mentioned > reader), and their read time
// only ever moves forward; a nil readAt leaves it as it is.
func AddParticipant(ctx context.Context, p Participant) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`INSERT INTO thread_participants (thread_id, user_uuid, role, last_read_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (thread_id, user_uuid) DO UPDATE SET
		   role = CASE
		     WHEN array_position(ARRAY['reader', 'mentioned', 'replier', 'author'], EXCLUDED.role)
		        > array_position(ARRAY['reader', 'mentioned', 'replier', 'author'], thread_participants.role)
		     THEN EXCLUDED.role ELSE thread_participants.role END,
		   last_read_at = GREATEST(thread_participants.last_read_at, EXCLUDED.last_read_at)`,
		p.ThreadID, p.UserUUID, p.Role, p.LastReadAt)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread AddParticipant failed: %+v", err)
	}
	return err
}

// ParticipantsOf is who takes part in one thread.
func ParticipantsOf(ctx context.Context, threadID uuid.UUID) ([]Participant, error) {
	return participants(ctx, `SELECT thread_id, user_uuid, role, last_read_at
		  FROM thread_participants WHERE thread_id = $1 ORDER BY user_uuid`, threadID)
}

// ParticipantsOfResource is who takes part in each of a resource's threads.
func ParticipantsOfResource(ctx context.Context, kind, resourceUUID string) ([]Participant, error) {
	return participants(ctx, `SELECT p.thread_id, p.user_uuid, p.role, p.last_read_at
		  FROM thread_participants p
		  JOIN threads t ON t.id = p.thread_id
		 WHERE t.resource_kind = $1 AND t.resource_uuid = $2
		 ORDER BY p.thread_id, p.user_uuid`, kind, resourceUUID)
}

func participants(ctx context.Context, q string, args ...any) ([]Participant, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q, args...)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread participants failed: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []Participant
	for rows.Next() {
		var p Participant
		var read sql.NullTime
		if err := rows.Scan(&p.ThreadID, &p.UserUUID, &p.Role, &read); err != nil {
			return nil, err
		}
		if read.Valid {
			t := read.Time
			p.LastReadAt = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRow(s scanner) (*Row, error) {
	var (
		r                       Row
		blockID, lineage, quote sql.NullString
		meta                    []byte
		resolvedBy, taskUUID    uuid.NullUUID
		resolvedAt              sql.NullTime
		anchorStart, anchorEnd  []byte
	)
	if err := s.Scan(&r.ID, &r.ResourceKind, &r.ResourceUUID, &r.AnchorKind, &anchorStart, &anchorEnd,
		&blockID, &lineage, &quote, &meta, &r.Status, &resolvedBy, &resolvedAt,
		&taskUUID, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.AnchorStart, r.AnchorEnd = anchorStart, anchorEnd
	r.AnchorBlockID, r.AnchorLineage, r.AnchorQuote = blockID.String, lineage.String, quote.String
	if len(meta) > 0 {
		r.AnchorMeta = meta
	}
	if resolvedBy.Valid {
		id := resolvedBy.UUID
		r.ResolvedBy = &id
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time
		r.ResolvedAt = &t
	}
	if taskUUID.Valid {
		id := taskUUID.UUID
		r.TaskUUID = &id
	}
	return &r, nil
}

// nullText stores "" as NULL.
func nullText(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// nullJSON stores no JSON as NULL. A []byte would be sent as bytea, which a
// jsonb column refuses, so it goes as text.
func nullJSON(b []byte) sql.NullString { return sql.NullString{String: string(b), Valid: len(b) > 0} }
