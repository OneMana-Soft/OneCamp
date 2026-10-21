package models

// Postgres access for AI Proactive Nudges (migration 75).
//
// A nudge is a short, actionable, per-user prompt the AI surfaces without being
// asked ("you committed to X by Friday and it's Thursday"). This package is the
// single gateway to the table. Writes are idempotent by dedup_key (one OPEN
// nudge per logical signal); reads are always scoped to the requesting user.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Kinds — keep aligned with the CHECK constraint in migration 75.
const (
	KindOverdueCommitment = "overdue_commitment"
	KindStaleQuestion     = "stale_question"
	KindBlockedTask       = "blocked_task"
	KindUnreviewedPR      = "unreviewed_pr"
	KindIdleDecision      = "idle_decision"
	// KindAgentRegression is an agent that started failing its own tests after
	// something changed. Its own kind because it is the only nudge about the
	// workspace's tooling rather than about the person's work.
	KindAgentRegression = "agent_regression"
	KindGeneric         = "generic"
)

// Statuses.
const (
	StatusOpen       = "open"
	StatusDismissed  = "dismissed"
	StatusActed      = "acted"
	StatusSuperseded = "superseded"
)

// Nudge is the in-memory form of a workspace_nudges row.
type Nudge struct {
	ID         uuid.UUID `json:"id"`
	UserID     uuid.UUID `json:"-"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	CTAURL     string    `json:"cta_url,omitempty"`
	CTAText    string    `json:"cta_text,omitempty"`
	SourceType string    `json:"source_type,omitempty"`
	SourceID   string    `json:"source_id,omitempty"`
	Status     string    `json:"status"`
	Priority   int       `json:"priority"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// UpsertInput is the write shape for one generated nudge.
type UpsertInput struct {
	UserID     uuid.UUID
	Kind       string
	Title      string
	Body       string
	CTAURL     string
	CTAText    string
	SourceType string
	SourceID   string
	Priority   int
	DedupKey   string
}

// Upsert inserts a new OPEN nudge or, when an OPEN row with the same dedup_key
// already exists, refreshes its mutable fields in place. This makes re-running
// the engine idempotent: the same signal updates the existing nudge rather than
// stacking duplicates. Terminal rows (dismissed/acted/superseded) for the same
// key are left untouched — but if the user already dismissed this exact signal
// we do NOT resurrect it (honored via the caller's recent-dismissal check).
// Returns the row id and whether it was newly created.
func Upsert(ctx context.Context, in UpsertInput) (uuid.UUID, bool, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if in.DedupKey == "" {
		return uuid.Nil, false, fmt.Errorf("dedup_key required")
	}

	const q = `
		INSERT INTO workspace_nudges (
			user_id, kind, title, body, cta_url, cta_text,
			source_type, source_id, status, priority, dedup_key
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'open',$9,$10)
		ON CONFLICT (dedup_key) WHERE status = 'open'
		DO UPDATE SET
			title      = EXCLUDED.title,
			body       = EXCLUDED.body,
			cta_url    = EXCLUDED.cta_url,
			cta_text   = EXCLUDED.cta_text,
			priority   = EXCLUDED.priority,
			updated_at = NOW()
		RETURNING id, (xmax = 0) AS inserted`

	var id uuid.UUID
	var inserted bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q,
		in.UserID, in.Kind, in.Title, in.Body, nullStr(in.CTAURL), nullStr(in.CTAText),
		in.SourceType, in.SourceID, in.Priority, in.DedupKey,
	).Scan(&id, &inserted)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/workspaceNudge Upsert failed: %+v", err)
		return uuid.Nil, false, err
	}
	return id, inserted, nil
}

// HasRecentTerminal reports whether the user already dismissed or acted on a
// nudge with this dedup_key within `within` — so the engine doesn't re-surface
// something the user just handled. Bounds nudge spam.
func HasRecentTerminal(ctx context.Context, dedupKey string, within time.Duration) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	cutoff := time.Now().Add(-within)
	const q = `
		SELECT EXISTS (
			SELECT 1 FROM workspace_nudges
			WHERE dedup_key = $1
			  AND status IN ('dismissed','acted')
			  AND updated_at >= $2
		)`
	var exists bool
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, q, dedupKey, cutoff).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// ListOpenForUser returns a user's open nudges, highest-priority then newest.
func ListOpenForUser(ctx context.Context, userID uuid.UUID, limit int) ([]*Nudge, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := `
		SELECT id, user_id, kind, title, body, cta_url, cta_text,
		       source_type, source_id, status, priority, created_at, updated_at
		FROM workspace_nudges
		WHERE user_id = $1 AND status = 'open'
		ORDER BY priority DESC, created_at DESC
		LIMIT ` + fmt.Sprintf("%d", limit)

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, q, userID)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/workspaceNudge ListOpenForUser failed: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*Nudge
	for rows.Next() {
		n, err := scanNudge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountOpenForUser returns the number of open nudges for the badge.
func CountOpenForUser(ctx context.Context, userID uuid.UUID) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT COUNT(*) FROM workspace_nudges WHERE user_id = $1 AND status = 'open'`, userID).Scan(&n)
	return n, err
}

// SetStatus transitions a nudge owned by userID to a terminal status. The
// user_id predicate is the authorization check — a user can only mutate their
// own nudges. Returns the affected row (for re-projection) or an error.
func SetStatus(ctx context.Context, id, userID uuid.UUID, status string) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if status != StatusDismissed && status != StatusActed {
		return fmt.Errorf("invalid status")
	}
	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE workspace_nudges SET status = $1, updated_at = NOW()
		 WHERE id = $2 AND user_id = $3 AND status = 'open'`, status, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("nudge not found")
	}
	return nil
}

