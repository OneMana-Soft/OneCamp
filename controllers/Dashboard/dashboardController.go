// Package controllers (Dashboard) is dashboards over HTTP (B9-b2;
// business/Dashboard, whose package comment states who may do what).
//
// A person's own and the workspace's are listed and made at /dashboards, a
// project's at /{project_uuid}/dashboards; every other route names the
// dashboard alone, and its scope is read from it: a project's checks the
// caller's standing in its project (Work Access) first, so someone the
// project isn't there for finds nothing (404), and so does anyone a
// dashboard isn't for (another's own; one not shared yet). Someone who reads
// one but may not keep it is told why (403).
package controllers

import (
	"errors"
	"net/http"
	"time"

	business "github.com/akashc777/OneCamp/business/Dashboard"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

const (
	bodyLimit = 64 << 10
	keepers   = "Only the project's admins keep its dashboards."
	admins    = "Only workspace admins keep the workspace's dashboards."
	keep      = "Only those who keep this dashboard change it."
	archived  = "This project is archived, so its dashboards can't be changed. Restore the project first."
	gone      = "That dashboard isn't there."
	full      = "There's no room for another here: delete one first."
)

// place is a dashboard as the caller stands to it.
type place struct {
	d        business.Dashboard
	deleted  bool
	who      userModels.UserInfo
	standing workAccess.Standing
}

func (p place) canRead() bool {
	return business.MayRead(p.d, p.who.UserPostgresInfo.Id, p.standing, p.who.UserPostgresInfo.IsAdmin)
}

func (p place) canKeep() bool {
	return business.MayKeep(p.d, p.who.UserPostgresInfo.Id, p.standing, p.who.UserPostgresInfo.IsAdmin)
}

// open is the dashboard in the URL as the caller stands to it, once they may
// read it (and, with keeping, keep it). A deleted one is there only to be
// restored by whoever keeps it.
func open(w http.ResponseWriter, r *http.Request, keeping bool) (place, bool) {
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return place{}, false
	}
	d, deleted, err := business.Get(r.Context(), chi.URLParam(r, "dashboard_id"))
	if err != nil {
		write(w, r, "open", err)
		return place{}, false
	}
	p := place{d: d, deleted: deleted, who: u}
	if d.Scope == business.ScopeProject {
		// Someone the project isn't there for finds nothing (Allow's 404).
		if _, _, p.standing, ok = projectaccess.RequireIDStanding(w, r, d.ProjectID, workAccess.NeedView, ""); !ok {
			return place{}, false
		}
	}
	if !p.canRead() || (deleted && !p.canKeep()) {
		helpers.WriteNotFound(w, gone)
		return place{}, false
	}
	if keeping && !p.canKeep() {
		switch {
		case d.Scope == business.ScopeProject && p.standing.Archived:
			helpers.WriteForbidden(w, archived)
		case d.Scope == business.ScopeProject:
			helpers.WriteForbidden(w, keepers)
		case d.Scope == business.ScopeWorkspace:
			helpers.WriteForbidden(w, admins)
		default:
			helpers.WriteForbidden(w, keep)
		}
		return place{}, false
	}
	return p, true
}

// live is open for a dashboard that isn't deleted.
func live(w http.ResponseWriter, r *http.Request, keeping bool) (place, bool) {
	p, ok := open(w, r, keeping)
	if ok && p.deleted {
		helpers.WriteNotFound(w, gone)
		return place{}, false
	}
	return p, ok
}

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var refusal *business.Refusal
	switch {
	case errors.As(err, &refusal):
		helpers.WriteBadRequest(w, refusal.Error())
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteNotFound(w, gone)
	case errors.Is(err, business.ErrFull):
		helpers.WriteBadRequest(w, full)
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Dashboard/%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "Something went wrong.")
	}
}

func answer(w http.ResponseWriter, data any) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": data})
}

// readable is those of list the caller may read.
func readable(list []business.Dashboard, who userModels.UserInfo, s workAccess.Standing) []business.Dashboard {
	out := make([]business.Dashboard, 0, len(list))
	for _, d := range list {
		if business.MayRead(d, who.UserPostgresInfo.Id, s, who.UserPostgresInfo.IsAdmin) {
			out = append(out, d)
		}
	}
	return out
}

