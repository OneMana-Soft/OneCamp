// Package controllers (ProjectLink) is a project's links over HTTP
// (basecamp-16): see business/ProjectLink. Whoever may see a project reads its
// links and their pictures; its admins add, rename, delete and restore them,
// while it isn't archived.
package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	linkBusiness "github.com/akashc777/OneCamp/business/ProjectLink"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// adminsOnly is the refusal of a change to a project's links.
const adminsOnly = "Only the project's admins can change its links."

// archivedRefusal is the refusal of a change to an archived project's links.
const archivedRefusal = "This project is archived, so its links can't change. Restore the project first."

// noSuchLink is the answer about a link the project doesn't have.
const noSuchLink = "That link isn't on this project."

// project checks the caller may do need with the project in the URL: see its
// links, or (NeedManage) change them, which an archived project refuses.
func project(w http.ResponseWriter, r *http.Request, need workAccess.Need) (uuid.UUID, workAccess.Standing, bool) {
	id, _, s, ok := projectaccess.RequireStanding(w, r, need, adminsOnly)
	if !ok {
		return uuid.Nil, s, false
	}
	if need != workAccess.NeedView && s.Archived {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": archivedRefusal})
		return uuid.Nil, s, false
	}
	return id, s, true
}

// linkIn reads the link in the URL.
func linkIn(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "link_id"))
	if err != nil {
		helpers.WriteNotFound(w, noSuchLink)
		return uuid.Nil, false
	}
	return id, true
}

// decode reads a request's JSON body into v, or answers that it couldn't.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(v); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return false
	}
	return true
}

// write answers err in words.
func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	switch {
	case errors.Is(err, linkBusiness.ErrInvalidURL):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a web address. A link starts with http:// or https://."})
	case errors.Is(err, linkBusiness.ErrNoTitle):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A link needs a title."})
	case errors.Is(err, linkBusiness.ErrDuplicate):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "This project has a link to that address already."})
	case errors.Is(err, linkBusiness.ErrFull):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": fmt.Sprintf("A project holds at most %d links. Delete one to add another.", linkBusiness.MostLinks)})
	case errors.Is(err, linkBusiness.ErrNotFound):
		helpers.WriteNotFound(w, noSuchLink)
	case errors.Is(err, linkBusiness.ErrNoPicture):
		helpers.WriteNotFound(w, "That link has no such picture.")
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/ProjectLink/%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "Something went wrong with the project's links.")
	}
}

// ListLinks is GET /project/{project_uuid}/links: the project's links, and
// whether the caller may change them.
func ListLinks(w http.ResponseWriter, r *http.Request) {
	id, s, ok := project(w, r, workAccess.NeedView)
	if !ok {
		return
	}
	links, err := linkBusiness.List(r.Context(), id)
	if err != nil {
		write(w, r, "ListLinks", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{
		"links":      links,
		"can_manage": !s.Archived && s.Can(workAccess.NeedManage),
	}})
}

// AddLink is POST /project/{project_uuid}/links {url, title?}: the server
// reads the page and keeps the link's card. The read finishes even when the
// caller goes away, so the link they asked for is never half made.
func AddLink(w http.ResponseWriter, r *http.Request) {
	id, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	var in struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if !decode(w, r, &in) {
		return
	}
	v, err := linkBusiness.Add(context.WithoutCancel(r.Context()), id, u.UserPostgresInfo.Id, in.URL, in.Title)
	if err != nil {
		write(w, r, "AddLink", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// EditLink is POST /project/{project_uuid}/links/{link_id}/edit {title}.
func EditLink(w http.ResponseWriter, r *http.Request) {
	id, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	link, ok := linkIn(w, r)
	if !ok {
		return
	}
	var in struct {
		Title string `json:"title"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := linkBusiness.Rename(r.Context(), id, link, in.Title); err != nil {
		write(w, r, "EditLink", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Renamed"})
}

// DeleteLink is POST /project/{project_uuid}/links/{link_id}/delete: a soft
// delete, which POST /restore/link/{id} undoes.
func DeleteLink(w http.ResponseWriter, r *http.Request) {
	id, _, ok := project(w, r, workAccess.NeedManage)
	if !ok {
		return
	}
	link, ok := linkIn(w, r)
	if !ok {
		return
	}
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	if err := linkBusiness.Delete(r.Context(), id, link, u.UserPostgresInfo.Id); err != nil {
		write(w, r, "DeleteLink", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted"})
}

// GetLinkPicture is GET /project/{project_uuid}/links/{link_id}/{kind}: a 302
// to the link's stored icon or image, for whoever may see the project.
func GetLinkPicture(w http.ResponseWriter, r *http.Request) {
	id, _, ok := project(w, r, workAccess.NeedView)
	if !ok {
		return
	}
	link, ok := linkIn(w, r)
	if !ok {
		return
	}
	kind := chi.URLParam(r, "kind")
	key, err := linkBusiness.Picture(r.Context(), id, link, kind)
	if err != nil {
		write(w, r, "GetLinkPicture", err)
		return
	}
	httpx.RedirectToStored(w, r, key, kind)
}

// RestoreLink is POST /restore/link/{id}: the project's admins bring a
// deleted link back, unless the project is archived, holds its most links
// (409, as an add) or a live link has its address since (409).
func RestoreLink(w http.ResponseWriter, r *http.Request, id uuid.UUID, owner softDelete.Owner) {
	_, p, ok := projectaccess.RequireID(w, r, owner.Project.String(), workAccess.NeedManage, adminsOnly)
	if !ok {
		return
	}
	if helpers.IsSoftDeleted(p.DeletedAt) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": archivedRefusal})
		return
	}
	if err := linkBusiness.Restore(r.Context(), owner.Project, id); err != nil {
		write(w, r, "RestoreLink", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Link restored."})
}
