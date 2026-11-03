// Package controllers (TaskTemplate) is task templates over HTTP (linear-09;
// business/TaskTemplate).
//
// WHO, as business/TaskTemplate's MayApply and MayManage say. In a project,
// whoever may add its tasks (Work Access NeedCreateTask) lists its templates
// and the workspace's, reads one and makes a task from it; its admins
// (NeedManage) keep, change, delete and restore its own. An archived project
// refuses all of it. The workspace's templates: any member lists and reads
// them, and its admins keep, change, delete and restore them. Someone a
// project isn't there for finds nothing (404), a member who may not is told
// why (403), and a template kept elsewhere isn't there (404) from here.
package controllers

import (
	"errors"
	"net/http"
	"strings"

	projectTemplate "github.com/akashc777/OneCamp/business/ProjectTemplate"
	business "github.com/akashc777/OneCamp/business/TaskTemplate"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	// bodyLimit holds a template at its limits (50 sub-tasks, 50 fields).
	bodyLimit = 256 << 10
	users     = "Only those who can add tasks to this project use its templates."
	keepers   = "Only the project's admins keep its templates."
	admins    = "Only workspace admins keep the workspace's templates."
	archived  = "This project is archived, so its templates can't be used or changed. Restore the project first."
	gone      = "That template isn't there any more."
	missing   = "Fill in what this template asks for first."
)

// place is where a request's templates are kept: a project, with the
// caller's standing in it, or (uuid.Nil) the workspace.
type place struct {
	project  uuid.UUID
	standing workAccess.Standing
	who      userModels.UserInfo
}

// scope is the place as a template's ProjectID names it.
func (p place) scope() string {
	if p.project == uuid.Nil {
		return ""
	}
	return p.project.String()
}

// canKeep is MayManage for the place's own templates.
func (p place) canKeep() bool {
	return business.MayManage(business.Template{ProjectID: p.scope()}, p.standing, p.who.UserPostgresInfo.IsAdmin)
}

// project checks the caller may do need with the project in the URL: use
// its templates (NeedCreateTask, MayApply) or keep them (NeedManage,
// MayManage).
func project(w http.ResponseWriter, r *http.Request, need workAccess.Need) (place, bool) {
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return place{}, false
	}
	deny := users
	if need == workAccess.NeedManage {
		deny = keepers
	}
	id, _, s, ok := projectaccess.RequireStanding(w, r, need, deny)
	if !ok {
		return place{}, false
	}
	p := place{project: id, standing: s, who: u}
	if s.Archived {
		helpers.WriteForbidden(w, archived)
		return place{}, false
	}
	// The rule itself, behind Work Access's check of need.
	may := business.MayApply(s)
	if need == workAccess.NeedManage {
		may = p.canKeep()
	}
	if !may {
		helpers.WriteForbidden(w, deny)
		return place{}, false
	}
	return p, true
}

// workspace is the workspace's templates: any member uses them, and keeping
// them (keep) takes a workspace admin.
func workspace(w http.ResponseWriter, r *http.Request, keep bool) (place, bool) {
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return place{}, false
	}
	p := place{who: u}
	if keep && !p.canKeep() {
		helpers.WriteForbidden(w, admins)
		return place{}, false
	}
	return p, true
}

// kept is the template in the URL as the place keeps it, live or deleted.
func (p place) kept(w http.ResponseWriter, r *http.Request) (business.Template, bool, bool) {
	t, deleted, err := business.Get(r.Context(), chi.URLParam(r, "template_id"))
	if err == nil && t.ProjectID != p.scope() {
		err = business.ErrNotFound
	}
	if err != nil {
		write(w, r, "kept", err)
		return business.Template{}, false, false
	}
	return t, deleted, true
}

