// Package models (Triage) keeps what waits in a project's triage and every
// decision on it (migration 595). Whether a project has triage on is its
// projects.triage policy (business/Policy); business/Triage owns the rules.
// Every statement runs through models/pgexec: the caller's ctx, the pool's
// timeout, and one transaction where a decision changes what it decides.
package models

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	triageDomain "github.com/akashc777/OneCamp/domain/Triage"
	"github.com/akashc777/OneCamp/models/pgexec"
	taskReadGeneration "github.com/akashc777/OneCamp/models/taskread/generation"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Item is a task that came in through triage.
type Item struct {
	TaskUUID    uuid.UUID `json:"task_uuid"`
	ProjectUUID uuid.UUID `json:"project_uuid"`
	Source      string    `json:"source"`
	// Who asked: a member, or a form's visitor by name and email.
	RequesterUUID  *uuid.UUID `json:"requester_uuid,omitempty"`
	RequesterName  string     `json:"requester_name"`
	RequesterEmail string     `json:"requester_email"`
	ArrivedAt      time.Time  `json:"arrived_at"`
	SnoozedUntil   *time.Time `json:"snoozed_until,omitempty"`
}

// Put records a task as waiting in triage. A task put again (a retry) stays
// as it was: a task goes through triage once.
// PutAll keeps a batch of items in one statement; an item whose task waits
// already is left as it is. The task-read generation moves once when the
// statement put any row in (B326's Put bump, once a statement, never a row).
func PutAll(ctx context.Context, its []Item) error {
	if len(its) == 0 {
		return nil
	}
	var tasks, projects, sources, requesters, names, emails []string
	for _, it := range its {
		r := ""
		if it.RequesterUUID != nil {
			r = it.RequesterUUID.String()
		}
		tasks, projects, sources = append(tasks, it.TaskUUID.String()), append(projects, it.ProjectUUID.String()), append(sources, it.Source)
		requesters, names, emails = append(requesters, r), append(names, it.RequesterName), append(emails, it.RequesterEmail)
	}
	n, err := pgexec.Exec(ctx, triageDomain.PutAll, pq.Array(tasks), pq.Array(projects), pq.Array(sources), pq.Array(requesters), pq.Array(names), pq.Array(emails))
	if n > 0 {
		taskReadGeneration.Bump(ctx, "Triage.PutAll")
	}
	return err
}

func CountWaiting(ctx context.Context, projectUUID uuid.UUID) (int, error) {
	return pgexec.Scalar[int](ctx, triageDomain.CountWaiting, projectUUID)
}

// CountAwake is how many tasks wait in a project's triage that no snooze
// holds back now: what the queue lists less what's snoozed.
func CountAwake(ctx context.Context, projectUUID uuid.UUID) (int, error) {
	return pgexec.Scalar[int](ctx, triageDomain.CountAwake, projectUUID)
}

// CountAwakeFor is CountAwake of what reader (a user's id) may see in the
// queue: less the restricted tasks whose rows keep them out.
func CountAwakeFor(ctx context.Context, projectUUID, reader uuid.UUID) (int, error) {
	return pgexec.Scalar[int](ctx, triageDomain.CountAwakeFor, projectUUID, reader)
}

// Waiting is what waits in a project's triage, oldest first, snoozed or not,
// at most limit.
func Waiting(ctx context.Context, projectUUID uuid.UUID, limit int) ([]Item, error) {
	return pgexec.Query(ctx, triageDomain.WaitingList,
		func(r pgexec.Row) (Item, error) {
			var it Item
			var requester uuid.NullUUID
			var snoozed sql.NullTime
			if err := r.Scan(&it.TaskUUID, &it.ProjectUUID, &it.Source, &requester, &it.RequesterName, &it.RequesterEmail, &it.ArrivedAt, &snoozed); err != nil {
				return it, err
			}
			if requester.Valid {
				it.RequesterUUID = &requester.UUID
			}
			if snoozed.Valid {
				it.SnoozedUntil = &snoozed.Time
			}
			return it, nil
		}, projectUUID, limit)
}

