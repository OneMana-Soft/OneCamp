package business

import (
	"context"
	"encoding/json"
	"errors"

	model "github.com/akashc777/OneCamp/models/postgres/Dashboard"
	"github.com/google/uuid"
)

func dashboardOf(m model.Dashboard) Dashboard {
	d := Dashboard{ID: m.ID.String(), Scope: ScopeWorkspace, Name: m.Name, Shared: m.Shared,
		CreatedBy: m.CreatedBy.String(), CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
	switch {
	case m.MineOf != nil:
		d.Scope, d.MineOf = ScopeMine, *m.MineOf
	case m.ProjectID != nil:
		d.Scope, d.ProjectID = ScopeProject, m.ProjectID.String()
	}
	return d
}

func dashboards(rows []model.Dashboard, err error) ([]Dashboard, error) {
	if err != nil {
		return nil, err
	}
	out := make([]Dashboard, 0, len(rows))
	for _, r := range rows {
		out = append(out, dashboardOf(r))
	}
	return out, nil
}

func widgetOf(m model.Widget) Widget {
	w := Widget{ID: m.ID.String(), Kind: m.Kind, Title: m.Title, X: m.X, Y: m.Y, W: m.W, H: m.H}
	// A query is kept only as CheckWidget made it, so it always reads back.
	_ = json.Unmarshal(m.Query, &w.Query)
	return w
}

func notFound(err error) error {
	if errors.Is(err, model.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// Get is a dashboard, and whether it is deleted.
func Get(ctx context.Context, id string) (Dashboard, bool, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return Dashboard{}, false, ErrNotFound
	}
	m, err := model.Get(ctx, u)
	if err != nil {
		return Dashboard{}, false, err
	}
	if m == nil {
		return Dashboard{}, false, ErrNotFound
	}
	return dashboardOf(*m), m.DeletedAt != nil, nil
}

// ListProject is a project's live dashboards, ListMine a person's own and
// ListWorkspace the workspace's: callers keep those MayRead allows.
func ListProject(ctx context.Context, project uuid.UUID) ([]Dashboard, error) {
	return dashboards(model.ListProject(ctx, project))
}

// ListMine is a person's own live dashboards.
func ListMine(ctx context.Context, person uuid.UUID) ([]Dashboard, error) {
	return dashboards(model.ListMine(ctx, person))
}

// ListWorkspace is the workspace's live dashboards.
func ListWorkspace(ctx context.Context) ([]Dashboard, error) {
	return dashboards(model.ListWorkspace(ctx))
}

// Add keeps a new dashboard in d's scope (its ProjectID or MineOf), as by.
func Add(ctx context.Context, d Dashboard, by uuid.UUID) (Dashboard, error) {
	name, err := CheckName(d.Name)
	if err != nil {
		return d, err
	}
	if err := CheckShared(d, d.Shared); err != nil {
		return d, err
	}
	m := model.Dashboard{Name: name, Shared: d.Shared, CreatedBy: by}
	switch d.Scope {
	case ScopeMine:
		m.MineOf = &d.MineOf
	case ScopeProject:
		p, err := uuid.Parse(d.ProjectID)
		if err != nil {
			return d, ErrNotFound
		}
		m.ProjectID = &p
	}
	added, err := model.AddWithin(ctx, &m, MaxDashboards)
	if err != nil {
		return d, err
	}
	if !added {
		return d, ErrFull
	}
	return dashboardOf(m), nil
}

// Change renames d and shares it or not, as by.
func Change(ctx context.Context, d Dashboard, name string, shared bool, by uuid.UUID) (Dashboard, error) {
	name, err := CheckName(name)
	if err != nil {
		return d, err
	}
	if err := CheckShared(d, shared); err != nil {
		return d, err
	}
	m, err := rowOf(d)
	if err != nil {
		return d, err
	}
	m.Name, m.Shared = name, shared
	if err := model.Update(ctx, &m, by); err != nil {
		return d, notFound(err)
	}
	d.Name, d.Shared, d.UpdatedAt = name, shared, m.UpdatedAt
	return d, nil
}

// rowOf is d as the store keeps it, for its scope's lock and its id.
func rowOf(d Dashboard) (model.Dashboard, error) {
	id, err := uuid.Parse(d.ID)
	if err != nil {
		return model.Dashboard{}, ErrNotFound
	}
	m := model.Dashboard{ID: id, Name: d.Name, Shared: d.Shared}
	switch d.Scope {
	case ScopeMine:
		owner := d.MineOf
		m.MineOf = &owner
	case ScopeProject:
		p, err := uuid.Parse(d.ProjectID)
		if err != nil {
			return m, ErrNotFound
		}
		m.ProjectID = &p
	}
	return m, nil
}

// Delete deletes d softly, as by; its widgets come back with it.
func Delete(ctx context.Context, d Dashboard, by uuid.UUID) error {
	m, err := rowOf(d)
	if err != nil {
		return err
	}
	return notFound(model.Delete(ctx, m.ID, by))
}

// Restore brings a deleted d back, while its scope has room.
func Restore(ctx context.Context, d Dashboard) error {
	m, err := rowOf(d)
	if err != nil {
		return err
	}
	restored, err := model.RestoreWithin(ctx, &m, MaxDashboards)
	if err != nil {
		return notFound(err)
	}
	if !restored {
		return ErrFull
	}
	return nil
}

// Widgets is d's live widgets in reading order.
func Widgets(ctx context.Context, d Dashboard) ([]Widget, error) {
	id, err := uuid.Parse(d.ID)
	if err != nil {
		return nil, ErrNotFound
	}
	rows, err := model.Widgets(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]Widget, 0, len(rows))
	for _, r := range rows {
		out = append(out, widgetOf(r))
	}
	return out, nil
}

// AddWidget puts w on d, as by, while d has room: over a saved view, one of
// by's own that fits d (checkView). It is a batch of one (AddWidgets).
func AddWidget(ctx context.Context, d Dashboard, w Widget, by uuid.UUID) (Widget, error) {
	made, err := AddWidgets(ctx, d, []Widget{w}, by)
	if err != nil {
		return w, err
	}
	return made[0], nil
}

// AddWidgets puts ws on d at once, as by, while d has room for every one of
// them: all go on, or none (the first that fails its check, a view that
// isn't by's own to put on d, or ErrFull). Their views are read in one go.
func AddWidgets(ctx context.Context, d Dashboard, ws []Widget, by uuid.UUID) ([]Widget, error) {
	id, err := uuid.Parse(d.ID)
	if err != nil {
		return nil, ErrNotFound
	}
	out := make([]Widget, len(ws))
	rows := make([]model.Widget, len(ws))
	for i, w := range ws {
		if out[i], err = CheckWidget(d, w); err != nil {
			return nil, err
		}
		if rows[i], err = widgetRow(d, out[i]); err != nil {
			return nil, err
		}
	}
	if err := checkViews(ctx, d, out, by); err != nil {
		return nil, err
	}
	added, err := model.AddWidgetsWithin(ctx, id, rows, by, MaxWidgets)
	if err != nil {
		return nil, err
	}
	if !added {
		return nil, ErrFull
	}
	for i := range out {
		out[i].ID = rows[i].ID.String()
	}
	return out, nil
}

// ChangeWidget changes d's widget w.ID to w, as by. A view it newly reads is
// one of by's own that fits d (checkView); the view it read already stays,
// so any keeper may retitle or move a widget over another keeper's view.
func ChangeWidget(ctx context.Context, d Dashboard, w Widget, by uuid.UUID) (Widget, error) {
	w, err := CheckWidget(d, w)
	if err != nil {
		return w, err
	}
	if w.Query.View != "" {
		if err := checkChangedView(ctx, d, w, by); err != nil {
			return w, err
		}
	}
	m, err := widgetRow(d, w)
	if err != nil {
		return w, err
	}
	if m.ID, err = uuid.Parse(w.ID); err != nil {
		return w, ErrNotFound
	}
	return w, notFound(model.UpdateWidget(ctx, &m, by))
}

// keptWidgets reads d's widgets as they stand (a seam for tests).
var keptWidgets = Widgets

// checkChangedView is checkView for w's view, unless d's widget w.ID reads
// it already.
func checkChangedView(ctx context.Context, d Dashboard, w Widget, by uuid.UUID) error {
	kept, err := keptWidgets(ctx, d)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(w.ID)
	if err != nil {
		return ErrNotFound
	}
	for _, k := range kept {
		if k.ID == id.String() && k.Query.View == w.Query.View {
			return nil
		}
	}
	return checkView(ctx, d, w.Query.View, by)
}

func widgetRow(d Dashboard, w Widget) (model.Widget, error) {
	id, err := uuid.Parse(d.ID)
	if err != nil {
		return model.Widget{}, ErrNotFound
	}
	q, err := json.Marshal(w.Query)
	if err != nil {
		return model.Widget{}, err
	}
	return model.Widget{DashboardID: id, Kind: w.Kind, Title: w.Title, Query: q, X: w.X, Y: w.Y, W: w.W, H: w.H}, nil
}

// Arrange moves d's widgets to places at once, as by: every one must be d's
// live widget, or none moves.
func Arrange(ctx context.Context, d Dashboard, places []Place, by uuid.UUID) error {
	if err := CheckArrange(places); err != nil {
		return err
	}
	id, err := uuid.Parse(d.ID)
	if err != nil {
		return ErrNotFound
	}
	rows := make([]model.Place, 0, len(places))
	for _, p := range places {
		rows = append(rows, model.Place{ID: uuid.MustParse(p.ID), X: p.X, Y: p.Y, W: p.W, H: p.H})
	}
	return notFound(model.Arrange(ctx, id, rows, by))
}

// RemoveWidget deletes d's widget softly, as by.
func RemoveWidget(ctx context.Context, d Dashboard, widget string, by uuid.UUID) error {
	id, wid, err := ids(d, widget)
	if err != nil {
		return err
	}
	return notFound(model.DeleteWidget(ctx, id, wid, by))
}

// RestoreWidget brings d's removed widget back, while d has room.
func RestoreWidget(ctx context.Context, d Dashboard, widget string) error {
	id, wid, err := ids(d, widget)
	if err != nil {
		return err
	}
	restored, err := model.RestoreWidgetWithin(ctx, id, wid, MaxWidgets)
	if err != nil {
		return notFound(err)
	}
	if !restored {
		return ErrFull
	}
	return nil
}

func ids(d Dashboard, widget string) (uuid.UUID, uuid.UUID, error) {
	id, err := uuid.Parse(d.ID)
	if err != nil {
		return id, id, ErrNotFound
	}
	wid, err := uuid.Parse(widget)
	if err != nil {
		return id, wid, ErrNotFound
	}
	return id, wid, nil
}
