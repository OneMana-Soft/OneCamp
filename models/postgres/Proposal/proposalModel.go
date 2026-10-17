// Package models (Proposal) stores edit proposals (migration 205; business/Proposal
// says what they are).
package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Row is one edit_proposals row. The JSON columns stay raw: business/Proposal
// owns their shape.
type Row struct {
	ID            uuid.UUID
	ResourceKind  string
	ResourceUUID  uuid.UUID
	ThreadID      *uuid.UUID
	AnchorKind    string
	AnchorStart   []byte
	AnchorEnd     []byte
	AnchorBlockID string
	AnchorLineage string
	AnchorQuote   string
	AnchorMeta    json.RawMessage
	BaseHash      string
	Base          json.RawMessage
	Proposed      json.RawMessage
	Hunks         json.RawMessage
	Note          string
	AuthorKind    string
	AuthorUUID    uuid.UUID
	AskerUUID     *uuid.UUID
	RunID         string
	Status        string
	Pending       json.RawMessage
	PendingAt     *time.Time
	AppliedMode   string
	DecidedBy     *uuid.UUID
	DecidedAt     *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const columns = `id, resource_kind, resource_uuid, thread_id, anchor_kind, anchor_start, anchor_end,
	COALESCE(anchor_block_id, ''), COALESCE(anchor_lineage, ''), COALESCE(anchor_quote, ''), anchor_meta,
	base_hash, base, proposed, hunks, COALESCE(note, ''), author_kind, author_uuid, asker_uuid,
	COALESCE(run_id, ''), status, pending, pending_at, COALESCE(applied_mode, ''), decided_by, decided_at,
	created_at, updated_at`

// PendingTTL is how long a step in flight holds a proposal. A step the API
// started and never finished (a crash) lets go after it.
const PendingTTL = 2 * time.Minute

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

func scan(row interface{ Scan(...any) error }) (*Row, error) {
	var r Row
	// Into []byte, which database/sql copies out of the driver's buffer.
	var meta, base, proposed, hunks, pending []byte
	if err := row.Scan(&r.ID, &r.ResourceKind, &r.ResourceUUID, &r.ThreadID, &r.AnchorKind, &r.AnchorStart, &r.AnchorEnd,
		&r.AnchorBlockID, &r.AnchorLineage, &r.AnchorQuote, &meta,
		&r.BaseHash, &base, &proposed, &hunks, &r.Note, &r.AuthorKind, &r.AuthorUUID, &r.AskerUUID,
		&r.RunID, &r.Status, &pending, &r.PendingAt, &r.AppliedMode, &r.DecidedBy, &r.DecidedAt,
		&r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.AnchorMeta, r.Base, r.Proposed, r.Hunks, r.Pending = meta, base, proposed, hunks, pending
	return &r, nil
}

// nullJSON is SQL NULL for an empty raw value.
func nullJSON(v json.RawMessage) any {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	return []byte(v)
}

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Store reads and writes proposals.
type Store struct{}

// Insert stores a new proposal.
func (Store) Insert(ctx context.Context, r *Row) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		INSERT INTO edit_proposals (id, resource_kind, resource_uuid, thread_id, anchor_kind, anchor_start, anchor_end,
			anchor_block_id, anchor_lineage, anchor_quote, anchor_meta, base_hash, base, proposed, hunks, note,
			author_kind, author_uuid, asker_uuid, run_id, status, pending, pending_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $24)`,
		r.ID, r.ResourceKind, r.ResourceUUID, r.ThreadID, r.AnchorKind, r.AnchorStart, r.AnchorEnd,
		nullText(r.AnchorBlockID), nullText(r.AnchorLineage), nullText(r.AnchorQuote), nullJSON(r.AnchorMeta),
		r.BaseHash, nullJSON(r.Base), []byte(r.Proposed), []byte(r.Hunks), nullText(r.Note),
		r.AuthorKind, r.AuthorUUID, r.AskerUUID, nullText(r.RunID), r.Status, nullJSON(r.Pending), r.PendingAt, r.CreatedAt)
	return err
}

// Get is one proposal, or nil.
func (Store) Get(ctx context.Context, id uuid.UUID) (*Row, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	r, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT `+columns+` FROM edit_proposals WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// List is a resource's proposals in the given statuses, open ones first, then
// newest first, at most limit.
func (Store) List(ctx context.Context, kind string, resource uuid.UUID, statuses []string, limit int) ([]Row, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT `+columns+` FROM edit_proposals
		 WHERE resource_kind = $1 AND resource_uuid = $2 AND status = ANY($3)
		 ORDER BY (status IN ('open', 'stale')) DESC, created_at DESC LIMIT $4`,
		kind, resource, pq.Array(statuses), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// CountOpen is how many open or stale proposals a resource has.
func (Store) CountOpen(ctx context.Context, kind string, resource uuid.UUID) (int, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM edit_proposals WHERE resource_kind = $1 AND resource_uuid = $2 AND status IN ('open', 'stale', 'preparing')`,
		kind, resource).Scan(&n)
	return n, err
}

// Claim sets the step in flight, unless another one holds the proposal (one set
// less than PendingTTL ago). nil, nil when it is held.
func (Store) Claim(ctx context.Context, id uuid.UUID, pending json.RawMessage) (*Row, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	r, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		UPDATE edit_proposals SET pending = $2, pending_at = NOW(), updated_at = NOW()
		 WHERE id = $1 AND (pending_at IS NULL OR pending_at < NOW() - make_interval(secs => $3))
		RETURNING `+columns, id, []byte(pending), PendingTTL.Seconds()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// Release lets go of a step in flight without changing anything else.
func (Store) Release(ctx context.Context, id uuid.UUID) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE edit_proposals SET pending = NULL, pending_at = NULL, updated_at = NOW() WHERE id = $1`, id)
	return err
}

// Save writes what a step or a decision changed, and lets go of the step.
func (Store) Save(ctx context.Context, r *Row) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE edit_proposals SET anchor_kind = $2, anchor_start = $3, anchor_end = $4, anchor_block_id = $5,
			anchor_lineage = $6, anchor_quote = $7, anchor_meta = $8, base_hash = $9, base = $10, proposed = $11,
			hunks = $12, status = $13, applied_mode = $14, decided_by = $15, decided_at = $16,
			pending = NULL, pending_at = NULL, updated_at = NOW()
		 WHERE id = $1`,
		r.ID, r.AnchorKind, r.AnchorStart, r.AnchorEnd, nullText(r.AnchorBlockID), nullText(r.AnchorLineage),
		nullText(r.AnchorQuote), nullJSON(r.AnchorMeta), r.BaseHash, nullJSON(r.Base), []byte(r.Proposed),
		[]byte(r.Hunks), r.Status, nullText(r.AppliedMode), r.DecidedBy, r.DecidedAt)
	return err
}

// Delete removes a proposal (one whose preparing failed).
func (Store) Delete(ctx context.Context, id uuid.UUID) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `DELETE FROM edit_proposals WHERE id = $1`, id)
	return err
}

// Sweep removes proposals left preparing past olderThan (the API stopped while
// it worked them out) and lets go of steps left in flight. It is the same
// whichever instance runs it, and however often.
func (Store) Sweep(ctx context.Context, olderThan time.Duration) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		DELETE FROM edit_proposals WHERE status = 'preparing' AND created_at < NOW() - make_interval(secs => $1)`,
		olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE edit_proposals SET pending = NULL, pending_at = NULL
		 WHERE pending_at IS NOT NULL AND pending_at < NOW() - make_interval(secs => $1)`, PendingTTL.Seconds())
	return n, err
}
