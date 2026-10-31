package models

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Milestone is a stage of a project's work (migration 935). It belongs to its
// home project (ProjectUUID), whose admins change it, and shows in Projects:
// its home first, then the projects it is shared with.
type Milestone struct {
	Id          uuid.UUID `json:"id"`
	ProjectUUID uuid.UUID `json:"project_uuid"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	// TargetDate is the day it should be done by ("2006-01-02"), or nil: a
	// day with no time and no zone, read as Postgres keeps it.
	TargetDate *string   `json:"target_date"`
	CreatedAt  time.Time `json:"created_at"`
	// Projects are every project it shows in, its home first. Never sent as
	// they are: a reader may not see them all.
	Projects []uuid.UUID `json:"-"`
}

// ErrFull is a project that shows as many milestones as it may, or a
// milestone shared with as many projects as it may.
var ErrFull = errors.New("milestones: full")

// ErrGone is a milestone deleted before a write to it could be made.
var ErrGone = errors.New("milestones: gone")

// hold keeps the milestone with id from being deleted until the transaction
// ends, or is ErrGone when it already is: tasks and projects are only
// written to a milestone that is there.
func hold(ctx context.Context, id uuid.UUID) error {
	_, ok, err := pgexec.QueryOne(ctx, `SELECT id FROM milestones WHERE id = $1 AND `+softDelete.Live("")+` FOR KEY SHARE`, func(r pgexec.Row) (uuid.UUID, error) {
		var got uuid.UUID
		return got, r.Scan(&got)
	}, id)
	if err == nil && !ok {
		err = ErrGone
	}
	return err
}

const columns = `m.id, m.project_uuid, m.name, m.description, to_char(m.target_date, 'YYYY-MM-DD'), m.created_at,
	ARRAY(SELECT mp.project_uuid::text FROM milestone_projects mp WHERE mp.milestone_id = m.id
	      ORDER BY mp.project_uuid <> m.project_uuid, mp.project_uuid)`

// scanAfter reads a milestone's columns, after the leading ones a query
// selects before them (lead).
func scanAfter(r pgexec.Row, lead ...any) (Milestone, error) {
	var m Milestone
	var projects pq.StringArray
	if err := r.Scan(append(lead, &m.Id, &m.ProjectUUID, &m.Name, &m.Description, &m.TargetDate, &m.CreatedAt, &projects)...); err != nil {
		return m, err
	}
	m.Projects = make([]uuid.UUID, 0, len(projects))
	for _, p := range projects {
		id, err := uuid.Parse(p)
		if err != nil {
			return m, err
		}
		m.Projects = append(m.Projects, id)
	}
	return m, nil
}

func scan(r pgexec.Row) (Milestone, error) { return scanAfter(r) }

// In is the milestones the projects show, each project's in its own order,
// by project.
func In(ctx context.Context, projects []uuid.UUID) (map[uuid.UUID][]Milestone, error) {
	out := make(map[uuid.UUID][]Milestone, len(projects))
	if len(projects) == 0 {
		return out, nil
	}
	type shown struct {
		project   uuid.UUID
		milestone Milestone
	}
	rows, err := pgexec.Query(ctx, `
		SELECT o.project_uuid, `+columns+` FROM milestone_projects o JOIN milestones m ON m.id = o.milestone_id AND `+softDelete.Live("m")+`
		 WHERE o.project_uuid = ANY($1::uuid[]) ORDER BY o.project_uuid, o.position, m.created_at, m.id`,
		func(r pgexec.Row) (shown, error) {
			var s shown
			m, err := scanAfter(r, &s.project)
			s.milestone = m
			return s, err
		}, pq.Array(projects))
	if err != nil {
		return nil, err
	}
	for _, s := range rows {
		out[s.project] = append(out[s.project], s.milestone)
	}
	return out, nil
}

// Get is one milestone, or nil.
func Get(ctx context.Context, id uuid.UUID) (*Milestone, error) {
	m, ok, err := pgexec.QueryOne(ctx, `SELECT `+columns+` FROM milestones m WHERE m.id = $1 AND `+softDelete.Live("m"), scan, id)
	if err != nil || !ok {
		return nil, err
	}
	return &m, nil
}

// Of is the milestone a task is in, or nil.
func Of(ctx context.Context, task uuid.UUID) (*Milestone, error) {
	m, ok, err := pgexec.QueryOne(ctx, `SELECT `+columns+` FROM milestones m JOIN task_milestones tm ON tm.milestone_id = m.id
		WHERE tm.task_uuid = $1 AND `+softDelete.Live("m"), scan, task)
	if err != nil || !ok {
		return nil, err
	}
	return &m, nil
}

// lock holds name for the transaction: a project's milestone list (its count
// and its order) or one milestone's projects are read and written under it.
func lock(ctx context.Context, name string) error {
	_, err := pgexec.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, name)
	return err
}

func projectLock(project uuid.UUID) string { return "milestones:" + project.String() }

// shownIn counts the milestones a project shows.
func shownIn(ctx context.Context, project uuid.UUID) (int, error) {
	return pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM milestone_projects mp JOIN milestones m ON m.id = mp.milestone_id AND `+softDelete.Live("m")+`
		WHERE mp.project_uuid = $1`, project)
}

