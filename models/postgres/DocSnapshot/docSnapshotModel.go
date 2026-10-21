package models

// Postgres access for doc versions (doc_snapshots: migration 88, and the doc
// versions migration): the index of a doc's version history. Each version's body, gzipped HTML, lives in
// object storage under object_key. The Snapshot Service (business/Snapshot) takes,
// names and prunes them; the rules for which to keep live there, not here.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Reasons a version was taken. Keep aligned with the CHECK in the doc versions migration.
const (
	// ReasonInterval: at most every 10 minutes while the doc is being edited.
	ReasonInterval = "interval"
	// ReasonIdle: 2 minutes after the last edit.
	ReasonIdle = "idle"
	// ReasonMassDelete: the body before a save that deleted most of it.
	ReasonMassDelete = "mass_delete"
	// ReasonManual: the body before a restore, so the restore can be undone.
	ReasonManual = "manual"
	// ReasonSaved: kept by someone on purpose ("Save this version").
	ReasonSaved = "saved"
	// ReasonBeforeWrite: the body before a server-side write replaced or
	// added to it (an API write, an import, an agent).
	ReasonBeforeWrite = "before_write"
)

// Kinds of version. Keep aligned with the CHECK in the doc versions migration.
const (
	KindDoc      = "doc"
	KindAgentRun = "agent_run"
)

// Contributor is someone whose edits a version holds: a person, or an agent
// with the person who asked it.
type Contributor struct {
	UserUUID  string `json:"user_uuid,omitempty"`
	AgentUUID string `json:"agent_uuid,omitempty"`
	AskerUUID string `json:"asker_uuid,omitempty"`
}

// DocSnapshot is the in-memory form of a doc_snapshots row.
type DocSnapshot struct {
	ID        uuid.UUID `json:"id"`
	DocUUID   string    `json:"doc_uuid"`
	ObjectKey string    `json:"-"`
	BodyBytes int       `json:"body_bytes"`
	// StoredBytes is the gzipped size in object storage.
	StoredBytes      int           `json:"stored_bytes"`
	Reason           string        `json:"reason"`
	ContributorUUIDs []string      `json:"contributor_uuids"`
	Contributors     []Contributor `json:"contributors"`
	Name             *string       `json:"name,omitempty"`
	Pinned           bool          `json:"pinned"`
	Kind             string        `json:"kind"`
	RunID            string        `json:"run_id,omitempty"`
	Lineage          string        `json:"lineage,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
}

const snapshotColumns = `id, doc_uuid, object_key, body_bytes, stored_bytes, reason, contributor_uuids,
	contributor_meta, name, pinned, kind, run_id, lineage, created_at`

// CreateSnapshot inserts a version's index row and returns its id. The id is
// made here (Postgres 12 has no gen_random_uuid), and so is created_at when
// the caller leaves it zero.
func CreateSnapshot(ctx context.Context, s *DocSnapshot) (uuid.UUID, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	if s.Kind == "" {
		s.Kind = KindDoc
	}
	if s.ContributorUUIDs == nil {
		s.ContributorUUIDs = []string{}
	}
	if s.Contributors == nil {
		s.Contributors = []Contributor{}
	}
	meta, err := json.Marshal(s.Contributors)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`INSERT INTO doc_snapshots (id, doc_uuid, object_key, body_bytes, stored_bytes, reason, contributor_uuids,
		                            contributor_meta, name, pinned, kind, run_id, lineage, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		s.ID, s.DocUUID, s.ObjectKey, s.BodyBytes, s.StoredBytes, s.Reason, pq.Array(s.ContributorUUIDs),
		string(meta), s.Name, s.Pinned, s.Kind, s.RunID, s.Lineage, s.CreatedAt)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot CreateSnapshot failed: %+v", err)
		return uuid.Nil, err
	}
	return s.ID, nil
}

