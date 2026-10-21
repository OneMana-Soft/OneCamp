// Package controllers answers the docs programme's import and export routes
// (A7): import a file as docs, follow the job, export a doc, and record an
// export the browser made. The work is business/ContentIO's.
package controllers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// whoIs is the signed-in member as the pipeline decides for them.
func whoIs(r *http.Request) (accessBusiness.Subject, userModels.UserInfo, bool) {
	u, who := accessBusiness.RequestSubject(r)
	return who, u, who.UserUID != ""
}

// fail answers an error from the pipeline with its status and a line a
// person can read.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, contentIOBusiness.ErrUnsupported):
		helpers.WriteJSON(w, http.StatusUnsupportedMediaType, helpers.Envolope{"msg": "This file type can't be imported. Use Markdown, HTML, text, Word (.docx) or a zip of them."})
	case errors.Is(err, contentIOBusiness.ErrTooLarge):
		helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{"msg": "The file is larger than an import takes."})
	case errors.Is(err, contentIOBusiness.ErrForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You can't add pages there."})
	case errors.Is(err, contentIOBusiness.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Not found, or not yours."})
	case errors.Is(err, contentIOBusiness.ErrConflict):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "This import has already ended."})
	case errors.Is(err, contentIOBusiness.ErrUnavailable):
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Imports and exports are not available on this server right now."})
	default:
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "That didn't go through. Try again shortly."})
	}
}

// ImportDoc takes a file sent with the request (multipart: file, and
// parent_uuid for where its pages go) and starts its import.
//
// POST /doc/import → 202 {job}
func ImportDoc(w http.ResponseWriter, r *http.Request) {
	who, _, ok := whoIs(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Send the file as multipart form data."})
		return
	}
	var parent string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				fail(w, contentIOBusiness.ErrTooLarge)
				return
			}
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "The form could not be read."})
			return
		}
		switch part.FormName() {
		case "parent_uuid":
			b, _ := io.ReadAll(io.LimitReader(part, 64))
			parent = strings.TrimSpace(string(b))
		case "file":
			job, err := contentIOBusiness.Pipeline.Import(r.Context(), who, contentIOBusiness.ImportRequest{
				Filename:   part.FileName(),
				MediaType:  part.Header.Get("Content-Type"),
				ParentUUID: parent,
				Size:       -1,
				Body:       part,
			})
			if err != nil {
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					err = contentIOBusiness.ErrTooLarge
				}
				fail(w, err)
				return
			}
			helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"job": job})
			return
		}
	}
	helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Send the file (after parent_uuid, when there is one)."})
}

type presignBody struct {
	Filename   string `json:"filename"`
	MediaType  string `json:"media_type"`
	Size       int64  `json:"size"`
	ParentUUID string `json:"parent_uuid"`
}

// PresignImport starts the import of a file too large to send with a
// request: the browser puts it at the answer's address, then calls
// StartImport.
//
// POST /doc/import/presign {filename, media_type, size, parent_uuid} → 201 {job, upload}
func PresignImport(w http.ResponseWriter, r *http.Request) {
	who, _, ok := whoIs(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	var body presignBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil || strings.TrimSpace(body.Filename) == "" || body.Size <= 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say the file's name and size."})
		return
	}
	job, upload, err := contentIOBusiness.Pipeline.PrepareUpload(r.Context(), who, contentIOBusiness.ImportRequest{
		Filename: body.Filename, MediaType: body.MediaType, ParentUUID: body.ParentUUID, Size: body.Size,
	})
	if err != nil {
		fail(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"job": job, "upload": upload})
}

// StartImport queues an import whose file has been uploaded.
//
// POST /doc/import/{job_id}/start → 202 {job}
func StartImport(w http.ResponseWriter, r *http.Request) {
	who, _, ok := whoIs(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	job, err := contentIOBusiness.Pipeline.StartUploaded(r.Context(), who, chi.URLParam(r, "job_id"))
	if err != nil {
		fail(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"job": job})
}

// GetJob is one of the caller's jobs.
//
// GET /content-io/jobs/{job_id} → 200 {job}
func GetJob(w http.ResponseWriter, r *http.Request) {
	who, _, ok := whoIs(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	job, err := contentIOBusiness.Pipeline.Job(r.Context(), who, chi.URLParam(r, "job_id"))
	if err != nil {
		fail(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"job": job})
}

// GetCapabilities is what imports and exports can do for the caller: whether
// the workspace lets them export, the formats, the size limits.
//
// GET /content-io/capabilities → 200 {capabilities}
func GetCapabilities(w http.ResponseWriter, r *http.Request) {
	who, u, ok := whoIs(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	c := contentIOBusiness.Pipeline.Capabilities(r.Context(), who, u.UserPostgresInfo.IsAdmin)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"capabilities": c})
}

type exportBody struct {
	DocUUID string `json:"doc_uuid"`
	Format  string `json:"format"`
}

func readExportBody(w http.ResponseWriter, r *http.Request) (exportBody, bool) {
	var body exportBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil || body.DocUUID == "" || body.Format == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which doc, and the format."})
		return body, false
	}
	body.Format = strings.ToLower(strings.TrimSpace(body.Format))
	return body, true
}

// httpTarget writes an export as a download.
type httpTarget struct {
	w       http.ResponseWriter
	started bool
}

func (t *httpTarget) Start(filename, mediaType string) io.Writer {
	t.started = true
	t.w.Header().Set("Content-Type", mediaType)
	t.w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	t.w.Header().Set("Cache-Control", "no-store")
	t.w.WriteHeader(http.StatusOK)
	return t.w
}

// ExportDoc writes a doc, as last saved, as Markdown, HTML or JSON. The
// workspace's export policy guards the route; this records the export.
//
// POST /doc/export {doc_uuid, format} → 200, the file
func ExportDoc(w http.ResponseWriter, r *http.Request) {
	who, _, ok := whoIs(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	body, ok := readExportBody(w, r)
	if !ok {
		return
	}
	to := &httpTarget{w: w}
	err := contentIOBusiness.Pipeline.Export(r.Context(), who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: body.DocUUID}, body.Format, to)
	if err != nil {
		if to.started {
			helpers.LogErrorWithContext(r.Context(), "content io: export of %s ended early: %v", body.DocUUID, err)
			return
		}
		if errors.Is(err, contentIOBusiness.ErrUnsupported) {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": fmt.Sprintf("The server exports md, html and json, not %q; PDF and Word are made in the app.", body.Format)})
			return
		}
		fail(w, err)
	}
}

// RecordExport records an export the browser made, after the same checks as
// the server's own: the workspace's export policy (on the route) and the
// caller's access to the doc.
//
// POST /doc/recordExport {doc_uuid, format} → 204
func RecordExport(w http.ResponseWriter, r *http.Request) {
	who, _, ok := whoIs(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	body, ok := readExportBody(w, r)
	if !ok {
		return
	}
	err := contentIOBusiness.Pipeline.RecordExport(r.Context(), who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: body.DocUUID}, body.Format)
	if err != nil {
		if errors.Is(err, contentIOBusiness.ErrUnsupported) {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Not a format docs export to."})
			return
		}
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
