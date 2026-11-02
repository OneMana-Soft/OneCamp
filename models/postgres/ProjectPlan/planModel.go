// Package model keeps a project's plan (linear-15, migration 548): who leads
// it, when it starts and should land (each at a resolution), its priority,
// its status and its labels, and the workspace's own project statuses and
// labels beside the built-in ones. Deleting a status or a label is soft
// (V-78): softDelete.ProjectStatus and softDelete.ProjectLabel.
package model

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

var (
	// ErrDates is a save whose start would fall after its target.
	ErrDates = errors.New("the start would come after the target")
	// ErrDuplicate is a status or label whose name a live one has.
	ErrDuplicate = errors.New("a live one has that name")
	// ErrNoProject is a save for a project this server keeps no row for.
	ErrNoProject = errors.New("no such project")
)

// Plan is a project's plan as stored. A project nobody has planned reads as
// None: in progress, with nothing else set.
type Plan struct {
	ProjectID        uuid.UUID
	LeadID           *uuid.UUID
	StartOn          *time.Time
	StartResolution  string
	TargetOn         *time.Time
	TargetResolution string
	Priority         string
	StatusCategory   string
	// StatusID is a live workspace status in StatusCategory, or nil: the
	// category's built-in (a deleted status reads as nil).
	StatusID *uuid.UUID
	// Labels are the project's live labels, by name.
	Labels    []uuid.UUID
	UpdatedAt *time.Time
}

// None is the plan of a project nobody has planned.
func None(project uuid.UUID) Plan {
	return Plan{ProjectID: project, StatusCategory: "in_progress", Labels: []uuid.UUID{}}
}

// Patch names the parts of a plan a save writes; the others keep what they
// hold, whoever saved them a moment ago.
type Patch struct {
	Lead, Start, Target, Priority, Status, Labels bool
}

const planColumns = `p.project_id, p.lead_id, p.start_on, COALESCE(p.start_resolution, ''), p.target_on,
	COALESCE(p.target_resolution, ''), COALESCE(p.priority, ''), p.status_category, s.id, p.updated_at,
	COALESCE((SELECT array_agg(l.id::text ORDER BY lower(l.name), l.id) FROM project_plan_labels pl
		JOIN project_labels l ON l.id = pl.label_id AND l.deleted_at IS NULL WHERE pl.project_id = p.project_id), '{}')`

const planFrom = ` FROM project_plans p LEFT JOIN project_statuses s ON s.id = p.status_id AND s.deleted_at IS NULL`

func scanPlan(r pgexec.Row) (Plan, error) {
	var p Plan
	var lead, status uuid.NullUUID
	var start, target sql.NullTime
	var updated time.Time
	var labels pq.StringArray
	if err := r.Scan(&p.ProjectID, &lead, &start, &p.StartResolution, &target, &p.TargetResolution, &p.Priority,
		&p.StatusCategory, &status, &updated, &labels); err != nil {
		return p, err
	}
	p.LeadID, p.StatusID = idOf(lead), idOf(status)
	if start.Valid {
		p.StartOn = &start.Time
	}
	if target.Valid {
		p.TargetOn = &target.Time
	}
	p.UpdatedAt = &updated
	p.Labels = make([]uuid.UUID, 0, len(labels))
	for _, s := range labels {
		if id, err := uuid.Parse(s); err == nil {
			p.Labels = append(p.Labels, id)
		}
	}
	return p, nil
}

func idOf(n uuid.NullUUID) *uuid.UUID {
	if !n.Valid {
		return nil
	}
	id := n.UUID
	return &id
}

func nullOf(id *uuid.UUID) uuid.NullUUID {
	if id == nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: *id, Valid: true}
}

func dayOf(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format("2006-01-02")
}