// DismissAllForUser clears a user's open nudges ("mark all read").
func DismissAllForUser(ctx context.Context, userID uuid.UUID) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE workspace_nudges SET status = 'dismissed', updated_at = NOW()
		 WHERE user_id = $1 AND status = 'open'`, userID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SupersedeOpenByKeys marks open nudges whose dedup_key is NOT in keep as
// superseded for a user — used at the end of an engine pass so signals that no
// longer hold (e.g. a commitment that got resolved) stop showing. keep may be
// empty (supersede all of the user's open nudges). Returns affected count.
func SupersedeStaleForUser(ctx context.Context, userID uuid.UUID, keep []string) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Build a NOT IN (...) guard. With no keys, supersede all open rows.
	args := []any{userID}
	q := `UPDATE workspace_nudges SET status = 'superseded', updated_at = NOW()
	      WHERE user_id = $1 AND status = 'open'`
	if len(keep) > 0 {
		ph := make([]string, len(keep))
		for i, k := range keep {
			ph[i] = fmt.Sprintf("$%d", i+2)
			args = append(args, k)
		}
		q += " AND dedup_key NOT IN (" + strings.Join(ph, ",") + ")"
	}
	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SupersedeOpenByDedupKey marks the OPEN nudge for a single logical signal
// (identified by dedup_key) as superseded, returning the owning user id so the
// caller can push a live badge update. Used to close the loop the instant the
// underlying item is resolved/dismissed/deleted, instead of waiting for the
// next engine sweep. Returns (uuid.Nil, nil) when there was no open nudge.
func SupersedeOpenByDedupKey(ctx context.Context, dedupKey string) (uuid.UUID, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var userID uuid.UUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`UPDATE workspace_nudges SET status = 'superseded', updated_at = NOW()
		 WHERE dedup_key = $1 AND status = 'open'
		 RETURNING user_id`, dedupKey).Scan(&userID)
	if err == sql.ErrNoRows {
		return uuid.Nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/workspaceNudge SupersedeOpenByDedupKey failed: %+v", err)
		return uuid.Nil, err
	}
	return userID, nil
}

// UsersWithOpenNudges returns the distinct user ids that currently have at
// least one OPEN nudge. The engine uses this to reconcile users whose signals
// ALL resolved between passes (and who therefore produce no candidates this
// run): they won't appear in the per-owner candidate map, so their now-stale
// open nudges must be superseded explicitly or they'd linger forever.
func UsersWithOpenNudges(ctx context.Context) ([]uuid.UUID, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`SELECT DISTINCT user_id FROM workspace_nudges WHERE status = 'open'`)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/workspaceNudge UsersWithOpenNudges failed: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PurgeOldTerminal deletes terminal (dismissed/acted/superseded) nudges older
// than `olderThan` so the table doesn't grow unbounded. Returns deleted count.
func PurgeOldTerminal(ctx context.Context, olderThan time.Duration) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	cutoff := time.Now().Add(-olderThan)
	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`DELETE FROM workspace_nudges
		 WHERE status IN ('dismissed','acted','superseded') AND updated_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// --- helpers ---

type scannable interface {
	Scan(dest ...any) error
}

func scanNudge(s scannable) (*Nudge, error) {
	var n Nudge
	var ctaURL, ctaText, srcType, srcID sql.NullString
	if err := s.Scan(
		&n.ID, &n.UserID, &n.Kind, &n.Title, &n.Body, &ctaURL, &ctaText,
		&srcType, &srcID, &n.Status, &n.Priority, &n.CreatedAt, &n.UpdatedAt,
	); err != nil {
		return nil, err
	}
	n.CTAURL = ctaURL.String
	n.CTAText = ctaText.String
	n.SourceType = srcType.String
	n.SourceID = srcID.String
	return &n, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
