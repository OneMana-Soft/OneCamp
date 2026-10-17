package controllers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	fileBusiness "github.com/akashc777/OneCamp/business/User"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/go-chi/chi/v5"
)

// Seams for tests: the grant, the attachment and the address it is served at.
var (
	guestGrant = func(ctx context.Context, token, kind string) (*guestModel.GuestGrant, error) {
		return guestBusiness.ValidateResourceGrant(ctx, token, kind)
	}
	guestAttachmentRow = func(ctx context.Context, objUUID, srcKey string) (*attachmentModels.Attachment, error) {
		return attachmentBusiness.GetAttachmentByObjUUID(ctx, objUUID, srcKey)
	}
	guestAttachmentURL = fileBusiness.GetFileURLByObjectName
)

// GuestDocAttachment GET /guest/doc-attachment/{token}/{obj_uuid}: a picture
// or file in a doc shared through a guest link, for that guest (docs
// programme B4; how-docs-work-backend B13). A guest has no session, so the
// member route answered 401 and every image in a shared doc was broken.
var GuestDocAttachment = guestAttachment("doc", postgressStruct.ATTACHMENT_SRC_DOC, "GuestDocAttachment")

// guestAttachment serves a file in a resource shared through a guest link (a
// board's image, a doc's picture or file) to that guest. The link is the
// credential, checked as every guest read checks it (ValidateResourceGrant: a
// link that is off, revoked, expired, or to something in the trash opens
// nothing), and the file must belong to the link's own resource: a guest
// can't reach another one's files by guessing an object id. Every refusal is
// the uniform "no longer available", so nothing can be learnt by probing; a
// read that failed is a 503 the page retries. The answer is a redirect to a
// signed, short-lived address, never kept by a cache. It isn't rate-limited:
// a doc or a board can hold many pictures, and the IP limiter doesn't count
// GETs.
func guestAttachment(kind, srcKey, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		objUUID := chi.URLParam(r, "obj_uuid")
		if objUUID == "" {
			notAvailable(w)
			return
		}
		grant, err := guestGrant(ctx, chi.URLParam(r, "token"), kind)
		if err != nil {
			guestReadFailed(w, r, name, err, "")
			return
		}
		att, err := guestAttachmentRow(ctx, objUUID, srcKey)
		if errors.Is(err, sql.ErrNoRows) {
			notAvailable(w)
			return
		}
		if err != nil {
			guestReadFailed(w, r, name, err, "")
			return
		}
		if att == nil || att.SrcKey != srcKey || att.SrcValue != grant.ResourceID {
			notAvailable(w)
			return
		}
		url, err := guestAttachmentURL(ctx, att.ObjKey)
		if err != nil {
			guestReadFailed(w, r, name, err, "")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		http.Redirect(w, r, url, http.StatusTemporaryRedirect)
	}
}