// show puts the milestone last in the project's order.
func show(ctx context.Context, project, id uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO milestone_projects (project_uuid, milestone_id, position)
		VALUES ($1, $2, COALESCE((SELECT MAX(position) FROM milestone_projects WHERE project_uuid = $1), 0) + 1)`, project, id)
	return err
}

// Create makes a milestone in project, last in its order, unless it shows
// most milestones already (ErrFull).
func Create(ctx context.Context, project uuid.UUID, name, description string, target *string, by uuid.UUID, most int) (*Milestone, error) {
	var made *Milestone
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if err := lock(ctx, projectLock(project)); err != nil {
			return err
		}
		n, err := shownIn(ctx, project)
		if err != nil {
			return err
		}
		if n >= most {
			return ErrFull
		}
		id, err := pgexec.Scalar[uuid.UUID](ctx, `INSERT INTO milestones (project_uuid, name, description, target_date, created_by)
			VALUES ($1, $2, $3, $4::date, $5) RETURNING id`, project, name, description, target, by)
		if err != nil {
			return err
		}
		if err := show(ctx, project, id); err != nil {
			return err
		}
		made, err = Get(ctx, id)
		return err
	})
	return made, err
}

// Edit sets a milestone's name, description and target day (nil for none).
func Edit(ctx context.Context, id uuid.UUID, name, description string, target *string) error {
	_, err := pgexec.Exec(ctx, `UPDATE milestones SET name = $2, description = $3, target_date = $4::date WHERE id = $1 AND `+softDelete.Live(""),
		id, name, description, target)
	return err
}

// Delete deletes a milestone softly (V-78): it leaves every project it shows
// in, and its tasks leave it, until its restore brings it back to each, with
// its tasks.
func Delete(ctx context.Context, id, by uuid.UUID) error {
	_, err := softDelete.Delete(ctx, softDelete.Milestone, id, by, "")
	return err
}

// Reorder puts the project's milestones in the order of ids, first to last.
// One it shows that ids leave out (made in another tab meanwhile) keeps its
// order after them; an id it doesn't show is passed over.
func Reorder(ctx context.Context, project uuid.UUID, ids []uuid.UUID) error {
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		if err := lock(ctx, projectLock(project)); err != nil {
			return err
		}
		if _, err := pgexec.Exec(ctx, `UPDATE milestone_projects SET position = position + $3
			WHERE project_uuid = $1 AND NOT (milestone_id = ANY($2::uuid[]))`, project, pq.Array(ids), len(ids)); err != nil {
			return err
		}
		_, err := pgexec.Exec(ctx, `UPDATE milestone_projects mp SET position = o.n
			FROM unnest($2::uuid[]) WITH ORDINALITY AS o(id, n) WHERE mp.project_uuid = $1 AND mp.milestone_id = o.id`,
			project, pq.Array(ids))
		return err
	})
}

// Share shows the milestone in project too, last in its order, unless the
// project shows mostPerProject milestones already or the milestone shows in
// mostProjects projects (ErrFull). Sharing it again changes nothing.
func Share(ctx context.Context, id, project uuid.UUID, mostProjects, mostPerProject int) error {
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		if err := lock(ctx, projectLock(project)); err != nil {
			return err
		}
		if err := lock(ctx, "milestone:"+id.String()); err != nil {
			return err
		}
		if err := hold(ctx, id); err != nil {
			return err
		}
		already, err := pgexec.Scalar[bool](ctx, `SELECT EXISTS (SELECT 1 FROM milestone_projects WHERE project_uuid = $1 AND milestone_id = $2)`, project, id)
		if err != nil || already {
			return err
		}
		shown, err := shownIn(ctx, project)
		if err != nil {
			return err
		}
		in, err := pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM milestone_projects WHERE milestone_id = $1`, id)
		if err != nil {
			return err
		}
		if shown >= mostPerProject || in >= mostProjects {
			return ErrFull
		}
		return show(ctx, project, id)
	})
}

// Unshare takes the milestone off project, and that project's tasks out of
// it, in one transaction.
func Unshare(ctx context.Context, id, project uuid.UUID) error {
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `DELETE FROM task_milestones tm USING tasks t
			WHERE tm.milestone_id = $1 AND t.id = tm.task_uuid AND t.project_id = $2`, id, project); err != nil {
			return err
		}
		_, err := pgexec.Exec(ctx, `DELETE FROM milestone_projects WHERE milestone_id = $1 AND project_uuid = $2`, id, project)
		return err
	})
}

// Members maps each of the milestones to its tasks.
func Members(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]string, error) {
	out := make(map[uuid.UUID][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	type pair struct{ milestone, task uuid.UUID }
	rows, err := pgexec.Query(ctx, `SELECT milestone_id, task_uuid FROM task_milestones WHERE milestone_id = ANY($1::uuid[])`,
		func(r pgexec.Row) (pair, error) {
			var p pair
			err := r.Scan(&p.milestone, &p.task)
			return p, err
		}, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		out[p.milestone] = append(out[p.milestone], p.task.String())
	}
	return out, nil
}

// SetTasks puts tasks in a milestone, or takes them out of theirs (to nil),
// in one statement however many, and answers the ids it moved; ErrGone when
// the milestone was deleted first.
func SetTasks(ctx context.Context, tasks []uuid.UUID, to *uuid.UUID) ([]string, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.String()
	}
	if to == nil {
		return pgexec.Query(ctx, `DELETE FROM task_milestones WHERE task_uuid = ANY($1::uuid[]) RETURNING task_uuid::text`, pgexec.Text, pq.Array(ids))
	}
	var moved []string
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if err := hold(ctx, *to); err != nil {
			return err
		}
		var err error
		moved, err = pgexec.Query(ctx, `
			INSERT INTO task_milestones (task_uuid, milestone_id) SELECT unnest($1::uuid[]), $2
			ON CONFLICT (task_uuid) DO UPDATE SET milestone_id = EXCLUDED.milestone_id, added_at = NOW()
			 WHERE task_milestones.milestone_id IS DISTINCT FROM EXCLUDED.milestone_id
			RETURNING task_uuid::text`, pgexec.Text, pq.Array(ids), *to)
		return err
	})
	return moved, err
}
