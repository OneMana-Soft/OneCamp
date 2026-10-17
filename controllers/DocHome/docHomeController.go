package controllers

// The docs home over HTTP (router/docsHome.go, docs programme B5): search
// over docs' bodies, a template into an empty doc, and a doc saved as a
// template. Who may do what is the Access Resolver's to say
// (business/DocHome asks it); a doc the person can't open is never named.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	business "github.com/akashc777/OneCamp/business/DocHome"
	marketplaceBusiness "github.com/akashc777/OneCamp/business/Marketplace"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// isUUID says whether s is a uuid, as uuids are written.
func isUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == strings.ToLower(s)
}

func badRequest(w http.ResponseWriter, msg string) {
	helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": msg})
}

// failed answers an error from the docs home's business.
func failed(ctx context.Context, w http.ResponseWriter, where string, err error) {
	switch {
	case errors.Is(err, business.ErrSearchTooShort):
		badRequest(w, err.Error())
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteDocNotFound(w)
	case errors.Is(err, business.ErrDeleted):
		helpers.WriteJSON(w, http.StatusGone, helpers.Envolope{"msg": "This doc was deleted.", "deleted": true})
	case errors.Is(err, business.ErrForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You can't change this doc."})
	case errors.Is(err, business.ErrNotEmpty):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"code": "not_empty", "msg": err.Error()})
	case errors.Is(err, docBusiness.ErrNoTemplate):
		badRequest(w, "There's no such template.")
	case errors.Is(err, marketplaceBusiness.ErrEmptyTemplate):
		helpers.WriteJSON(w, http.StatusUnprocessableEntity, helpers.Envolope{"code": "empty", "msg": err.Error()})
	case errors.Is(err, marketplaceBusiness.ErrTemplateTooLarge):
		helpers.WriteJSON(w, http.StatusUnprocessableEntity, helpers.Envolope{"code": "too_large", "msg": err.Error()})
	case errors.Is(err, marketplaceBusiness.ErrTemplateName):
		badRequest(w, err.Error())
	case errors.Is(err, collab.ErrDocInTrash):
		helpers.WriteJSON(w, http.StatusGone, helpers.Envolope{"msg": "This doc was deleted.", "deleted": true})
	case errors.Is(err, collab.ErrUnavailable):
		helpers.WriteRetryLater(w, "The doc can't be written just now.")
	default:
		helpers.LogErrorWithContext(ctx, "controllers/DocHome %s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Something went wrong. Try again."})
	}
}

// searchLimit reads a page size: 1 to 50, 20 when not given.
func searchLimit(r *http.Request) int { return helpers.LimitParam(r.URL.Query(), 20, 50) }

// SearchDocs is the docs home's search (GET /doc/search?q=&owner=&limit=
// &cursor=): docs whose title or body has the words, best first, with the
// passages they were found in, each one the person can open.
func SearchDocs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	q := r.URL.Query()
	owner := q.Get("owner")
	if owner != "" && !isUUID(owner) {
		badRequest(w, "That isn't a person.")
		return
	}
	searcher := business.Searcher{Subject: who}
	for _, p := range user.UserDgraphInfo.Projects {
		if p != nil && p.Uuid != "" {
			searcher.ProjectUUIDs = append(searcher.ProjectUUIDs, p.Uuid)
		}
	}
	for _, t := range user.UserDgraphInfo.Teams {
		if t != nil && t.Uuid != "" {
			searcher.TeamUUIDs = append(searcher.TeamUUIDs, t.Uuid)
		}
	}
	page, err := business.SearchDocs(ctx, searcher, q.Get("q"), owner, helpers.CursorOffset(q.Get("cursor")), searchLimit(r))
	if err != nil {
		failed(ctx, w, "SearchDocs", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": page})
}

// ApplyTemplate gives an empty doc a template's body (POST /doc/applyTemplate
// {doc_uuid, template_id}), written by the server into the live doc: 409 once
// the doc holds anything.
func ApplyTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, who := accessBusiness.RequestSubject(r)
	var in struct {
		DocUUID    string `json:"doc_uuid"`
		TemplateID string `json:"template_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !isUUID(in.DocUUID) {
		badRequest(w, "Say which doc the template goes into.")
		return
	}
	if strings.TrimSpace(in.TemplateID) == "" {
		badRequest(w, "Say which template to use.")
		return
	}
	res, err := business.ApplyTemplate(ctx, who, in.DocUUID, in.TemplateID)
	if err != nil {
		failed(ctx, w, "ApplyTemplate", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Template added.", "data": helpers.Envolope{"persisted": res.Persisted}})
}

// SaveAsTemplate makes a doc the person may change a template the team can
// start from (POST /doc/saveAsTemplate {doc_uuid, name, description?}).
func SaveAsTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, who := accessBusiness.RequestSubject(r)
	var in struct {
		DocUUID     string `json:"doc_uuid"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !isUUID(in.DocUUID) {
		badRequest(w, "Say which doc to make a template of.")
		return
	}
	t, err := business.SaveAsTemplate(ctx, who, user.UserPostgresInfo.Id, in.DocUUID, in.Name, strings.TrimSpace(in.Description))
	if err != nil {
		failed(ctx, w, "SaveAsTemplate", err)
		return
	}
	auditBusiness.Record(r, "marketplace.publish", auditBusiness.CategoryApp,
		"Published template: "+t.Name, map[string]interface{}{"template_id": t.Id.String(), "kind": t.Kind, "doc_uuid": in.DocUUID})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"msg": "Saved as a template.", "data": t})
}
