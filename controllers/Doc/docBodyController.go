package docController

// Writes of a doc's body (docs programme A4).
//
// POST /doc/updateDoc with a body writes it through the Live Doc Writer, as one
// transaction on the live copy, keeping a version of what it replaced first. It
// used to write doc_body underneath the copy people had open: the next save undid
// it, and once the doc was unloaded a tab still holding the old copy doubled it.
//
// POST /docColab/updateDoc is the collaboration service's save (business/Doc
// SaveFromCollab): one upsert, and side effects only when the body changed.
//
// Route swaps at A4's wiring: these replace UpdateDocBody and UpdateDocFromCollab
// in docController.go (router/docsVersions.go registers them).
//
// Its doc_private and doc_public_comment (the share dialog's General access)
// are the doc's general access: changed as every change to its sharing is
// (accessBusiness.Apply), by its owner or someone with full access, so the
// live sockets of the people who lose it are closed (I10) and its indexes
// are written again (I13). Apply goes first, so a change it refuses leaves
// the doc as it was.

import (
	"encoding/json"
	"errors"
	"net/http"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// UpdateDocBodyLive is POST /doc/updateDoc: the body through the live doc, the
// title as before, and who may open it or comment on it through the Access
// Resolver's Apply, refused as the sharing panel refuses it (accessFailed).
func UpdateDocBodyLive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in adapter.InputUpdateDoc
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req"})
		return
	}
	userInfo, who := accessBusiness.RequestSubject(r)
	if _, err := uuid.Parse(in.DocId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid doc id"})
		return
	}
	// Whether they may edit it is decided before anything about it is said: a
	// doc they can't open answers as one that isn't there (404), one in the
	// trash as deleted to the people it was there for (410), one they may only
	// read 403, and a hard lock says so (423); a decision that couldn't be
	// made is a read that failed (503).
	if _, err := accessBusiness.Open(ctx, who, docRes(in.DocId), accessBusiness.LevelEdit); err != nil {
		treeFailed(w, r, "UpdateDocBodyLive", err)
		return
	}
	access, err := business.GetDgraphDocByUUIDOnlyEditingInfo(ctx, in.DocId, userInfo.UserDgraphInfo.Uid)
	if err == nil && access == nil {
		err = business.ErrNotFound
	}
	if err != nil {
		helpers.WriteDocReadFailure(w, r, err, "controllers/UpdateDocBodyLive")
		return
	}
	// Deleted since the decision: as every write to the trash.
	if business.IsDeleted(access) {
		docGone(w, true)
		return
	}
	if in.IsPrivate != nil || in.PublicComment != nil {
		// Who can open it is full access's to say, sent changed or not.
		if _, err := accessBusiness.Open(ctx, who, docRes(in.DocId), accessBusiness.LevelFull); err != nil {
			if errors.Is(err, accessBusiness.ErrUndecided) {
				helpers.WriteDocReadFailure(w, r, err, "controllers/UpdateDocBodyLive")
				return
			}
			accessFailed(ctx, w, "UpdateDocBodyLive", err)
			return
		}
		general := generalChange(access, in)
		in.IsPrivate, in.PublicComment = nil, nil
		if general != nil {
			if refuseDemo(ctx, w, userInfo, in.DocId) {
				return
			}
			if _, err := accessBusiness.Apply(ctx, who, *general); err != nil {
				accessFailed(ctx, w, "UpdateDocBodyLive", err)
				return
			}
		}
	}

	data := helpers.Envolope{}
	if in.Body != nil {
		res, err := business.ReplaceBodyLive(ctx, in.DocId, *in.Body, collab.By{UserUUID: userInfo.UserDgraphInfo.Uuid}, "")
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/UpdateDocBodyLive live write failed err: %+v", err)
			if errors.Is(err, collab.ErrDocInTrash) {
				// Deleted after the access check: as every write to the trash.
				docGone(w, true)
				return
			}
			status, msg := liveWriteStatus(err)
			helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
			return
		}
		data["persisted"] = res.Persisted
		data["version_id"] = res.VersionID
		in.Body = nil
	}
	if in.Title != nil {
		err := business.UpdateDoc(ctx, &in)
		if docMissing(w, err) {
			return
		}
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/UpdateDocBodyLive update failed err: %+v", err)
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to update doc"})
			return
		}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated docInfo successfully!", "data": data})
}

// generalChange is the change to a doc's general access that /doc/updateDoc's
// doc_private and doc_public_comment ask for: the general access the doc's
// stored settings make with them in place, as A5 makes one change to its
// sharing. nil when it is the one the doc has: a dialog sending what is there
// already changes nothing, and tells nothing (I15). doc is read for the
// person. Pure.
func generalChange(doc *dgraphStruct.DgraphDoc, in adapter.InputUpdateDoc) *accessBusiness.Change {
	after := *doc
	if in.IsPrivate != nil {
		after.IsPrivate = in.IsPrivate
	}
	if in.PublicComment != nil {
		after.PublicComment = in.PublicComment
	}
	g := business.GeneralOf(&after)
	if g == business.GeneralOf(doc) {
		return nil
	}
	return &accessBusiness.Change{Resource: docRes(doc.Uuid), General: &g.Level, Private: &g.Private}
}

// liveWriteStatus is the answer for a live write the service did not make.
func liveWriteStatus(err error) (int, string) {
	switch {
	case errors.Is(err, collab.ErrNoSuchDoc):
		return http.StatusNotFound, helpers.DocNotFound
	case errors.Is(err, collab.ErrRefused):
		var op *collab.OpError
		if errors.As(err, &op) {
			return op.Status, "The doc can't take that: " + op.Msg
		}
		return http.StatusBadRequest, "The doc can't take that."
	}
	return http.StatusServiceUnavailable, "The doc can't be written just now. Try again in a moment."
}

// SaveDocFromCollab is POST /docColab/updateDoc, the collaboration service's
// save. It answers {stored, changed}; the service sends the whole body when a
// state-only save comes back unstored.
func SaveDocFromCollab(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in business.CollabSave
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req"})
		return
	}
	saved, err := business.SaveFromCollab(ctx, in)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SaveDocFromCollab doc %s err: %+v", in.DocUUID, err)
		// The service retries what may pass (5xx) and gives up on a refusal
		// (4xx): collaboration-service saveFailures.js isTransient.
		status := http.StatusServiceUnavailable
		switch {
		case errors.Is(err, business.ErrUnsafeCollabBody):
			status = http.StatusUnprocessableEntity
		case errors.Is(err, business.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, business.ErrDocDeleted):
			status = http.StatusGone
		case errors.Is(err, business.ErrNotAPage):
			// A folder, a file or a link has no body (docs F1): a refusal,
			// which the service gives up on.
			status = http.StatusConflict
		}
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": "Failed to save the doc"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "saved", "data": saved})
}