// List is the caller's own dashboards and the workspace's they may read, and
// whether they keep the workspace's. GET /project/dashboards
func List(w http.ResponseWriter, r *http.Request) {
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	mine, err := business.ListMine(r.Context(), u.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, "List", err)
		return
	}
	shared, err := business.ListWorkspace(r.Context())
	if err != nil {
		write(w, r, "List", err)
		return
	}
	answer(w, map[string]any{"mine": mine, "workspace": readable(shared, u, workAccess.Standing{}),
		"can_keep_workspace": u.UserPostgresInfo.IsAdmin})
}

type input struct {
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	Shared bool   `json:"shared"`
}

// Add makes the caller's own dashboard, or (a workspace admin) the
// workspace's. POST /project/dashboards {name, scope: mine|workspace, shared}
func Add(w http.ResponseWriter, r *http.Request) {
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	var in input
	if !httpx.ReadJSON(w, r, bodyLimit, &in) {
		return
	}
	d := business.Dashboard{Scope: business.ScopeMine, MineOf: u.UserPostgresInfo.Id, Name: in.Name, Shared: in.Shared}
	switch in.Scope {
	case business.ScopeWorkspace:
		if !u.UserPostgresInfo.IsAdmin {
			helpers.WriteForbidden(w, admins)
			return
		}
		d = business.Dashboard{Scope: business.ScopeWorkspace, Name: in.Name, Shared: in.Shared}
	case business.ScopeMine, "":
	default:
		helpers.WriteBadRequest(w, "A dashboard is your own or the workspace's here; a project's is made in the project.")
		return
	}
	made, err := business.Add(r.Context(), d, u.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, "Add", err)
		return
	}
	answer(w, made)
}

// ListInProject is the project's dashboards the caller may read, and whether
// they keep them. GET /project/{project_uuid}/dashboards
func ListInProject(w http.ResponseWriter, r *http.Request) {
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	id, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	list, err := business.ListProject(r.Context(), id)
	if err != nil {
		write(w, r, "ListInProject", err)
		return
	}
	can := business.MayKeep(business.Dashboard{Scope: business.ScopeProject}, u.UserPostgresInfo.Id, s, u.UserPostgresInfo.IsAdmin)
	answer(w, map[string]any{"dashboards": readable(list, u, s), "can_keep": can})
}

// AddToProject makes one of the project's dashboards: its admins, never
// while it is archived. POST /project/{project_uuid}/dashboards {name, shared}
func AddToProject(w http.ResponseWriter, r *http.Request) {
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	id, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	d := business.Dashboard{Scope: business.ScopeProject, ProjectID: id.String()}
	if !business.MayKeep(d, u.UserPostgresInfo.Id, s, u.UserPostgresInfo.IsAdmin) {
		if s.Archived {
			helpers.WriteForbidden(w, archived)
		} else {
			helpers.WriteForbidden(w, keepers)
		}
		return
	}
	var in input
	if !httpx.ReadJSON(w, r, bodyLimit, &in) {
		return
	}
	d.Name, d.Shared = in.Name, in.Shared
	made, err := business.Add(r.Context(), d, u.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, "AddToProject", err)
		return
	}
	answer(w, made)
}

// Get is a dashboard with its widgets, and whether the caller keeps it: one
// who keeps it reads each widget's whole query, to change it; a reader, only
// what draws it (business.Shown). GET /project/dashboards/{dashboard_id}
func Get(w http.ResponseWriter, r *http.Request) {
	p, ok := live(w, r, false)
	if !ok {
		return
	}
	widgets, err := business.Widgets(r.Context(), p.d)
	if err != nil {
		write(w, r, "Get", err)
		return
	}
	keeps := p.canKeep()
	answer(w, map[string]any{"dashboard": p.d, "widgets": business.Shown(widgets, keeps), "can_keep": keeps})
}

// Data is every widget worked out as the caller, in tz: nothing a widget
// shows is kept. GET /project/dashboards/{dashboard_id}/data?tz=
func Data(w http.ResponseWriter, r *http.Request) {
	p, ok := live(w, r, false)
	if !ok {
		return
	}
	widgets, err := business.Widgets(r.Context(), p.d)
	if err != nil {
		write(w, r, "Data", err)
		return
	}
	reader := business.Reader{UUID: p.who.UserPostgresInfo.Id, DgraphUID: p.who.UserDgraphInfo.Uid, IsAdmin: p.who.UserPostgresInfo.IsAdmin}
	answer(w, map[string]any{"widgets": business.Compute(r.Context(), reader, time.Now(), helpers.Location(r.URL.Query().Get("tz")), widgets)})
}

