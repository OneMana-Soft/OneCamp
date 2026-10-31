package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Scale is a project's estimate scale (migration 933; business/EstimateScale
// has its sizes). A project with none estimates in hours.
type Scale struct {
	ProjectUUID uuid.UUID `json:"-"`
	// Kind is exponential, fibonacci, linear or tshirt.
	Kind string `json:"kind"`
	// Extended adds the scale's two larger sizes.
	Extended bool `json:"extended"`
	// AllowZero makes zero a size.
	AllowZero bool `json:"allow_zero"`
	// CountUnestimated counts a task with no points as one.
	CountUnestimated bool       `json:"count_unestimated"`
	SetBy            *uuid.UUID `json:"-"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

const columns = `project_uuid, kind, extended, allow_zero, count_unestimated, set_by, updated_at`

func scan(r pgexec.Row) (Scale, error) {
	var s Scale
	var by uuid.NullUUID
	err := r.Scan(&s.ProjectUUID, &s.Kind, &s.Extended, &s.AllowZero, &s.CountUnestimated, &by, &s.UpdatedAt)
	if by.Valid {
		s.SetBy = &by.UUID
	}
	return s, err
}

// Of is a project's scale, or nil when it estimates in hours.
func Of(ctx context.Context, project uuid.UUID) (*Scale, error) {
	scales, err := OfMany(ctx, []uuid.UUID{project})
	if err != nil {
		return nil, err
	}
	return scales[project], nil
}

// OfMany is the scales of projects, by project, in one read. A project that
// estimates in hours has none.
func OfMany(ctx context.Context, projects []uuid.UUID) (map[uuid.UUID]*Scale, error) {
	out := make(map[uuid.UUID]*Scale, len(projects))
	if len(projects) == 0 {
		return out, nil
	}
	scales, err := pgexec.Query(ctx, `SELECT `+columns+` FROM project_estimate_scales WHERE project_uuid = ANY($1::uuid[])`, scan, pq.Array(helpers.UUIDStrings(projects)))
	if err != nil {
		return nil, err
	}
	for i := range scales {
		out[scales[i].ProjectUUID] = &scales[i]
	}
	return out, nil
}

// Set writes a project's scale, replacing the one it had.
func Set(ctx context.Context, s Scale) (*Scale, error) {
	got, _, err := pgexec.QueryOne(ctx, `
		INSERT INTO project_estimate_scales (project_uuid, kind, extended, allow_zero, count_unestimated, set_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (project_uuid) DO UPDATE SET kind = EXCLUDED.kind, extended = EXCLUDED.extended,
			allow_zero = EXCLUDED.allow_zero, count_unestimated = EXCLUDED.count_unestimated,
			set_by = EXCLUDED.set_by, updated_at = NOW()
		RETURNING `+columns, scan, s.ProjectUUID, s.Kind, s.Extended, s.AllowZero, s.CountUnestimated, s.SetBy)
	if err != nil {
		return nil, err
	}
	return &got, nil
}

// Clear puts a project back on hours. Its tasks keep their points, unread
// until it counts in points again.
func Clear(ctx context.Context, project uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `DELETE FROM project_estimate_scales WHERE project_uuid = $1`, project)
	return err
}