// Plans is the plan of each of ids, in one read; a project nobody has
// planned reads as None.
func Plans(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Plan, error) {
	out := make(map[uuid.UUID]Plan, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := pgexec.Query(ctx, `SELECT `+planColumns+planFrom+` WHERE p.project_id = ANY($1)`, scanPlan, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = None(id)
	}
	for _, p := range rows {
		out[p.ProjectID] = p
	}
	return out, nil
}

// Get is project's plan.
func Get(ctx context.Context, project uuid.UUID) (Plan, error) {
	m, err := Plans(ctx, []uuid.UUID{project})
	if err != nil {
		return Plan{}, err
	}
	return m[project], nil
}

// NewLabel is a label a save makes as it gives it to the project; one a live
// label already has the name of (whatever its case) is that label.
type NewLabel struct {
	Name, Color string
}

// Save writes the parts of p that patch names, by by, in one transaction
// with the project's labels when it names them: p.Labels, then each of
// fresh, made when no live label has its name. Labels the project carries
// that were deleted stay with it, for their restore. It answers the plan as
// it now is. ErrDates: the start would fall after the target, with the dates
// another save left. most is how many live labels the workspace may hold.
func Save(ctx context.Context, p Plan, patch Patch, fresh []NewLabel, most int, by uuid.UUID) (Plan, error) {
	var saved Plan
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		sets := []string{"updated_by = EXCLUDED.updated_by", "updated_at = NOW()"}
		add := func(on bool, cols ...string) {
			for _, c := range cols {
				if on {
					sets = append(sets, c+" = EXCLUDED."+c)
				}
			}
		}
		add(patch.Lead, "lead_id")
		add(patch.Start, "start_on", "start_resolution")
		add(patch.Target, "target_on", "target_resolution")
		add(patch.Priority, "priority")
		add(patch.Status, "status_category", "status_id")
		if _, err := pgexec.Exec(ctx, `INSERT INTO project_plans (project_id, lead_id, start_on, start_resolution, target_on,
				target_resolution, priority, status_category, status_id, updated_by)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5, NULLIF($6, ''), NULLIF($7, ''), $8, $9, $10)
			ON CONFLICT (project_id) DO UPDATE SET `+strings.Join(sets, ", "),
			p.ProjectID, nullOf(p.LeadID), dayOf(p.StartOn), p.StartResolution, dayOf(p.TargetOn), p.TargetResolution,
			p.Priority, p.StatusCategory, nullOf(p.StatusID), uuid.NullUUID{UUID: by, Valid: by != uuid.Nil}); err != nil {
			return err
		}
		if patch.Labels {
			ids := append([]uuid.UUID{}, p.Labels...)
			for _, l := range fresh {
				id, err := labelNamed(ctx, l, most, by)
				if err != nil {
					return err
				}
				ids = append(ids, id)
			}
			if _, err := pgexec.Exec(ctx, `DELETE FROM project_plan_labels WHERE project_id = $1 AND NOT (label_id = ANY($2))
				AND label_id IN (SELECT id FROM project_labels WHERE deleted_at IS NULL)`, p.ProjectID, pq.Array(ids)); err != nil {
				return err
			}
			if _, err := pgexec.Exec(ctx, `INSERT INTO project_plan_labels (project_id, label_id, added_by)
				SELECT $1, id, $3 FROM project_labels WHERE id = ANY($2) AND deleted_at IS NULL ON CONFLICT DO NOTHING`,
				p.ProjectID, pq.Array(ids), uuid.NullUUID{UUID: by, Valid: by != uuid.Nil}); err != nil {
				return err
			}
		}
		var err error
		saved, _, err = pgexec.QueryOne(ctx, `SELECT `+planColumns+planFrom+` WHERE p.project_id = $1`, scanPlan, p.ProjectID)
		return err
	})
	var pe *pq.Error
	switch {
	case errors.As(err, &pe) && pe.Code == "23514":
		return Plan{}, ErrDates
	case errors.As(err, &pe) && pe.Code == "23503" && strings.Contains(pe.Constraint, "project_id"):
		return Plan{}, ErrNoProject
	}
	return saved, err
}

// ErrFull is a new or restored status or label past the workspace's most.
var ErrFull = errors.New("the workspace has as many as it may")

// LabelBound is the workspace's most labels: every new label (a plan
// change's or the settings') and every deleted label's restore counts under
// its one lock, so two can't both take the workspace's last place (ErrFull).
func LabelBound(most int) softDelete.Bound {
	return softDelete.Bound{Lock: "project_labels", Most: most, Full: ErrFull, Count: func(ctx context.Context) (int, error) {
		return pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM project_labels WHERE `+softDelete.Live(""))
	}}
}

// StatusBound is the workspace's most statuses of its own, which its new
// statuses and its deleted statuses' restores count under alike (ErrFull).
func StatusBound(most int) softDelete.Bound {
	return softDelete.Bound{Lock: "project_statuses", Most: most, Full: ErrFull, Count: func(ctx context.Context) (int, error) {
		return pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM project_statuses WHERE `+softDelete.Live(""))
	}}
}