// ErrTooManyWaiting is AllWaiting's answer when more than
// triageDomain.MaxWaiting tasks wait: the lists' filter fails closed on it.
var ErrTooManyWaiting = fmt.Errorf("triage: more than %d tasks are waiting", triageDomain.MaxWaiting)

// AllWaiting is every task waiting in any project's triage: its project,
// by its task. It reads at most triageDomain.MaxWaiting+1 rows, and more
// than MaxWaiting waiting is ErrTooManyWaiting.
func AllWaiting(ctx context.Context) (map[uuid.UUID]uuid.UUID, error) {
	all, err := pgexec.Pairs[uuid.UUID, uuid.UUID](ctx, triageDomain.AllWaiting, triageDomain.MaxWaiting+1)
	if err != nil {
		return nil, err
	}
	if len(all) > triageDomain.MaxWaiting {
		return nil, ErrTooManyWaiting
	}
	return all, nil
}

// EndWait ends a task's wait without a decision: the task was deleted while
// it waited, so nobody can decide it. Its item stays, decided now: it no
// longer counts toward the project's cap nor sits in the lists' waiting set.
// No decision is kept, since nobody made one. A task not waiting is left as
// it is.
func EndWait(ctx context.Context, taskUUID uuid.UUID) error {
	n, err := pgexec.Exec(ctx, `UPDATE triage_items SET decided_at = NOW() WHERE task_uuid = $1 AND decided_at IS NULL`, taskUUID)
	if n > 0 {
		taskReadGeneration.Bump(ctx, "Triage.EndWait")
	}
	return err
}

// Decision is one decision on a waiting task.
type Decision struct {
	TaskUUID    uuid.UUID
	ProjectUUID uuid.UUID
	Decision    string
	Reason      string
	DuplicateOf *uuid.UUID
	// SnoozedUntil, for a snooze: the task stays waiting, out of the queue
	// until then.
	SnoozedUntil *time.Time
	DecidedBy    uuid.UUID
}

// Decide keeps a decision on a task waiting in the project's triage. In one
// transaction it locks the task's item while it waits, runs change (the
// task's own change, made with the caller's ctx, outside the transaction),
// then ends the wait, or for a snooze sets when it comes back, and keeps the
// decision. found false, with nothing run or kept, when the task isn't
// waiting there: decided already (by someone deciding at the same time,
// say), or never in triage. An error from change keeps nothing. requester is
// the member who asked for the task, if a member did.
func Decide(ctx context.Context, d Decision, change func(context.Context) error) (found bool, requester *uuid.UUID, err error) {
	err = pgexec.InTx(ctx, func(tx context.Context) error {
		asker, waiting, err := pgexec.QueryOne(tx, `SELECT requester_uuid FROM triage_items
			WHERE task_uuid = $1 AND project_uuid = $2 AND decided_at IS NULL FOR UPDATE`,
			func(r pgexec.Row) (uuid.NullUUID, error) {
				var id uuid.NullUUID
				err := r.Scan(&id)
				return id, err
			}, d.TaskUUID, d.ProjectUUID)
		if err != nil || !waiting {
			return err
		}
		found = true
		if asker.Valid {
			requester = &asker.UUID
		}
		if err := change(ctx); err != nil {
			return err
		}
		if _, err := pgexec.Exec(tx, `UPDATE triage_items SET snoozed_until = $2, decided_at = CASE WHEN $3 THEN NULL ELSE NOW() END
			WHERE task_uuid = $1`, d.TaskUUID, d.SnoozedUntil, d.SnoozedUntil != nil); err != nil {
			return err
		}
		taskReadGeneration.Bump(tx, "Triage.Decide")
		_, err = pgexec.Exec(tx, `INSERT INTO triage_decisions (task_uuid, project_uuid, decision, reason, duplicate_of, snoozed_until, decided_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			d.TaskUUID, d.ProjectUUID, d.Decision, d.Reason, d.DuplicateOf, d.SnoozedUntil, d.DecidedBy)
		return err
	})
	if err != nil || !found {
		return false, nil, err
	}
	return true, requester, nil
}