// GetLatestSnapshot returns the most recent version of a doc, or nil.
func GetLatestSnapshot(ctx context.Context, docUUID string) (*DocSnapshot, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	row := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT `+snapshotColumns+`
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

// ListSnapshots returns a doc's versions newest first, capped by limit: those
// older than before when it is set (the next page of a long history).
func ListSnapshots(ctx context.Context, docUUID string, before time.Time, limit int) ([]*DocSnapshot, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if before.IsZero() {
		return querySnapshots(ctx, `SELECT `+snapshotColumns+`
			   FROM doc_snapshots
			  WHERE doc_uuid = $1
			  ORDER BY created_at DESC
			  LIMIT $2`, docUUID, limit)
	}
	return querySnapshots(ctx, `SELECT `+snapshotColumns+`
		   FROM doc_snapshots
		  WHERE doc_uuid = $1 AND created_at < $2
		  ORDER BY created_at DESC
		  LIMIT $3`, docUUID, before, limit)
}

func querySnapshots(ctx context.Context, query string, args ...any) ([]*DocSnapshot, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot query failed: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*DocSnapshot
	for rows.Next() {
		s, serr := scanSnapshot(rows)
		if serr != nil {
			helpers.LogErrorWithContext(cctx, "models/DocSnapshot scan failed: %+v", serr)
			return nil, serr
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSnapshotByID returns a single version, scoped to its doc, or nil.
func GetSnapshotByID(ctx context.Context, id uuid.UUID, docUUID string) (*DocSnapshot, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	row := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT `+snapshotColumns+`
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

// SetSnapshotName names (or, with nil, unnames) a version and pins or unpins
// it. It answers false for no such version of that doc.
func SetSnapshotName(ctx context.Context, id uuid.UUID, docUUID string, name *string, pinned bool) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE doc_snapshots SET name = $3, pinned = $4 WHERE id = $1 AND doc_uuid = $2`,
		id, docUUID, name, pinned)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot SetSnapshotName failed: %+v", err)
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RetentionRow is what pruning decides by.
type RetentionRow struct {
	ID          uuid.UUID
	ObjectKey   string
	Kind        string
	Kept        bool
	StoredBytes int
	CreatedAt   time.Time
}

// ListForRetention is every version of a doc, newest first, with what pruning
// decides by.
func ListForRetention(ctx context.Context, docUUID string) ([]RetentionRow, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`SELECT id, object_key, kind, (pinned OR name IS NOT NULL), stored_bytes, created_at
		   FROM doc_snapshots
		  WHERE doc_uuid = $1
		  ORDER BY created_at DESC`, docUUID)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot ListForRetention failed: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []RetentionRow
	for rows.Next() {
		var r RetentionRow
		if err := rows.Scan(&r.ID, &r.ObjectKey, &r.Kind, &r.Kept, &r.StoredBytes, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DocsWithVersionsBefore pages through the docs that have a version older than
// cutoff that nobody kept, in doc order after `after`: the ones a retention
// pass may thin.
func DocsWithVersionsBefore(ctx context.Context, cutoff time.Time, after string, limit int) ([]string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`SELECT DISTINCT doc_uuid
		   FROM doc_snapshots
		  WHERE created_at < $1 AND NOT pinned AND name IS NULL AND doc_uuid > $2
		  ORDER BY doc_uuid
		  LIMIT $3`, cutoff, after, limit)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot DocsWithVersionsBefore failed: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// KeptBytes is what a doc's named and pinned versions take in object storage.
func KeptBytes(ctx context.Context, docUUID string) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var n int64
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT COALESCE(SUM(stored_bytes), 0) FROM doc_snapshots WHERE doc_uuid = $1 AND (pinned OR name IS NOT NULL)`,
		docUUID).Scan(&n)
	return n, err
}

// DeleteUnkeptByIDs removes versions by id, never one someone named or pinned
// meanwhile, and answers the object keys of those it removed.
func DeleteUnkeptByIDs(ctx context.Context, ids []uuid.UUID) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	strIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		strIDs = append(strIDs, id.String())
	}
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`DELETE FROM doc_snapshots
		  WHERE id = ANY($1::uuid[]) AND NOT pinned AND name IS NULL
		  RETURNING object_key`,
		"{"+strings.Join(strIDs, ",")+"}")
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/DocSnapshot DeleteUnkeptByIDs failed: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return keys, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanSnapshot(r rowScanner) (*DocSnapshot, error) {
	var s DocSnapshot
	var meta []byte
	err := r.Scan(&s.ID, &s.DocUUID, &s.ObjectKey, &s.BodyBytes, &s.StoredBytes, &s.Reason, pq.Array(&s.ContributorUUIDs),
		&meta, &s.Name, &s.Pinned, &s.Kind, &s.RunID, &s.Lineage, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	if s.ContributorUUIDs == nil {
		s.ContributorUUIDs = []string{}
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &s.Contributors)
	}
	if s.Contributors == nil {
		s.Contributors = []Contributor{}
	}
	return &s, nil
}