// Change renames the dashboard and shares it or not.
// POST /project/dashboards/{dashboard_id} {name, shared}
func Change(w http.ResponseWriter, r *http.Request) {
	p, ok := live(w, r, true)
	if !ok {
		return
	}
	var in input
	if !httpx.ReadJSON(w, r, bodyLimit, &in) {
		return
	}
	d, err := business.Change(r.Context(), p.d, in.Name, in.Shared, p.who.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, "Change", err)
		return
	}
	answer(w, d)
}

// Delete deletes the dashboard softly. POST /project/dashboards/{dashboard_id}/delete
func Delete(w http.ResponseWriter, r *http.Request) {
	p, ok := live(w, r, true)
	if !ok {
		return
	}
	if err := business.Delete(r.Context(), p.d, p.who.UserPostgresInfo.Id); err != nil {
		write(w, r, "Delete", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted.", "data": map[string]string{"id": p.d.ID}})
}

// Restore brings a deleted dashboard back. POST /project/dashboards/{dashboard_id}/restore
func Restore(w http.ResponseWriter, r *http.Request) {
	p, ok := open(w, r, true)
	if !ok {
		return
	}
	if err := business.Restore(r.Context(), p.d); err != nil {
		write(w, r, "Restore", err)
		return
	}
	answer(w, p.d)
}

// AddWidget puts a widget on the dashboard.
// POST /project/dashboards/{dashboard_id}/widgets {kind, title, query, x, y, w, h}
func AddWidget(w http.ResponseWriter, r *http.Request) {
	p, ok := live(w, r, true)
	if !ok {
		return
	}
	var in business.Widget
	if !httpx.ReadJSON(w, r, bodyLimit, &in) {
		return
	}
	made, err := business.AddWidget(r.Context(), p.d, in, p.who.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, "AddWidget", err)
		return
	}
	answer(w, made)
}

// ChangeWidget changes one of the dashboard's widgets.
// POST /project/dashboards/{dashboard_id}/widgets/{widget_id} {kind, title, query, x, y, w, h}
func ChangeWidget(w http.ResponseWriter, r *http.Request) {
	p, ok := live(w, r, true)
	if !ok {
		return
	}
	var in business.Widget
	if !httpx.ReadJSON(w, r, bodyLimit, &in) {
		return
	}
	in.ID = chi.URLParam(r, "widget_id")
	changed, err := business.ChangeWidget(r.Context(), p.d, in, p.who.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, "ChangeWidget", err)
		return
	}
	answer(w, changed)
}

// RemoveWidget deletes one of the dashboard's widgets softly.
// POST /project/dashboards/{dashboard_id}/widgets/{widget_id}/delete
func RemoveWidget(w http.ResponseWriter, r *http.Request) {
	p, ok := live(w, r, true)
	if !ok {
		return
	}
	id := chi.URLParam(r, "widget_id")
	if err := business.RemoveWidget(r.Context(), p.d, id, p.who.UserPostgresInfo.Id); err != nil {
		write(w, r, "RemoveWidget", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Removed.", "data": map[string]string{"id": id}})
}

// RestoreWidget brings a removed widget back.
// POST /project/dashboards/{dashboard_id}/widgets/{widget_id}/restore
func RestoreWidget(w http.ResponseWriter, r *http.Request) {
	p, ok := live(w, r, true)
	if !ok {
		return
	}
	id := chi.URLParam(r, "widget_id")
	if err := business.RestoreWidget(r.Context(), p.d, id); err != nil {
		write(w, r, "RestoreWidget", err)
		return
	}
	answer(w, map[string]string{"id": id})
}

// Arrange moves the dashboard's widgets at once.
// POST /project/dashboards/{dashboard_id}/arrange {places: [{id, x, y, w, h}]}
func Arrange(w http.ResponseWriter, r *http.Request) {
	p, ok := live(w, r, true)
	if !ok {
		return
	}
	var in struct {
		Places []business.Place `json:"places"`
	}
	if !httpx.ReadJSON(w, r, bodyLimit, &in) {
		return
	}
	if err := business.Arrange(r.Context(), p.d, in.Places, p.who.UserPostgresInfo.Id); err != nil {
		write(w, r, "Arrange", err)
		return
	}
	answer(w, map[string]any{"places": in.Places})
}
