package models

// Postgres access for board snapshots (migration 85). This package is the
// single gateway to the board_snapshots index table; the snapshot blob itself
// lives in object storage (MinIO) and is referenced here by object_key.
//
// Snapshots give boards a recoverable version history and protect against
// accidental mass-deletion of the canvas. Retention is bounded by a per-board
// cap (PruneBoardSnapshots) and an age sweep (ListExpiredSnapshots), so the
// table and the backing object store never grow without limit.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Snapshot reasons - keep aligned with the CHECK constraint in migration 85.
const (
	ReasonInterval   = "interval"
	ReasonMassDelete = "mass_delete"
	ReasonManual     = "manual"
)

// BoardSnapshot is the in-memory form of a board_snapshots row.
type BoardSnapshot struct {
	ID           uuid.UUID `json:"id"`
	BoardUUID    string    `json:"board_uuid"`
	ObjectKey    string    `json:"-"`
	ElementCount int       `json:"element_count"`
	StateBytes   int       `json:"state_bytes"`
	Reason       string    `json:"reason"`
	// ContributorUUIDs is the set of users who edited the board in the window
	// leading up to this snapshot (version "edited by"). May be empty.
	ContributorUUIDs []string  `json:"contributor_uuids"`
	CreatedAt        time.Time `json:"created_at"`
}

// ExpiredSnapshot is the minimal projection the age-sweep needs to delete the
// MinIO object and then the row.
type ExpiredSnapshot struct {
	ID        uuid.UUID
	ObjectKey string
}

// CreateSnapshot inserts a snapshot index row and returns its id.
func CreateSnapshot(ctx context.Context, s *BoardSnapshot) (uuid.UUID, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var id uuid.UUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`INSERT INTO board_snapshots (board_uuid, object_key, element_count, state_bytes, reason, contributor_uuids)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		s.BoardUUID, s.ObjectKey, s.ElementCount, s.StateBytes, s.Reason, pq.Array(s.ContributorUUIDs)).Scan(&id)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/BoardSnapshot CreateSnapshot failed: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// GetLatestSnapshot returns the most recent snapshot for a board, or nil when
// none exist. Used for cadence and mass-delete comparisons.
func GetLatestSnapshot(ctx context.Context, boardUUID string) (*BoardSnapshot, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	row := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT id, board_uuid, object_key, element_count, state_bytes, reason, contributor_uuids, created_at
		   FROM board_snapshots
		  WHERE board_uuid = $1
		  ORDER BY created_at DESC
		  LIMIT 1`, boardUUID)

	s, err := scanSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/BoardSnapshot GetLatestSnapshot failed: %+v", err)
		return nil, err
	}
	return s, nil
}

// ListSnapshots returns a board's snapshots newest-first, capped by limit.
func ListSnapshots(ctx context.Context, boardUUID string, limit int) ([]*BoardSnapshot, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`SELECT id, board_uuid, object_key, element_count, state_bytes, reason, contributor_uuids, created_at
		   FROM board_snapshots
		  WHERE board_uuid = $1
		  ORDER BY created_at DESC
		  LIMIT $2`, boardUUID, limit)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/BoardSnapshot ListSnapshots failed: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*BoardSnapshot
	for rows.Next() {
		s, serr := scanSnapshotRows(rows)
		if serr != nil {
			helpers.LogErrorWithContext(cctx, "models/BoardSnapshot ListSnapshots scan failed: %+v", serr)
			return nil, serr
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSnapshotByID returns a single snapshot, scoped to its board so a caller
// can never restore another board's snapshot by guessing an id.
func GetSnapshotByID(ctx context.Context, id uuid.UUID, boardUUID string) (*BoardSnapshot, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	row := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT id, board_uuid, object_key, element_count, state_bytes, reason, contributor_uuids, created_at
		   FROM board_snapshots
		  WHERE id = $1 AND board_uuid = $2`, id, boardUUID)

	s, err := scanSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/BoardSnapshot GetSnapshotByID failed: %+v", err)
		return nil, err
	}
	return s, nil
}

// PruneBoardSnapshots deletes the board's snapshots beyond the most recent
// `keep` and returns the object keys of the deleted rows so the caller can
// remove the backing MinIO objects. Bounds per-board storage.
func PruneBoardSnapshots(ctx context.Context, boardUUID string, keep int) ([]string, error) {
	if keep < 1 {
		keep = 1
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`DELETE FROM board_snapshots
		  WHERE id IN (
		      SELECT id FROM board_snapshots
		       WHERE board_uuid = $1
		       ORDER BY created_at DESC
		       OFFSET $2
		  )
		  RETURNING object_key`, boardUUID, keep)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/BoardSnapshot PruneBoardSnapshots failed: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if serr := rows.Scan(&k); serr != nil {
			return keys, serr
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// ListExpiredSnapshots returns snapshots older than cutoff (id + object_key),
// capped by limit so a backlog after downtime can't hammer object storage in
// one tick. The caller deletes the MinIO objects then calls DeleteByIDs.
func ListExpiredSnapshots(ctx context.Context, cutoff time.Time, limit int) ([]ExpiredSnapshot, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`SELECT id, object_key
		   FROM board_snapshots
		  WHERE created_at < $1
		  ORDER BY created_at ASC
		  LIMIT $2`, cutoff, limit)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/BoardSnapshot ListExpiredSnapshots failed: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []ExpiredSnapshot
	for rows.Next() {
		var e ExpiredSnapshot
		if serr := rows.Scan(&e.ID, &e.ObjectKey); serr != nil {
			return out, serr
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteByIDs removes snapshot rows by id (after their MinIO objects are gone).
func DeleteByIDs(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	strIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		strIDs = append(strIDs, id.String())
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`DELETE FROM board_snapshots WHERE id = ANY($1::uuid[])`,
		"{"+strings.Join(strIDs, ",")+"}")
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/BoardSnapshot DeleteByIDs failed: %+v", err)
	}
	return err
}

// --- scan helpers ----------------------------------------------------------

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanSnapshot(r rowScanner) (*BoardSnapshot, error) {
	var s BoardSnapshot
	err := r.Scan(&s.ID, &s.BoardUUID, &s.ObjectKey, &s.ElementCount, &s.StateBytes, &s.Reason, pq.Array(&s.ContributorUUIDs), &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	if s.ContributorUUIDs == nil {
		s.ContributorUUIDs = []string{}
	}
	return &s, nil
}

func scanSnapshotRows(rows *sql.Rows) (*BoardSnapshot, error) {
	return scanSnapshot(rows)
}
