// Package models (SoftDelete) is the one soft delete for the parts of a
// project a person deletes from its settings, a person's time entries and a
// draft invoice (V-78, the owner: "all the deletes were soft delete").
//
// A delete marks the row (deleted_at, deleted_by) and keeps it, so what hangs
// off it stays too: a field's task values, a cycle's or a milestone's task
// links, a form's submissions. A restore clears the mark and they are back.
//
// Every read of these tables says Live: a deleted row is gone from lists,
// counts, lookups and joins until it is restored. The restore route
// (controllers/Restore) names a row by Kind; each kind's own access check
// reads who the row belongs to through DeletedOwner.
//
// A kind whose create holds a most (a project's 30 fields, the workspace's
// 300 labels) is capped: its create and its restore share a Bound, the same
// lock, count and words, so a delete and a restore never lift a count past
// what a create may make (V-184 head 2).
package models

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Kind is what the restore route calls a soft-deletable row.
type Kind string

const (
	Field         Kind = "field"          // a project's custom task field (task_fields)
	Status        Kind = "status"         // a project's custom status (task_statuses)
	Cycle         Kind = "cycle"          // a project's cycle (project_cycles)
	Milestone     Kind = "milestone"      // a milestone, with the projects it's shared to (milestones)
	Form          Kind = "form"           // a project's intake form or a table's form (project_forms)
	TimeEntry     Kind = "time_entry"     // a person's time entry (task_time_entries)
	Invoice       Kind = "invoice"        // a project's draft invoice (invoices)
	Link          Kind = "link"           // a project's link to a tool elsewhere (project_links)
	ProjectStatus Kind = "project_status" // one of the workspace's own project statuses (project_statuses)
	ProjectLabel  Kind = "project_label"  // one of the workspace's project labels (project_labels)
)

// table is a kind's table: its name, whether its rows carry updated_at,
// whether its create holds a most (capped: its restore needs a Bound), the
// columns that say whom a row belongs to ("" where it has none), and, where a
// restore can stop partway, pending: the column that holds what it has still
// to bring back, so a live row with it set is still to restore (Restorable).
type table struct {
	name                         string
	stamped, capped              bool
	project, owner, person, task string
	pending                      string
}

var tables = map[Kind]table{
	Field:         {name: "task_fields", stamped: true, capped: true, project: "project_id"},
	Status:        {name: "task_statuses", stamped: true, capped: true, project: "project_id", pending: "deleted_moves"},
	Cycle:         {name: "project_cycles", project: "project_uuid"},
	Milestone:     {name: "milestones", capped: true, project: "project_uuid"},
	Form:          {name: "project_forms", stamped: true, capped: true, project: "project_uuid", owner: "target_table"},
	TimeEntry:     {name: "task_time_entries", project: "project_uuid", person: "user_id", task: "task_uuid"},
	Invoice:       {name: "invoices", stamped: true, project: "project_id"},
	Link:          {name: "project_links", capped: true, project: "project_uuid"},
	ProjectStatus: {name: "project_statuses", stamped: true, capped: true},
	ProjectLabel:  {name: "project_labels", stamped: true, capped: true},
}

var (
	// ErrNoSuchKind is a kind the restore route doesn't know.
	ErrNoSuchKind = errors.New("nothing of that kind can be restored")
	// ErrTaken is a restore whose place a live row has taken since the
	// delete: a field's or a status's name, a person's running timer.
	ErrTaken = errors.New("a live one has taken its place")
	// ErrUnbounded is a capped kind's restore that names no Bound: it could
	// bring a row back past what the kind's create may make.
	ErrUnbounded = errors.New("a capped kind's restore needs its bound")
)

// Bound is one most that a capped kind's rows stay within, the same for its
// create and its restore: the transaction lock both take (Lock), the most
// (Most), how many live rows count against it (Count, which reads through
// pgexec, so inside the write's own transaction), and what a write past it
// answers (Full: the create's refusal, so a restore says the same words).
type Bound struct {
	Lock  string
	Most  int
	Count func(context.Context) (int, error)
	Full  error
}

// Add runs add, a create's insert, while b has room: under b's lock, after a
// count below its most (pgexec.Capped). With no room it adds nothing and
// answers b.Full.
func (b Bound) Add(ctx context.Context, add func(context.Context) error) error {
	added, err := pgexec.Capped(ctx, b.Lock, b.Most, b.Count, add)
	if err == nil && !added {
		return b.Full
	}
	return err
}

// lockOrder is bounds' locks sorted, once each. Every restore takes its locks
// in this order, so two restores over the same projects (a milestone shown in
// several) never wait on each other in a ring.
func lockOrder(bounds []Bound) []string {
	locks := make([]string, 0, len(bounds))
	for _, b := range bounds {
		locks = append(locks, b.Lock)
	}
	slices.Sort(locks)
	return slices.Compact(locks)
}

// ParseKind is the Kind a route names, and whether there is one.
func ParseKind(s string) (Kind, bool) {
	_, ok := tables[Kind(s)]
	return Kind(s), ok
}

// By is the deleted_by a soft delete writes for who took the row away: NULL
// for nobody (a job, a sweep), never the zero id. Tables outside the kinds
// (a grant keyed by its project and person) mark their rows with it too.
func By(who uuid.UUID) any {
	if who == uuid.Nil {
		return nil
	}
	return who
}

// Live is the live-rows predicate: every read of these tables says it, for
// the table's alias in the statement ("" for none).
func Live(alias string) string {
	if alias == "" {
		return "deleted_at IS NULL"
	}
	return alias + ".deleted_at IS NULL"
}