// usable is the live template in the URL that a task in the place's project
// may start from: the project's own, or the workspace's.
func (p place) usable(w http.ResponseWriter, r *http.Request) (business.Template, bool) {
	t, deleted, err := business.Get(r.Context(), chi.URLParam(r, "template_id"))
	if err == nil && (deleted || !business.Usable(t, p.project)) {
		err = business.ErrNotFound
	}
	if err != nil {
		write(w, r, "usable", err)
		return business.Template{}, false
	}
	return t, true
}

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	if projectaccess.RefuseStatus(w, err) {
		return
	}
	var te *business.TemplateError
	switch {
	case errors.As(err, &te):
		helpers.WriteBadRequest(w, te.Error())
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteNotFound(w, gone)
	case errors.Is(err, taskWriter.ErrBadValue):
		helpers.WriteBadRequest(w, "That task can't be made as it is: "+strings.TrimPrefix(err.Error(), taskWriter.ErrBadValue.Error()+": ")+".")
	case errors.Is(err, workAccess.ErrNotAllowed):
		helpers.WriteForbidden(w, err.Error())
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/TaskTemplate/%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "Something went wrong.")
	}
}

// list answers the place's list, what the caller may keep, and the types a
// template there may have (a project's built-in and own ones; the
// workspace's built-in ones alone), for a template's type and default.
func list(w http.ResponseWriter, r *http.Request, p place, where string, read func() ([]business.Summary, error)) {
	got, err := read()
	if err != nil {
		write(w, r, where, err)
		return
	}
	types, err := workTaxonomy.Types(r.Context(), p.project)
	if err != nil {
		write(w, r, where, err)
		return
	}
	// defaults: for each type, the template a new task of it starts from.
	out := map[string]any{"templates": got, "defaults": business.Defaults(got), "can_manage": p.canKeep(), "types": workTaxonomy.Answers(types)}
	if p.project != uuid.Nil {
		out["can_manage_workspace"] = business.MayManage(business.Template{}, p.standing, p.who.UserPostgresInfo.IsAdmin)
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// add keeps the template in the body in the place.
func add(w http.ResponseWriter, r *http.Request, p place, where string) {
	var t business.Template
	if !httpx.ReadJSON(w, r, bodyLimit, &t) {
		return
	}
	t.ProjectID = p.scope()
	saved, err := business.Save(r.Context(), t, p.who.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, where, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": saved})
}

// change changes the place's template in the URL to the one in the body.
func change(w http.ResponseWriter, r *http.Request, p place, where string) {
	old, deleted, ok := p.kept(w, r)
	if !ok {
		return
	}
	if deleted {
		helpers.WriteNotFound(w, gone)
		return
	}
	var t business.Template
	if !httpx.ReadJSON(w, r, bodyLimit, &t) {
		return
	}
	t.ID, t.ProjectID = old.ID, old.ProjectID
	saved, err := business.Update(r.Context(), t, p.who.UserPostgresInfo.Id)
	if err != nil {
		write(w, r, where, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": saved})
}

// remove deletes the place's template in the URL softly.
func remove(w http.ResponseWriter, r *http.Request, p place, where string) {
	t, _, ok := p.kept(w, r)
	if !ok {
		return
	}
	if err := business.Delete(r.Context(), t.ID, p.who.UserPostgresInfo.Id); err != nil {
		write(w, r, where, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted.", "data": map[string]string{"id": t.ID}})
}

// restore brings the place's deleted template in the URL back.
func restore(w http.ResponseWriter, r *http.Request, p place, where string) {
	t, _, ok := p.kept(w, r)
	if !ok {
		return
	}
	if err := business.Restore(r.Context(), t); err != nil {
		write(w, r, where, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": t})
}

// ListInProject is the templates a task in the project may start from: its
// own, then the workspace's, each by name; the one each type starts from;
// and whether the caller keeps either. GET /project/{project_uuid}/task-templates
func ListInProject(w http.ResponseWriter, r *http.Request) {
	if p, ok := project(w, r, workAccess.NeedCreateTask); ok {
		list(w, r, p, "ListInProject", func() ([]business.Summary, error) { return business.List(r.Context(), p.project) })
	}
}

// GetInProject is one of them in full, to make a task from or to change,
// with its assignee as the shared rule shows them (business/TaskTemplate Show).
// GET /project/{project_uuid}/task-templates/{template_id}
func GetInProject(w http.ResponseWriter, r *http.Request) {
	p, ok := project(w, r, workAccess.NeedCreateTask)
	if !ok {
		return
	}
	if t, ok := p.usable(w, r); ok {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.Show(r.Context(), t, p.project)})
	}
}

// MakeInProject makes a task in the project from one of them, typed over
// (business/TaskTemplate Typed), its days counted from today in the maker's
// zone (tz). A property it asks for and the maker left empty answers 422
// with the properties still to fill, and nothing is made.
// POST /project/{project_uuid}/task-templates/{template_id}/make
func MakeInProject(w http.ResponseWriter, r *http.Request) {
	p, ok := project(w, r, workAccess.NeedCreateTask)
	if !ok {
		return
	}
	t, ok := p.usable(w, r)
	if !ok {
		return
	}
	var in struct {
		business.Typed
		TZ string `json:"tz"`
	}
	if !httpx.ReadJSON(w, r, bodyLimit, &in) {
		return
	}
	res, err := business.Make(r.Context(), &p.who, p.project, t, in.Typed, projectTemplate.StartOn("", in.TZ, false))
	var asks *business.MissingError
	if errors.As(err, &asks) {
		helpers.WriteJSON(w, http.StatusUnprocessableEntity, helpers.Envolope{"msg": missing, "missing": asks.Props, "left_off": res.LeftOff})
		return
	}
	if err != nil {
		write(w, r, "MakeInProject", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// AddToProject keeps a template for the project.
// POST /project/{project_uuid}/task-templates
func AddToProject(w http.ResponseWriter, r *http.Request) {
	if p, ok := project(w, r, workAccess.NeedManage); ok {
		add(w, r, p, "AddToProject")
	}
}

// ChangeInProject changes one of the project's templates.
// POST /project/{project_uuid}/task-templates/{template_id}
func ChangeInProject(w http.ResponseWriter, r *http.Request) {
	if p, ok := project(w, r, workAccess.NeedManage); ok {
		change(w, r, p, "ChangeInProject")
	}
}

// DeleteFromProject deletes one of the project's templates softly.
// POST /project/{project_uuid}/task-templates/{template_id}/delete
func DeleteFromProject(w http.ResponseWriter, r *http.Request) {
	if p, ok := project(w, r, workAccess.NeedManage); ok {
		remove(w, r, p, "DeleteFromProject")
	}
}

// RestoreToProject brings one of the project's deleted templates back.
// POST /project/{project_uuid}/task-templates/{template_id}/restore
func RestoreToProject(w http.ResponseWriter, r *http.Request) {
	if p, ok := project(w, r, workAccess.NeedManage); ok {
		restore(w, r, p, "RestoreToProject")
	}
}

// ListWorkspace is the workspace's templates by name, and whether the
// caller keeps them. GET /project/task-templates
func ListWorkspace(w http.ResponseWriter, r *http.Request) {
	if p, ok := workspace(w, r, false); ok {
		list(w, r, p, "ListWorkspace", func() ([]business.Summary, error) { return business.ListWorkspace(r.Context()) })
	}
}

// GetFromWorkspace is one of the workspace's templates in full.
// GET /project/task-templates/{template_id}
func GetFromWorkspace(w http.ResponseWriter, r *http.Request) {
	p, ok := workspace(w, r, false)
	if !ok {
		return
	}
	t, deleted, ok := p.kept(w, r)
	if !ok {
		return
	}
	if deleted {
		helpers.WriteNotFound(w, gone)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.Show(r.Context(), t, uuid.Nil)})
}

// AddToWorkspace keeps a template for the whole workspace.
// POST /project/task-templates
func AddToWorkspace(w http.ResponseWriter, r *http.Request) {
	if p, ok := workspace(w, r, true); ok {
		add(w, r, p, "AddToWorkspace")
	}
}

// ChangeInWorkspace changes one of the workspace's templates.
// POST /project/task-templates/{template_id}
func ChangeInWorkspace(w http.ResponseWriter, r *http.Request) {
	if p, ok := workspace(w, r, true); ok {
		change(w, r, p, "ChangeInWorkspace")
	}
}

// DeleteFromWorkspace deletes one of the workspace's templates softly.
// POST /project/task-templates/{template_id}/delete
func DeleteFromWorkspace(w http.ResponseWriter, r *http.Request) {
	if p, ok := workspace(w, r, true); ok {
		remove(w, r, p, "DeleteFromWorkspace")
	}
}

// RestoreToWorkspace brings one of the workspace's deleted templates back.
// POST /project/task-templates/{template_id}/restore
func RestoreToWorkspace(w http.ResponseWriter, r *http.Request) {
	if p, ok := workspace(w, r, true); ok {
		restore(w, r, p, "RestoreToWorkspace")
	}
}
