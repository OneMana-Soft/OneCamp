package models

// The backups of the merge of people's stray graph nodes
// (person_merge_backups; business/User strayMerge.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// MergeBackup is one row: a stray as the graph held it (Kind "node", with
// its predicates in Node), or an edge into it ("in_edge", from OtherUID) or
// out of it ("out_edge", to OtherUID).
type MergeBackup struct {
	Kind      string
	UserUUID  string
	PersonUID string
	StrayUID  string
	Predicate string
	OtherUID  string
	List      bool
	// Added is that the merge gave the person's own node this edge.
	Added  bool
	Facets json.RawMessage
	Node   json.RawMessage
}

// mergeBackupInsert is how many rows one INSERT writes: 10 parameters a row
// besides the run and batch, well under Postgres's 65,535.
const mergeBackupInsert = 500

// BackUpMergeBatch writes a batch's rows as pending, in place of any an
// earlier try of the same batch left.
func BackUpMergeBatch(ctx context.Context, run, batch uuid.UUID, rows []MergeBackup) error {
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		if err := dropMergeBatch(ctx, run, batch); err != nil {
			return err
		}
		for start := 0; start < len(rows); start += mergeBackupInsert {
			chunk := rows[start:min(start+mergeBackupInsert, len(rows))]
			values := make([]string, len(chunk))
			args := []any{run, batch}
			for i, r := range chunk {
				n := len(args)
				values[i] = fmt.Sprintf("($1, $2, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
					n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9, n+10)
				args = append(args, r.Kind, r.UserUUID, r.PersonUID, r.StrayUID, orNull(r.Predicate), orNull(r.OtherUID),
					r.List, r.Added, orNull(string(r.Facets)), orNull(string(r.Node)))
			}
			if _, err := pgexec.Exec(ctx, `INSERT INTO person_merge_backups
				(run_id, batch_id, kind, user_uuid, person_uid, stray_uid, predicate, other_uid, list, added, facets, node)
				VALUES `+strings.Join(values, ", "), args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// orNull is s, or NULL when it's empty.
func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// MarkMergeBatch moves a batch's rows from one state to another: pending to
// applied once its graph change has landed, applied to restored once a
// restore has put it back.
func MarkMergeBatch(ctx context.Context, run, batch uuid.UUID, from, to string) error {
	_, err := pgexec.Exec(ctx, `UPDATE person_merge_backups SET state = $4
		WHERE run_id = $1 AND batch_id = $2 AND state = $3`, run, batch, from, to)
	return err
}

// dropMergeBatch deletes the pending rows an earlier try of a batch left.
func dropMergeBatch(ctx context.Context, run, batch uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `DELETE FROM person_merge_backups
		WHERE run_id = $1 AND batch_id = $2 AND state = 'pending'`, run, batch)
	return err
}

// SettleMergeBatches settles pending batches in one transaction: the rows of
// those whose graph change landed are marked applied, and those of the ones
// whose change never did go.
func SettleMergeBatches(ctx context.Context, landed, never []uuid.UUID) error {
	ids := func(batches []uuid.UUID) any {
		out := make([]string, len(batches))
		for i, b := range batches {
			out[i] = b.String()
		}
		return pq.Array(out)
	}
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `UPDATE person_merge_backups SET state = 'applied'
			WHERE state = 'pending' AND batch_id = ANY($1::uuid[])`, ids(landed)); err != nil {
			return err
		}
		_, err := pgexec.Exec(ctx, `DELETE FROM person_merge_backups
			WHERE state = 'pending' AND batch_id = ANY($1::uuid[])`, ids(never))
		return err
	})
}

// PendingMergeBatch is a batch whose rows were written and never marked, so
// its graph change may or may not have landed, with the strays it deletes.
type PendingMergeBatch struct {
	Run, Batch uuid.UUID
	Strays     []string
}

// PendingMergeBatches lists them.
func PendingMergeBatches(ctx context.Context) ([]PendingMergeBatch, error) {
	return pgexec.Query(ctx, `SELECT run_id, batch_id, array_agg(stray_uid) FROM person_merge_backups
		WHERE state = 'pending' AND kind = 'node' GROUP BY run_id, batch_id`,
		func(r pgexec.Row) (PendingMergeBatch, error) {
			var b PendingMergeBatch
			err := r.Scan(&b.Run, &b.Batch, pq.Array(&b.Strays))
			return b, err
		})
}

// AppliedMergeBackups is a run's rows whose graph change landed and hasn't
// been put back, by batch, in the order they were written.
func AppliedMergeBackups(ctx context.Context, run uuid.UUID) (batches []uuid.UUID, rows map[uuid.UUID][]MergeBackup, err error) {
	type row struct {
		batch uuid.UUID
		MergeBackup
	}
	got, err := pgexec.Query(ctx, `SELECT batch_id, kind, user_uuid, person_uid, stray_uid, COALESCE(predicate, ''),
			COALESCE(other_uid, ''), list, added, facets, node
		FROM person_merge_backups WHERE run_id = $1 AND state = 'applied' ORDER BY id`,
		func(r pgexec.Row) (row, error) {
			var b row
			var facets, node []byte
			err := r.Scan(&b.batch, &b.Kind, &b.UserUUID, &b.PersonUID, &b.StrayUID, &b.Predicate, &b.OtherUID,
				&b.List, &b.Added, &facets, &node)
			b.Facets, b.Node = facets, node
			return b, err
		}, run)
	if err != nil {
		return nil, nil, err
	}
	rows = map[uuid.UUID][]MergeBackup{}
	for _, r := range got {
		if rows[r.batch] == nil {
			batches = append(batches, r.batch)
		}
		rows[r.batch] = append(rows[r.batch], r.MergeBackup)
	}
	return batches, rows, nil
}