// Delete marks the live row of kind with id deleted, by whom, where scope
// holds too: a condition over the row's own columns ("project_id = $3"),
// its arguments from $3 on. It answers whether a live row matched.
func Delete(ctx context.Context, kind Kind, id, by uuid.UUID, scope string, args ...any) (bool, error) {
	n, err := DeleteAll(ctx, kind, []uuid.UUID{id}, by, scope, args...)
	return n > 0, err
}

// DeleteAll is Delete for every live row of kind among ids, in one statement,
// and answers how many it marked. A scope that looks into another table names
// the row by its table ("task_fields.id"). by is uuid.Nil when no one person
// did it: an import's rollback.
func DeleteAll(ctx context.Context, kind Kind, ids []uuid.UUID, by uuid.UUID, scope string, args ...any) (int64, error) {
	t, ok := tables[kind]
	if !ok {
		return 0, ErrNoSuchKind
	}
	if len(ids) == 0 {
		return 0, nil
	}
	q := fmt.Sprintf(`UPDATE %s SET deleted_at = NOW(), deleted_by = $2%s WHERE id = ANY($1) AND deleted_at IS NULL`, t.name, t.touch())
	if scope != "" {
		q += " AND (" + scope + ")"
	}
	return pgexec.Exec(ctx, q, append([]any{pq.Array(ids), uuid.NullUUID{UUID: by, Valid: by != uuid.Nil}}, args...)...)
}

// Restore brings the deleted row of kind with id back and answers whether one
// was deleted. ErrTaken: a live row has taken its place since.
//
// A capped kind's restore names its bounds (a milestone's: one for each
// project that shows it) and is held to them as its create is: under their
// locks the row comes back, and when a count is then past its most the
// restore is undone and answers that bound's Full. With no bound, a capped
// kind's restore is refused before it reads anything (ErrUnbounded).
func Restore(ctx context.Context, kind Kind, id uuid.UUID, bounds ...Bound) (bool, error) {
	t, ok := tables[kind]
	if !ok {
		return false, ErrNoSuchKind
	}
	if len(bounds) == 0 {
		if t.capped {
			return false, ErrUnbounded
		}
		return t.restore(ctx, id)
	}
	restored := false
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		for _, lock := range lockOrder(bounds) {
			if _, err := pgexec.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lock); err != nil {
				return err
			}
		}
		back, err := t.restore(ctx, id)
		if err != nil || !back {
			return err
		}
		// Live again, it counts: more than a most means there was no room.
		for _, b := range bounds {
			n, err := b.Count(ctx)
			if err != nil {
				return err
			}
			if n > b.Most {
				return b.Full
			}
		}
		restored = true
		return nil
	})
	return restored, err
}

// restore clears the deleted row's mark and answers whether there was one.
func (t table) restore(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := pgexec.Exec(ctx, fmt.Sprintf(`UPDATE %s SET deleted_at = NULL, deleted_by = NULL%s WHERE id = $1 AND deleted_at IS NOT NULL`, t.name, t.touch()), id)
	if helpers.IsUniqueViolation(err) {
		return false, ErrTaken
	}
	return n > 0, err
}

// RestoreAs is Restore for a caller with words of its own for ErrTaken (nil
// keeps ErrTaken). A row someone restored a moment ago is restored: nil.
func RestoreAs(ctx context.Context, kind Kind, id uuid.UUID, taken error, bounds ...Bound) error {
	_, err := Restore(ctx, kind, id, bounds...)
	if errors.Is(err, ErrTaken) && taken != nil {
		return taken
	}
	return err
}

// Restorable is the predicate for the rows of kind that a restore takes: the
// deleted ones, and, for a kind whose restore can stop partway, the live ones
// it has still to finish. The restore route and the kind's own restore both
// say it, so a restore the route lets through is one the kind can finish.
func Restorable(kind Kind) string {
	return tables[kind].restorable()
}

func (t table) restorable() string {
	if t.pending == "" {
		return "deleted_at IS NOT NULL"
	}
	return "(deleted_at IS NOT NULL OR " + t.pending + " IS NOT NULL)"
}

func (t table) touch() string {
	if t.stamped {
		return ", updated_at = NOW()"
	}
	return ""
}

// Owner is whom a deleted row belongs to, for its restore's check: its
// project, a table's form's table, a time entry's person and task (uuid.Nil:
// none).
type Owner struct {
	Project, Table, Person, Task uuid.UUID
}

// DeletedOwner reads whom the row of kind with id that a restore would take
// (Restorable) belongs to, and whether there is one.
func DeletedOwner(ctx context.Context, kind Kind, id uuid.UUID) (Owner, bool, error) {
	t, ok := tables[kind]
	if !ok {
		return Owner{}, false, ErrNoSuchKind
	}
	col := func(c string) string {
		if c == "" {
			return "NULL::uuid"
		}
		return c
	}
	q := fmt.Sprintf(`SELECT %s, %s, %s, %s FROM %s WHERE id = $1 AND %s`, col(t.project), col(t.owner), col(t.person), col(t.task), t.name, t.restorable())
	return pgexec.QueryOne(ctx, q, func(r pgexec.Row) (Owner, error) {
		var p, tb, ps, tk uuid.NullUUID
		err := r.Scan(&p, &tb, &ps, &tk)
		return Owner{Project: p.UUID, Table: tb.UUID, Person: ps.UUID, Task: tk.UUID}, err
	}, id)
}
