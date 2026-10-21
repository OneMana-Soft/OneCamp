package controllers

// The workspace export (docs programme C4): an admin exports every doc they
// can open, and every private doc whose owner let it in, as a zip of Markdown
// (business/ContentIO workspaceExport.go). A doc's owner lets it in, or takes
// it out, here too. Starting an export and every consent go to the audit log.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// StartWorkspaceExport starts an export of the workspace's docs, for an admin
// (the route is /admin's), and records it in the audit log.
//
// POST /admin/docs/exports → 202 {export}; 409 {export, msg} while one they
// started is still being made
func StartWorkspaceExport(w http.ResponseWriter, r *http.Request) {
	who, _, ok := whoIs(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	job, err := contentIOBusiness.ExportWorkspace(r.Context(), who)
	switch {
	case errors.Is(err, contentIOBusiness.ErrConflict):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"export": job, "msg": "An export you started is still being made."})
		return
	case errors.Is(err, contentIOBusiness.ErrNotAdmin):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only an admin can export the workspace."})
		return
	case err != nil:
		fail(w, err)
		return
	}
	auditBusiness.Record(r, "docs.workspace_export", auditBusiness.CategoryDocs,
		"Started an export of the workspace's docs", map[string]interface{}{"job_id": job.ID})
	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"export": job})
}

// ListWorkspaceExports is the admin's last few workspace exports, newest
// first.
//
// GET /admin/docs/exports → 200 {exports}
func ListWorkspaceExports(w http.ResponseWriter, r *http.Request) {
	who, _, ok := whoIs(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	exports, err := contentIOBusiness.RecentWorkspaceExports(r.Context(), who)
	if err != nil {
		fail(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"exports": exports})
}

// GetWorkspaceExport is one of the admin's workspace exports, with an address
// to download it from once it is made ("" before, and once the zip is gone a
// day later). The address is good for 15 minutes, and handing it out is in
// the audit log: the zip holds every doc the export took.
//
// GET /admin/docs/exports/{job_id} → 200 {export, download_url}
func GetWorkspaceExport(w http.ResponseWriter, r *http.Request) {
	who, _, ok := whoIs(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	job, url, err := contentIOBusiness.WorkspaceExport(r.Context(), who, chi.URLParam(r, "job_id"))
	if err != nil {
		fail(w, err)
		return
	}
	if url != "" {
		auditBusiness.Record(r, "docs.workspace_export_download", auditBusiness.CategoryDocs,
			"Downloaded an export of the workspace's docs", map[string]interface{}{"job_id": job.ID})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"export": job, "download_url": url})
}

// GetExportConsent says whether a doc's owner let it into workspace exports,
// and whether the caller is that owner.
//
// GET /doc/exportConsent?doc_uuid= → 200 {consent: {allowed, owner}}
func GetExportConsent(w http.ResponseWriter, r *http.Request) {
	who, _, ok := whoIs(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	state, err := contentIOBusiness.ExportConsent(r.Context(), who, r.URL.Query().Get("doc_uuid"))
	if err != nil {
		fail(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"consent": state})
}

// SetExportConsent lets a doc into workspace exports, or takes it back out:
// for its owner. Recorded in the audit log, which never names a doc by its
// title here (the doc is one the admins reading the log can't open).
//
// POST /doc/exportConsent {doc_uuid, allow} → 200 {consent}
func SetExportConsent(w http.ResponseWriter, r *http.Request) {
	who, _, ok := whoIs(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	var body struct {
		DocUUID string `json:"doc_uuid"`
		Allow   *bool  `json:"allow"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil || body.DocUUID == "" || body.Allow == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which doc, and whether to let it in."})
		return
	}
	err := contentIOBusiness.SetExportConsent(r.Context(), who, body.DocUUID, *body.Allow)
	if errors.Is(err, contentIOBusiness.ErrForbidden) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the doc's owner can change this."})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	summary := "Took a private page out of workspace exports"
	if *body.Allow {
		summary = "Let a private page into workspace exports"
	}
	auditBusiness.Record(r, "docs.export_consent", auditBusiness.CategoryDocs, summary,
		map[string]interface{}{"resource_kind": "doc", "resource_uuid": body.DocUUID, "allowed": *body.Allow})
	state, err := contentIOBusiness.ExportConsent(r.Context(), who, body.DocUUID)
	if err != nil {
		fail(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"consent": state})
}