// labelNamed is the live label named l.Name (whatever its case), made when
// there is none, under the workspace's most (ErrFull).
func labelNamed(ctx context.Context, l NewLabel, most int, by uuid.UUID) (uuid.UUID, error) {
	find := func() (uuid.UUID, bool, error) {
		return pgexec.QueryOne(ctx, `SELECT id FROM project_labels WHERE lower(name) = lower($1) AND `+softDelete.Live(""),
			func(r pgexec.Row) (id uuid.UUID, err error) { err = r.Scan(&id); return }, l.Name)
	}
	if id, ok, err := find(); err != nil || ok {
		return id, err
	}
	b := LabelBound(most)
	added, err := pgexec.Capped(ctx, b.Lock, b.Most, b.Count, func(ctx context.Context) error {
		// Another save may make the same name a moment before: its label is this one.
		_, err := pgexec.Exec(ctx, `INSERT INTO project_labels (name, color, created_by) VALUES ($1, $2, $3)
			ON CONFLICT (lower(name)) WHERE deleted_at IS NULL DO NOTHING`, l.Name, l.Color, uuid.NullUUID{UUID: by, Valid: by != uuid.Nil})
		return err
	})
	if err != nil {
		return uuid.Nil, err
	}
	id, ok, err := find()
	switch {
	case err != nil:
		return uuid.Nil, err
	case !ok && !added:
		return uuid.Nil, ErrFull
	case !ok:
		return uuid.Nil, errors.New("project_labels: a label made a moment ago reads as gone")
	}
	return id, nil
}

// CreateLabel makes a label, under the workspace's most (ErrFull).
// ErrDuplicate: a live label has its name, whatever its case.
func CreateLabel(ctx context.Context, l NewLabel, most int, by uuid.UUID) (*Label, error) {
	var made Label
	b := LabelBound(most)
	added, err := pgexec.Capped(ctx, b.Lock, b.Most, b.Count, func(ctx context.Context) error {
		var err error
		made, _, err = pgexec.QueryOne(ctx, `INSERT INTO project_labels (name, color, created_by) VALUES ($1, $2, $3)
			RETURNING id, name, color`, scanLabel, l.Name, l.Color, uuid.NullUUID{UUID: by, Valid: by != uuid.Nil})
		return err
	})
	switch {
	case helpers.IsUniqueViolation(err):
		return nil, ErrDuplicate
	case err != nil:
		return nil, err
	case !added:
		return nil, ErrFull
	}
	return &made, nil
}

// Status is one of the workspace's own project statuses.
type Status struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Category string    `json:"category"`
	Color    string    `json:"color"`
	Position int       `json:"position"`
}

func scanStatus(r pgexec.Row) (s Status, err error) {
	err = r.Scan(&s.ID, &s.Name, &s.Category, &s.Color, &s.Position)
	return
}

// Statuses is the workspace's live project statuses, in their order.
func Statuses(ctx context.Context) ([]Status, error) {
	return pgexec.Query(ctx, `SELECT id, name, category, color, position FROM project_statuses WHERE `+softDelete.Live("")+`
		ORDER BY position, created_at, id`, scanStatus)
}

// GetStatus is the live status with id, or nil.
func GetStatus(ctx context.Context, id uuid.UUID) (*Status, error) {
	s, ok, err := pgexec.QueryOne(ctx, `SELECT id, name, category, color, position FROM project_statuses WHERE id = $1 AND `+softDelete.Live(""), scanStatus, id)
	if err != nil || !ok {
		return nil, err
	}
	return &s, nil
}

// CreateStatus makes a status after the others, under the workspace's most
// (StatusBound).
func CreateStatus(ctx context.Context, s Status, most int, by uuid.UUID) (*Status, error) {
	var made Status
	b := StatusBound(most)
	added, err := pgexec.Capped(ctx, b.Lock, b.Most, b.Count,
		func(ctx context.Context) error {
			var err error
			made, _, err = pgexec.QueryOne(ctx, `INSERT INTO project_statuses (name, category, color, created_by, position)
				VALUES ($1, $2, $3, $4, COALESCE((SELECT MAX(position) FROM project_statuses WHERE `+softDelete.Live("")+`), 0) + 1)
				RETURNING id, name, category, color, position`, scanStatus, s.Name, s.Category, s.Color, uuid.NullUUID{UUID: by, Valid: by != uuid.Nil})
			return err
		})
	switch {
	case helpers.IsUniqueViolation(err):
		return nil, ErrDuplicate
	case err != nil:
		return nil, err
	case !added:
		return nil, ErrFull
	}
	return &made, nil
}

