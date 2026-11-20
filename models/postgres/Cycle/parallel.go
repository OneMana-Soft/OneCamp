package models

// Parallel cycles (migration 593, jira-04 head C): a project whose admins
// turn them on lets its cycles share days, two teams' sprints side by side as
// in Jira. A task is still in at most one cycle: task_cycles' key is the task.
// Off, the default and what no row in project_settings means, a project's
// cycles run one after another, and every overlap check here holds only then
// (oneAtATime), under the project's cycles lock that SetParallel takes too.

import (
	"context"
	"errors"

	cycleDomain "github.com/akashc777/OneCamp/domain/Cycle"
	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// ErrScheduled is turning parallel cycles on while a schedule runs the
// project's cycles: a schedule makes them one after another.
var ErrScheduled = errors.New("the project's cycles run on a schedule")

// ErrParallel is setting a schedule for a project whose cycles run side by
// side: the scheduler makes the next cycle only where cycles run one at a time.
var ErrParallel = errors.New("the project's cycles run side by side")

// SharedDaysError is turning parallel cycles off while two of the project's
// open cycles share days. It names them, the earlier first.
type SharedDaysError struct{ A, B Cycle }

func (e *SharedDaysError) Error() string {
	return ErrOverlap.Error() + ": " + e.A.Label() + " and " + e.B.Label()
}

// Unwrap makes a SharedDaysError ErrOverlap to errors.Is.
func (e *SharedDaysError) Unwrap() error { return ErrOverlap }

// oneAtATime is the condition, in a statement over project_cycles named by
// alias ("" for the table's own name), that the cycle's project runs its
// cycles one after another. Pure.
func oneAtATime(alias string) string {
	if alias == "" {
		alias = "project_cycles"
	}
	return `NOT EXISTS (SELECT 1 FROM project_settings ps WHERE ps.project_uuid = ` + alias + `.project_uuid AND ps.parallel_cycles)`
}

// Parallel is whether the project's cycles may share days.
func Parallel(ctx context.Context, project uuid.UUID) (bool, error) {
	return pgexec.Scalar[bool](ctx, `SELECT EXISTS (SELECT 1 FROM project_settings WHERE project_uuid = $1 AND parallel_cycles)`, project)
}

// SetParallel turns the project's parallel cycles on or off, under its
// cycles lock, so no cycle is made, started or restored meanwhile. On is
// ErrScheduled while a schedule runs its cycles; off is a *SharedDaysError
// while two of its open cycles (not completed) share days. A completed cycle
// is history: it may have run beside another.
func SetParallel(ctx context.Context, project uuid.UUID, on bool, by uuid.UUID) error {
	var setBy *uuid.UUID
	if by != uuid.Nil {
		setBy = &by
	}
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		if err := lockProject(ctx, project); err != nil {
			return err
		}
		if on {
			scheduled, err := pgexec.Scalar[bool](ctx, `SELECT `+cycleDomain.Scheduled("$1"), project)
			if err != nil {
				return err
			}
			if scheduled {
				return ErrScheduled
			}
		} else {
			pair, err := sharingDays(ctx, project)
			if err != nil {
				return err
			}
			if len(pair) == 2 {
				return &SharedDaysError{A: pair[0], B: pair[1]}
			}
		}
		_, err := pgexec.Exec(ctx, `INSERT INTO project_settings (project_uuid, parallel_cycles, set_by) VALUES ($1, $2, $3)
			ON CONFLICT (project_uuid) DO UPDATE SET parallel_cycles = EXCLUDED.parallel_cycles, set_by = EXCLUDED.set_by, updated_at = NOW()`,
			project, on, setBy)
		return err
	})
}

// sharingDays is the first two open cycles of the project that share days,
// the earlier first, or none.
func sharingDays(ctx context.Context, project uuid.UUID) ([]Cycle, error) {
	return pgexec.Query(ctx, `WITH pair AS (
		SELECT a.id AS x, b.id AS y FROM project_cycles a JOIN project_cycles b ON b.project_uuid = a.project_uuid AND b.id <> a.id
		WHERE a.project_uuid = $1 AND a.completed_at IS NULL AND b.completed_at IS NULL AND `+softDelete.Live("a")+` AND `+softDelete.Live("b")+`
			AND a.starts_at < b.ends_at AND b.starts_at < a.ends_at AND (a.starts_at, a.number) < (b.starts_at, b.number)
		ORDER BY a.starts_at, a.number, b.starts_at, b.number LIMIT 1)
		SELECT `+columns+` FROM project_cycles WHERE id IN (SELECT x FROM pair UNION ALL SELECT y FROM pair) ORDER BY starts_at, number`, scanRow, project)
}
