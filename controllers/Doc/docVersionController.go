package docController

// A doc's version history (docs programme A4, the Snapshot Service; B8 builds the
// history sheet on it):
//
//	GET  /doc/versions?doc_uuid=                  versions, newest first, and the people they credit
//	POST /doc/versions {doc_uuid, name, pinned}    keep the doc as it is now ("Save this version")
//	GET  /doc/versions/{version_id}?doc_uuid=     a version's body, to read beside the doc
//	POST /doc/versions/{version_id}/name {doc_uuid, name, pinned}
//	POST /doc/versions/{version_id}/restore {doc_uuid}   live, for everyone
//
// Each takes edit access to the doc, decided by the Access Resolver.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	snapshotBusiness "github.com/akashc777/OneCamp/business/Snapshot"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/go-chi/chi/v5"
)

type versionRequest struct {
	DocUUID string `json:"doc_uuid"`
	Name    string `json:"name"`
	Pinned  bool   `json:"pinned"`
}

// versionPerson is how a version names someone.
type versionPerson struct {
	Name       string `json:"name"`
	ProfileKey string `json:"profile_key,omitempty"`
}

// versionError answers a refusal as what it is.
func versionError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := http.StatusInternalServerError, "Couldn't reach this doc's history just now. Try again in a moment."
	switch {
	case errors.Is(err, snapshotBusiness.ErrNotFound):
		status, msg = http.StatusNotFound, "There's no such version."
	case errors.Is(err, snapshotBusiness.ErrForbidden):
		status, msg = http.StatusForbidden, "You need edit access to see this doc's history."
	case errors.Is(err, snapshotBusiness.ErrInTrash), errors.Is(err, collab.ErrDocInTrash):
		status, msg = http.StatusGone, "This doc is in the trash."
	case errors.Is(err, snapshotBusiness.ErrKeptFull):
		status, msg = http.StatusConflict, "This doc keeps as many named and pinned versions as it has room for. Unpin one first."
	case errors.Is(err, snapshotBusiness.ErrBadName):
		status, msg = http.StatusBadRequest, "A version's name is at most 120 characters."
	case errors.Is(err, collab.ErrUnavailable):
		status, msg = http.StatusServiceUnavailable, "The doc can't be restored just now. Try again in a moment."
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/docVersions err: %+v", err)
	}
	helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
}

// ListDocVersions is GET /doc/versions.
func ListDocVersions(w http.ResponseWriter, r *http.Request) {
	docUUID := r.URL.Query().Get("doc_uuid")
	_, who := accessBusiness.RequestSubject(r)
	list, err := snapshotBusiness.Snapshots.List(r.Context(), who, accessBusiness.KindDoc, docUUID)
	if err != nil {
		versionError(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": helpers.Envolope{
		"versions": list,
		"people":   peopleIn(r.Context(), list),
	}})
}

// peopleIn names everyone the versions credit, people and agents, in one read.
func peopleIn(ctx context.Context, list []snapshotBusiness.Version) map[string]versionPerson {
	seen := map[string]bool{"": true}
	var ids []string
	for _, v := range list {
		for _, c := range v.Contributors {
			for _, id := range []string{c.UserUUID, c.AgentUUID, c.AskerUUID} {
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	out := map[string]versionPerson{}
	if len(ids) == 0 {
		return out
	}
	resolved, err := userDomain.ResolveUserDisplays(ctx, ids)
	if err != nil {
		return out
	}
	for id, d := range resolved {
		name := d.Display
		if name == "" {
			name = d.FullName
		}
		if name == "" {
			name = d.Name
		}
		out[id] = versionPerson{Name: name, ProfileKey: d.ProfileKey}
	}
	return out
}

// TakeDocVersion is POST /doc/versions: keep the doc as it is now.
func TakeDocVersion(w http.ResponseWriter, r *http.Request) {
	var in versionRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req"})
		return
	}
	_, who := accessBusiness.RequestSubject(r)
	v, err := snapshotBusiness.Snapshots.Take(r.Context(), snapshotBusiness.Take{
		ResourceKind: accessBusiness.KindDoc, ResourceUUID: in.DocUUID, Name: in.Name, Pinned: in.Pinned,
		By: snapshotBusiness.Contributor{UserUUID: who.UserUUID}, Who: &who,
	})
	if err != nil {
		versionError(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": v})
}

// PreviewDocVersion is GET /doc/versions/{version_id}.
func PreviewDocVersion(w http.ResponseWriter, r *http.Request) {
	_, who := accessBusiness.RequestSubject(r)
	html, err := snapshotBusiness.Snapshots.Preview(r.Context(), who, accessBusiness.KindDoc, r.URL.Query().Get("doc_uuid"), chi.URLParam(r, "version_id"))
	if err != nil {
		versionError(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": helpers.Envolope{"html": html}})
}

// NameDocVersion is POST /doc/versions/{version_id}/name.
func NameDocVersion(w http.ResponseWriter, r *http.Request) {
	var in versionRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req"})
		return
	}
	_, who := accessBusiness.RequestSubject(r)
	if err := snapshotBusiness.Snapshots.Name(r.Context(), who, accessBusiness.KindDoc, in.DocUUID, chi.URLParam(r, "version_id"), in.Name, in.Pinned); err != nil {
		versionError(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success"})
}

// RestoreDocVersion is POST /doc/versions/{version_id}/restore.
func RestoreDocVersion(w http.ResponseWriter, r *http.Request) {
	var in versionRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req"})
		return
	}
	_, who := accessBusiness.RequestSubject(r)
	if err := snapshotBusiness.Snapshots.Restore(r.Context(), who, accessBusiness.KindDoc, in.DocUUID, chi.URLParam(r, "version_id")); err != nil {
		versionError(w, r, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Restored"})
}