// UpdateStatus renames, recolours or moves the live status with id; a nil
// part keeps what it holds. It answers whether one was live.
func UpdateStatus(ctx context.Context, id uuid.UUID, name, color *string, position *int) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE project_statuses SET name = COALESCE($2, name), color = COALESCE($3, color),
		position = COALESCE($4, position), updated_at = NOW() WHERE id = $1 AND `+softDelete.Live(""), id, name, color, position)
	if helpers.IsUniqueViolation(err) {
		return false, ErrDuplicate
	}
	return n > 0, err
}

// DeleteStatus deletes the live status with id, by by: its projects read as
// its category's built-in until a restore.
func DeleteStatus(ctx context.Context, id, by uuid.UUID) (bool, error) {
	return softDelete.Delete(ctx, softDelete.ProjectStatus, id, by, "")
}

// Label is one of the workspace's project labels.
type Label struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Color string    `json:"color"`
}

func scanLabel(r pgexec.Row) (l Label, err error) {
	err = r.Scan(&l.ID, &l.Name, &l.Color)
	return
}

// Labels is the workspace's live project labels, by name.
func Labels(ctx context.Context) ([]Label, error) {
	return pgexec.Query(ctx, `SELECT id, name, color FROM project_labels WHERE `+softDelete.Live("")+` ORDER BY lower(name), id`, scanLabel)
}

// UpdateLabel renames or recolours the live label with id; a nil part keeps
// what it holds. It answers whether one was live.
func UpdateLabel(ctx context.Context, id uuid.UUID, name, color *string) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE project_labels SET name = COALESCE($2, name), color = COALESCE($3, color), updated_at = NOW()
		WHERE id = $1 AND `+softDelete.Live(""), id, name, color)
	if helpers.IsUniqueViolation(err) {
		return false, ErrDuplicate
	}
	return n > 0, err
}

// DeleteLabel deletes the live label with id, by by: it leaves every
// project's labels until a restore.
func DeleteLabel(ctx context.Context, id, by uuid.UUID) (bool, error) {
	return softDelete.Delete(ctx, softDelete.ProjectLabel, id, by, "")
}

// RestoreLabel brings the deleted label with id back: to the workspace,
// while it has fewer than mostLabels live labels (LabelBound; past it,
// ErrFull and nothing changes), and to each project that carried it (Save
// keeps a deleted label's rows for this), except a project that has taken
// most live labels since, which it leaves, so no project carries more than
// most. The projects' plan rows are locked first, as every Save locks its
// own, so a save giving one of them its last label waits for the restore, or
// the restore for it. It answers whether a label was deleted. ErrTaken
// (softDelete): a live label has its name now, and nothing changes.
func RestoreLabel(ctx context.Context, id uuid.UUID, mostLabels, most int) (bool, error) {
	restored := false
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `SELECT 1 FROM project_plans WHERE project_id IN
				(SELECT project_id FROM project_plan_labels WHERE label_id = $1) ORDER BY project_id FOR UPDATE`, id); err != nil {
			return err
		}
		var err error
		if restored, err = softDelete.Restore(ctx, softDelete.ProjectLabel, id, LabelBound(mostLabels)); err != nil || !restored {
			return err
		}
		// Live again, it counts: a project it takes past most has no room.
		_, err = pgexec.Exec(ctx, `DELETE FROM project_plan_labels pl WHERE pl.label_id = $1 AND (SELECT COUNT(*)
				FROM project_plan_labels x JOIN project_labels l ON l.id = x.label_id
				WHERE x.project_id = pl.project_id AND `+softDelete.Live("l")+`) > $2`, id, most)
		return err
	})
	return restored, err
}

// Names is each of people's names as the workspace shows them (their display
// name, else their username), in one read.
func Names(ctx context.Context, people []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(people) == 0 {
		return map[uuid.UUID]string{}, nil
	}
	return pgexec.Pairs[uuid.UUID, string](ctx, `SELECT id, COALESCE(NULLIF(display_name, ''), username, '') FROM users WHERE id = ANY($1)`, pq.Array(people))
}
