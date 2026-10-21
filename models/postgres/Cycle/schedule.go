package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Schedule is a project's cycle schedule (migration 473; business/Cycle's
// Scheduler): its cycles run themselves.
type Schedule struct {
	ProjectUUID   uuid.UUID  `json:"project_uuid"`
	Weeks         int        `json:"weeks"`
	CooldownWeeks int        `json:"cooldown_weeks"`
	Upcoming      int        `json:"upcoming"`
	RollOver      bool       `json:"roll_over"`
	Anchor        time.Time  `json:"starts_at"`
	SetBy         *uuid.UUID `json:"set_by,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func scanSchedule(r pgexec.Row) (Schedule, error) {
	var s Schedule
	var by uuid.NullUUID
	err := r.Scan(&s.ProjectUUID, &s.Weeks, &s.CooldownWeeks, &s.Upcoming, &s.RollOver, &s.Anchor, &by, &s.UpdatedAt)
	if by.Valid {
		s.SetBy = &by.UUID
	}
	return s, err
}

// ScheduleOf is a project's schedule, or nil when its cycles are run by hand.
func ScheduleOf(ctx context.Context, project uuid.UUID) (*Schedule, error) {
	s, found, err := pgexec.QueryOne(ctx, `
		SELECT project_uuid, weeks, cooldown_weeks, upcoming, roll_over, anchor, set_by, updated_at
		FROM cycle_schedules WHERE project_uuid = $1`, scanSchedule, project)
	if err != nil || !found {
		return nil, err
	}
	return &s, nil
}

// Schedules is every project's schedule, for the Sweeper's pass.
func Schedules(ctx context.Context) ([]Schedule, error) {
	return pgexec.Query(ctx, `
		SELECT project_uuid, weeks, cooldown_weeks, upcoming, roll_over, anchor, set_by, updated_at
		FROM cycle_schedules ORDER BY project_uuid`, scanSchedule)
}

// SetSchedule writes a project's schedule, replacing the one it had.
func SetSchedule(ctx context.Context, s Schedule) (*Schedule, error) {
	got, _, err := pgexec.QueryOne(ctx, `
		INSERT INTO cycle_schedules (project_uuid, weeks, cooldown_weeks, upcoming, roll_over, anchor, set_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (project_uuid) DO UPDATE SET weeks = EXCLUDED.weeks, cooldown_weeks = EXCLUDED.cooldown_weeks,
			upcoming = EXCLUDED.upcoming, roll_over = EXCLUDED.roll_over, anchor = EXCLUDED.anchor,
			set_by = EXCLUDED.set_by, updated_at = NOW()
		RETURNING project_uuid, weeks, cooldown_weeks, upcoming, roll_over, anchor, set_by, updated_at`,
		scanSchedule, s.ProjectUUID, s.Weeks, s.CooldownWeeks, s.Upcoming, s.RollOver, s.Anchor, s.SetBy)
	if err != nil {
		return nil, err
	}
	return &got, nil
}

// DropSchedule hands a project's cycles back to its people; false when it had
// no schedule. Its cycles stay as they are.
func DropSchedule(ctx context.Context, project uuid.UUID) (bool, error) {
	n, err := pgexec.Exec(ctx, `DELETE FROM cycle_schedules WHERE project_uuid = $1`, project)
	return n > 0, err
}

// ListMany is the cycles of each of the projects, in order, in one statement.
func ListMany(ctx context.Context, projects []uuid.UUID) (map[uuid.UUID][]Cycle, error) {
	out := make(map[uuid.UUID][]Cycle, len(projects))
	if len(projects) == 0 {
		return out, nil
	}
	rows, err := pgexec.Query(ctx, `SELECT `+columns+` FROM project_cycles WHERE project_uuid = ANY($1::uuid[]) ORDER BY project_uuid, number`,
		func(r pgexec.Row) (Cycle, error) {
			var c Cycle
			err := r.Scan(&c.Id, &c.ProjectUUID, &c.Number, &c.Name, &c.StartsAt, &c.EndsAt, &c.CompletedAt, &c.DoneCount, &c.CarriedCount)
			return c, err
		}, pq.Array(projects))
	if err != nil {
		return nil, err
	}
	for _, c := range rows {
		out[c.ProjectUUID] = append(out[c.ProjectUUID], c)
	}
	return out, nil
}

// overlapsAny is whether any of the windows shares days with a cycle the project has.
func overlapsAny(ctx context.Context, project uuid.UUID, starts, ends []time.Time) (bool, error) {
	return pgexec.Scalar[bool](ctx, `SELECT EXISTS (
		SELECT 1 FROM project_cycles c, unnest($2::timestamptz[], $3::timestamptz[]) AS w(s, e)
		WHERE c.project_uuid = $1 AND c.starts_at < w.e AND c.ends_at > w.s)`, project, pq.Array(starts), pq.Array(ends))
}

// Window is a cycle to make: when it starts and ends.
type Window struct {
	StartsAt time.Time
	EndsAt   time.Time
}

// CreateMany makes the cycles, numbered after the project's last, in one
// transaction under the project's lock (Create's): none when any of them
// would share days with a cycle the project has (ErrOverlap), as when
// someone made one by hand since the schedule was read.
func CreateMany(ctx context.Context, project uuid.UUID, windows []Window, by *uuid.UUID) ([]Cycle, error) {
	if len(windows) == 0 {
		return nil, nil
	}
	starts := make([]time.Time, len(windows))
	ends := make([]time.Time, len(windows))
	for i, w := range windows {
		starts[i], ends[i] = w.StartsAt, w.EndsAt
	}
	var made []Cycle
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if err := lockProject(ctx, project); err != nil {
			return err
		}
		clash, err := overlapsAny(ctx, project, starts, ends)
		if err != nil {
			return err
		}
		if clash {
			return ErrOverlap
		}
		made, err = pgexec.Query(ctx, `
			INSERT INTO project_cycles (project_uuid, number, name, starts_at, ends_at, created_by)
			SELECT $1, COALESCE((SELECT MAX(number) FROM project_cycles WHERE project_uuid = $1), 0) + w.n, '', w.s, w.e, $4
			FROM unnest($2::timestamptz[], $3::timestamptz[]) WITH ORDINALITY AS w(s, e, n)
			RETURNING `+columns,
			func(r pgexec.Row) (Cycle, error) {
				var c Cycle
				err := r.Scan(&c.Id, &c.ProjectUUID, &c.Number, &c.Name, &c.StartsAt, &c.EndsAt, &c.CompletedAt, &c.DoneCount, &c.CarriedCount)
				return c, err
			}, project, pq.Array(starts), pq.Array(ends), by)
		return err
	})
	if err != nil {
		return nil, err
	}
	return made, nil
}
