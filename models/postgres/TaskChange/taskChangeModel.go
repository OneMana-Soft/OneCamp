package models

// task_changes (migration 543): the store of the Work Change Log
// (business/WorkFeed).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/lib/pq"
)

// New is one change to keep.
type New struct {
	ProjectUUID  string
	ResourceKind string
	ResourceUUID string
	Kind         string
	Diff         json.RawMessage
	ActorUUID    string
	Source       string
	Restricted   bool
	At           time.Time
}

// Kept is a change as kept, with its number.
type Kept struct {
	Sequence int64
	New
}

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

// Append keeps changes in the order given and answers their numbers in that
// order. Appends take one transaction lock, so numbers commit in order and a
// reader never moves past one still being written.
func Append(ctx context.Context, changes []New) ([]int64, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var q strings.Builder
	q.WriteString(`INSERT INTO task_changes (project_uuid, resource_kind, resource_uuid, kind, diff, actor_uuid, source, restricted, changed_at) VALUES `)
	args := make([]any, 0, len(changes)*9)
	for i, c := range changes {
		if i > 0 {
			q.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&q, "($%d, $%d, $%d, $%d, $%d, NULLIF($%d, '')::uuid, $%d, $%d, $%d)", n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9)
		diff, source, at := c.Diff, c.Source, c.At
		if len(diff) == 0 {
			diff = json.RawMessage(`{}`)
		}
		if source == "" {
			source = "app"
		}
		if at.IsZero() {
			at = time.Now()
		}
		args = append(args, c.ProjectUUID, c.ResourceKind, c.ResourceUUID, c.Kind, []byte(diff), c.ActorUUID, source, c.Restricted, at)
	}
	q.WriteString(` RETURNING sequence`)
	var seqs []int64
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('task_changes'))`); err != nil {
			return err
		}
		var err error
		seqs, err = pgexec.Query(ctx, q.String(), func(r pgexec.Row) (int64, error) {
			var s int64
			return s, r.Scan(&s)
		}, args...)
		return err
	})
	if err != nil {
		return nil, err
	}
	// Numbers are drawn in VALUES order; RETURNING's order isn't promised.
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	return seqs, nil
}

// After answers up to limit changes numbered after seq, in order.
func After(ctx context.Context, seq int64, limit int) ([]Kept, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return pgexec.Query(ctx, `SELECT sequence, project_uuid::text, resource_kind, resource_uuid::text, kind, diff,
		COALESCE(actor_uuid::text, ''), source, restricted, changed_at
		FROM task_changes WHERE sequence > $1 ORDER BY sequence LIMIT $2`,
		func(r pgexec.Row) (Kept, error) {
			var k Kept
			var diff []byte
			err := r.Scan(&k.Sequence, &k.ProjectUUID, &k.ResourceKind, &k.ResourceUUID, &k.Kind, &diff,
				&k.ActorUUID, &k.Source, &k.Restricted, &k.At)
			k.Diff = diff
			return k, err
		}, seq, limit)
}

// HeadAndOldest answers the newest and the oldest numbers kept, 0 for none.
func HeadAndOldest(ctx context.Context) (head, oldest int64, err error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	type ends struct{ head, oldest int64 }
	e, _, err := pgexec.QueryOne(ctx, `SELECT COALESCE(max(sequence), 0), COALESCE(min(sequence), 0) FROM task_changes`,
		func(r pgexec.Row) (ends, error) {
			var e ends
			return e, r.Scan(&e.head, &e.oldest)
		})
	return e.head, e.oldest, err
}

// FirstToKeep is the oldest change a prune of those made before cutoff
// keeps: the first numbered at or after cutoff, or the newest if none is, so
// a prune takes only from the oldest end, leaves no hole a reader could step
// over, and always leaves one change (the oldest kept then tells a reader
// whose place is older that it missed what went). 0 is an empty log.
func FirstToKeep(ctx context.Context, cutoff time.Time) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return pgexec.Scalar[int64](ctx, `SELECT COALESCE(
		(SELECT sequence FROM task_changes WHERE changed_at >= $1 ORDER BY sequence LIMIT 1),
		(SELECT max(sequence) FROM task_changes), 0)`, cutoff)
}

// DeleteBefore deletes up to limit of the oldest changes numbered before seq,
// in one statement, and answers how many went.
func DeleteBefore(ctx context.Context, seq int64, limit int) (int64, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return pgexec.Exec(ctx, `DELETE FROM task_changes WHERE sequence IN (
		SELECT sequence FROM task_changes WHERE sequence < $1 ORDER BY sequence LIMIT $2)`, seq, limit)
}

// LatestValues answers, for each of fields, its value in the newest change to
// the resource numbered before `before` that names it: the value the change
// numbered `before` replaced. A field no earlier change names (older than the
// log, or pruned) is left out; a JSON null is kept. One read, newest first on
// the resource's own changes (idx_task_changes_task, migration 547).
func LatestValues(ctx context.Context, resource string, before int64, fields []string) (map[string]json.RawMessage, error) {
	if len(fields) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	type named struct {
		field string
		value json.RawMessage
	}
	found, err := pgexec.Query(ctx, `SELECT f.name, prev.value
		FROM unnest($3::text[]) AS f(name)
		CROSS JOIN LATERAL (
			SELECT diff -> f.name AS value FROM task_changes
			WHERE resource_uuid = $1::uuid AND sequence < $2 AND diff -> f.name IS NOT NULL
			ORDER BY sequence DESC LIMIT 1
		) prev`,
		func(r pgexec.Row) (named, error) {
			var n named
			var v []byte
			err := r.Scan(&n.field, &v)
			n.value = v
			return n, err
		}, resource, before, pq.Array(fields))
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(found))
	for _, n := range found {
		out[n.field] = n.value
	}
	return out, nil
}

// LatestFieldValue answers a custom field's value in the newest change to the
// resource numbered before `before` that set it (business/TaskField's shape,
// {"field_id", "value"}), and whether any did. One read, as LatestValues.
func LatestFieldValue(ctx context.Context, resource string, before int64, fieldID string) (json.RawMessage, bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	found, err := pgexec.Query(ctx, `SELECT diff -> 'value' FROM task_changes
		WHERE resource_uuid = $1::uuid AND sequence < $2 AND diff ->> 'field_id' = $3 AND diff -> 'value' IS NOT NULL
		ORDER BY sequence DESC LIMIT 1`,
		func(r pgexec.Row) (json.RawMessage, error) {
			var v []byte
			err := r.Scan(&v)
			return v, err
		}, resource, before, fieldID)
	if err != nil || len(found) == 0 {
		return nil, false, err
	}
	return found[0], true, nil
}
