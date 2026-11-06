// Package models (Dashboard) stores dashboards and their widgets (migration
// 571): a person's own (MineOf), a project's (ProjectID), or with neither the
// workspace's. Who may read or keep one, and what a widget's query may say,
// is business/Dashboard's; here a widget's query is JSON beside its place.
// Deleting either is soft, and either comes back.
package models

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ErrNotFound: no live dashboard or widget has that id (or, to restore, no
// deleted one).
var ErrNotFound = errors.New("no such dashboard")

// Dashboard is one kept dashboard.
type Dashboard struct {
	ID        uuid.UUID
	ProjectID *uuid.UUID // a project's
	MineOf    *uuid.UUID // a person's own
	Name      string
	Shared    bool
	CreatedBy uuid.UUID
	CreatedAt time.Time
	UpdatedBy *uuid.UUID
	UpdatedAt time.Time
	DeletedAt *time.Time
	DeletedBy *uuid.UUID
}

// Widget is one widget on a dashboard, at X, Y on a 12-column grid, W wide
// and H tall.
type Widget struct {
	ID          uuid.UUID
	DashboardID uuid.UUID
	Kind        string
	Title       string
	Query       []byte
	X, Y, W, H  int
	CreatedBy   uuid.UUID
	CreatedAt   time.Time
	UpdatedBy   *uuid.UUID
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// Place is where a widget goes on its dashboard.
type Place struct {
	ID         uuid.UUID
	X, Y, W, H int
}

const (
	columns       = `id, project_id, mine_of, name, shared, created_by, created_at, updated_by, updated_at, deleted_at, deleted_by`
	widgetColumns = `id, dashboard_id, kind, title, query, x, y, w, h, created_by, created_at, updated_by, updated_at, deleted_at`
)

func scan(r pgexec.Row) (Dashboard, error) {
	var d Dashboard
	return d, r.Scan(&d.ID, &d.ProjectID, &d.MineOf, &d.Name, &d.Shared, &d.CreatedBy, &d.CreatedAt,
		&d.UpdatedBy, &d.UpdatedAt, &d.DeletedAt, &d.DeletedBy)
}

func scanWidget(r pgexec.Row) (Widget, error) {
	var w Widget
	return w, r.Scan(&w.ID, &w.DashboardID, &w.Kind, &w.Title, &w.Query, &w.X, &w.Y, &w.W, &w.H,
		&w.CreatedBy, &w.CreatedAt, &w.UpdatedBy, &w.UpdatedAt, &w.DeletedAt)
}

// scopeKey names the lock a scope's cap is kept under: a person's own, a
// project's, or the workspace's.
func scopeKey(d *Dashboard) string {
	switch {
	case d.MineOf != nil:
		return "dashboards:mine:" + d.MineOf.String()
	case d.ProjectID != nil:
		return "dashboards:project:" + d.ProjectID.String()
	}
	return "dashboards:workspace"
}

// count is how many live dashboards d's scope keeps.
func count(ctx context.Context, d *Dashboard) (int, error) {
	return pgexec.Scalar[int](ctx, `SELECT count(*) FROM dashboards
		 WHERE deleted_at IS NULL AND project_id IS NOT DISTINCT FROM $1 AND mine_of IS NOT DISTINCT FROM $2`,
		d.ProjectID, d.MineOf)
}

// AddWithin keeps a new dashboard, and fills in its id and times, while its
// scope keeps fewer than limit live ones: the count and the insert hold the
// scope's lock (pgexec.Capped), so two at once can't both take the last
// place. added is false when the scope is full.
func AddWithin(ctx context.Context, d *Dashboard, limit int) (added bool, err error) {
	return pgexec.Capped(ctx, scopeKey(d), limit,
		func(ctx context.Context) (int, error) { return count(ctx, d) },
		func(ctx context.Context) error {
			d.ID = uuid.New()
			_, _, err := pgexec.QueryOne(ctx, `
				INSERT INTO dashboards (id, project_id, mine_of, name, shared, created_by)
				VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at, updated_at`,
				func(r pgexec.Row) (bool, error) { return true, r.Scan(&d.CreatedAt, &d.UpdatedAt) },
				d.ID, d.ProjectID, d.MineOf, d.Name, d.Shared, d.CreatedBy)
			return err
		})
}

// Get is a dashboard, live or deleted; nil when there's none.
func Get(ctx context.Context, id uuid.UUID) (*Dashboard, error) {
	d, found, err := pgexec.QueryOne(ctx, `SELECT `+columns+` FROM dashboards WHERE id = $1`, scan, id)
	if err != nil || !found {
		return nil, err
	}
	return &d, nil
}

func list(ctx context.Context, query string, args ...any) ([]Dashboard, error) {
	out, err := pgexec.Query(ctx, query, scan, args...)
	if out == nil && err == nil {
		out = []Dashboard{}
	}
	return out, err
}

// ListProject is a project's live dashboards by name.
func ListProject(ctx context.Context, project uuid.UUID) ([]Dashboard, error) {
	return list(ctx, `SELECT `+columns+` FROM dashboards
		 WHERE deleted_at IS NULL AND project_id = $1 ORDER BY lower(name), created_at`, project)
}

// ListMine is a person's own live dashboards by name.
func ListMine(ctx context.Context, person uuid.UUID) ([]Dashboard, error) {
	return list(ctx, `SELECT `+columns+` FROM dashboards
		 WHERE deleted_at IS NULL AND mine_of = $1 ORDER BY lower(name), created_at`, person)
}

// ListWorkspace is the workspace's live dashboards by name.
func ListWorkspace(ctx context.Context) ([]Dashboard, error) {
	return list(ctx, `SELECT `+columns+` FROM dashboards
		 WHERE deleted_at IS NULL AND project_id IS NULL AND mine_of IS NULL ORDER BY lower(name), created_at`)
}

// Update changes a live dashboard's name and whether it is shared, as by.
func Update(ctx context.Context, d *Dashboard, by uuid.UUID) error {
	_, found, err := pgexec.QueryOne(ctx, `
		UPDATE dashboards SET name = $2, shared = $3, updated_by = $4, updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL RETURNING updated_at`,
		func(r pgexec.Row) (bool, error) { return true, r.Scan(&d.UpdatedAt) }, d.ID, d.Name, d.Shared, by)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	d.UpdatedBy = &by
	return nil
}

// Delete deletes a live dashboard softly, as by. Its widgets stay as they
// are, so restoring it brings them back.
func Delete(ctx context.Context, id, by uuid.UUID) error {
	return changeOne(ctx, `UPDATE dashboards SET deleted_at = NOW(), deleted_by = $2
		 WHERE id = $1 AND deleted_at IS NULL`, id, by)
}

// RestoreWithin brings a deleted dashboard back while its scope keeps fewer
// than limit live ones, under the same lock AddWithin takes. restored is
// false when the scope is full.
func RestoreWithin(ctx context.Context, d *Dashboard, limit int) (restored bool, err error) {
	return pgexec.Capped(ctx, scopeKey(d), limit,
		func(ctx context.Context) (int, error) { return count(ctx, d) },
		func(ctx context.Context) error {
			return changeOne(ctx, `UPDATE dashboards SET deleted_at = NULL, deleted_by = NULL
				 WHERE id = $1 AND deleted_at IS NOT NULL`, d.ID)
		})
}

// Widgets is a dashboard's live widgets in reading order: by row, then column.
func Widgets(ctx context.Context, dashboard uuid.UUID) ([]Widget, error) {
	out, err := pgexec.Query(ctx, `SELECT `+widgetColumns+` FROM dashboard_widgets
		 WHERE dashboard_id = $1 AND deleted_at IS NULL ORDER BY y, x, created_at`, scanWidget, dashboard)
	if out == nil && err == nil {
		out = []Widget{}
	}
	return out, err
}

// countWidgets is how many live widgets a dashboard has.
func countWidgets(ctx context.Context, dashboard uuid.UUID) (int, error) {
	return pgexec.Scalar[int](ctx, `SELECT count(*) FROM dashboard_widgets
		 WHERE dashboard_id = $1 AND deleted_at IS NULL`, dashboard)
}

func widgetKey(dashboard uuid.UUID) string { return "dashboard_widgets:" + dashboard.String() }

// AddWidgetWithin keeps a new widget, and fills in its id and times, while
// its dashboard has fewer than limit live ones (under the dashboard's lock).
func AddWidgetWithin(ctx context.Context, w *Widget, limit int) (added bool, err error) {
	return pgexec.Capped(ctx, widgetKey(w.DashboardID), limit,
		func(ctx context.Context) (int, error) { return countWidgets(ctx, w.DashboardID) },
		func(ctx context.Context) error {
			w.ID = uuid.New()
			_, _, err := pgexec.QueryOne(ctx, `
				INSERT INTO dashboard_widgets (id, dashboard_id, kind, title, query, x, y, w, h, created_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING created_at, updated_at`,
				func(r pgexec.Row) (bool, error) { return true, r.Scan(&w.CreatedAt, &w.UpdatedAt) },
				w.ID, w.DashboardID, w.Kind, w.Title, w.Query, w.X, w.Y, w.W, w.H, w.CreatedBy)
			return err
		})
}

// UpdateWidget changes a live widget's kind, title, query and place, as by.
func UpdateWidget(ctx context.Context, w *Widget, by uuid.UUID) error {
	_, found, err := pgexec.QueryOne(ctx, `
		UPDATE dashboard_widgets SET kind = $3, title = $4, query = $5, x = $6, y = $7, w = $8, h = $9,
		       updated_by = $10, updated_at = NOW()
		 WHERE id = $1 AND dashboard_id = $2 AND deleted_at IS NULL RETURNING updated_at`,
		func(r pgexec.Row) (bool, error) { return true, r.Scan(&w.UpdatedAt) },
		w.ID, w.DashboardID, w.Kind, w.Title, w.Query, w.X, w.Y, w.W, w.H, by)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	w.UpdatedBy = &by
	return nil
}

// Arrange moves several of a dashboard's live widgets at once, in one
// statement. Every one must be there: none moves otherwise.
func Arrange(ctx context.Context, dashboard uuid.UUID, places []Place, by uuid.UUID) error {
	if len(places) == 0 {
		return nil
	}
	ids, xs, ys, ws, hs := make([]string, len(places)), make([]int64, len(places)), make([]int64, len(places)), make([]int64, len(places)), make([]int64, len(places))
	for i, p := range places {
		ids[i], xs[i], ys[i], ws[i], hs[i] = p.ID.String(), int64(p.X), int64(p.Y), int64(p.W), int64(p.H)
	}
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		n, err := pgexec.Exec(ctx, `
			UPDATE dashboard_widgets d SET x = p.x, y = p.y, w = p.w, h = p.h, updated_by = $2, updated_at = NOW()
			  FROM unnest($3::uuid[], $4::int[], $5::int[], $6::int[], $7::int[]) AS p(id, x, y, w, h)
			 WHERE d.id = p.id AND d.dashboard_id = $1 AND d.deleted_at IS NULL`,
			dashboard, by, pq.Array(ids), pq.Array(xs), pq.Array(ys), pq.Array(ws), pq.Array(hs))
		if err != nil {
			return err
		}
		if n != int64(len(places)) {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteWidget deletes a live widget softly, as by.
func DeleteWidget(ctx context.Context, dashboard, id, by uuid.UUID) error {
	return changeOne(ctx, `UPDATE dashboard_widgets SET deleted_at = NOW(), deleted_by = $3
		 WHERE id = $1 AND dashboard_id = $2 AND deleted_at IS NULL`, id, dashboard, by)
}

// RestoreWidgetWithin brings a deleted widget back while its dashboard has
// fewer than limit live ones.
func RestoreWidgetWithin(ctx context.Context, dashboard, id uuid.UUID, limit int) (restored bool, err error) {
	return pgexec.Capped(ctx, widgetKey(dashboard), limit,
		func(ctx context.Context) (int, error) { return countWidgets(ctx, dashboard) },
		func(ctx context.Context) error {
			return changeOne(ctx, `UPDATE dashboard_widgets SET deleted_at = NULL, deleted_by = NULL
				 WHERE id = $1 AND dashboard_id = $2 AND deleted_at IS NOT NULL`, id, dashboard)
		})
}

func changeOne(ctx context.Context, query string, args ...any) error {
	n, err := pgexec.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
