package models

// Postgres access for doc snapshots (migration 88) - document version history.
// Mirrors the board snapshot model: this is the index table; the gzipped HTML
// body blob lives in MinIO, referenced by object_key. Retention is bounded by a
// per-doc cap (PruneDocSnapshots) and an age sweep (ListExpiredSnapshots).

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

// Snapshot reasons - keep aligned with the CHECK constraint in migration 88.
const (
	ReasonInterval   = "interval"
	ReasonMassDelete = "mass_delete"
	ReasonManual     = "manual"
)

// DocSnapshot is the in-memory form of a doc_snapshots row.
type DocSnapshot struct {
	ID               uuid.UUID `json:"id"`
	DocUUID          string    `json:"doc_uuid"`
	ObjectKey        string    `json:"-"`
	BodyBytes        int       `json:"body_bytes"`
	Reason           string    `json:"reason"`
	ContributorUUIDs []string  `json:"contributor_uuids"`
	CreatedAt        time.Time `json:"created_at"`
}

// ExpiredSnapshot is the minimal projection the age-sweep needs.
type ExpiredSnapshot struct {
	ID        uuid.UUID
	ObjectKey string
}

// CreateSnapshot inserts a snapshot index row and returns its id.
func CreateSnapshot(ctx context.Context, s *DocSnapshot) (uuid.UUID, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var id uuid.UUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`INSERT INTO doc_snapshots (doc_uuid, object_key, body_bytes, reason, contributor_uuids)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id`,
		s.DocUUID, s.ObjectKey, s.BodyBytes, s.Reason, pq.Array(s.ContributorUUIDs)).Scan(&id)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot CreateSnapshot failed: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// GetLatestSnapshot returns the most recent snapshot for a doc, or nil.
func GetLatestSnapshot(ctx context.Context, docUUID string) (*DocSnapshot, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	row := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT id, doc_uuid, object_key, body_bytes, reason, contributor_uuids, created_at
		   FROM doc_snapshots
		  WHERE doc_uuid = $1
		  ORDER BY created_at DESC
		  LIMIT 1`, docUUID)

	s, err := scanSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot GetLatestSnapshot failed: %+v", err)
		return nil, err
	}
	return s, nil
}

// ListSnapshots returns a doc's snapshots newest-first, capped by limit.
func ListSnapshots(ctx context.Context, docUUID string, limit int) ([]*DocSnapshot, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`SELECT id, doc_uuid, object_key, body_bytes, reason, contributor_uuids, created_at
		   FROM doc_snapshots
		  WHERE doc_uuid = $1
		  ORDER BY created_at DESC
		  LIMIT $2`, docUUID, limit)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot ListSnapshots failed: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*DocSnapshot
	for rows.Next() {
		s, serr := scanSnapshot(rows)
		if serr != nil {
			helpers.LogErrorWithContext(cctx, "models/DocSnapshot ListSnapshots scan failed: %+v", serr)
			return nil, serr
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSnapshotByID returns a single snapshot, scoped to its doc.
func GetSnapshotByID(ctx context.Context, id uuid.UUID, docUUID string) (*DocSnapshot, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	row := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT id, doc_uuid, object_key, body_bytes, reason, contributor_uuids, created_at
		   FROM doc_snapshots
		  WHERE id = $1 AND doc_uuid = $2`, id, docUUID)

	s, err := scanSnapshot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot GetSnapshotByID failed: %+v", err)
		return nil, err
	}
	return s, nil
}

// PruneDocSnapshots deletes the doc's snapshots beyond the most recent `keep`
// and returns the object keys of the deleted rows for MinIO cleanup.
func PruneDocSnapshots(ctx context.Context, docUUID string, keep int) ([]string, error) {
	if keep < 1 {
		keep = 1
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`DELETE FROM doc_snapshots
		  WHERE id IN (
		      SELECT id FROM doc_snapshots
		       WHERE doc_uuid = $1
		       ORDER BY created_at DESC
		       OFFSET $2
		  )
		  RETURNING object_key`, docUUID, keep)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot PruneDocSnapshots failed: %+v", err)
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

// ListExpiredSnapshots returns snapshots older than cutoff (id + object_key).
func ListExpiredSnapshots(ctx context.Context, cutoff time.Time, limit int) ([]ExpiredSnapshot, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`SELECT id, object_key
		   FROM doc_snapshots
		  WHERE created_at < $1
		  ORDER BY created_at ASC
		  LIMIT $2`, cutoff, limit)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot ListExpiredSnapshots failed: %+v", err)
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
		`DELETE FROM doc_snapshots WHERE id = ANY($1::uuid[])`,
		"{"+strings.Join(strIDs, ",")+"}")
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot DeleteByIDs failed: %+v", err)
	}
	return err
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanSnapshot(r rowScanner) (*DocSnapshot, error) {
	var s DocSnapshot
	err := r.Scan(&s.ID, &s.DocUUID, &s.ObjectKey, &s.BodyBytes, &s.Reason, pq.Array(&s.ContributorUUIDs), &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	if s.ContributorUUIDs == nil {
		s.ContributorUUIDs = []string{}
	}
	return &s, nil
}
