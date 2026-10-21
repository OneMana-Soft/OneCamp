package controllers

// Publishing a doc's "Anyone with the link" grant as a web page (docs C6,
// D-25): the same grant, given a readable address. Who may: whoever may share
// the doc outside the workspace (its owner, the guest link rule), while guest
// access and its publishing sub-option are on; never the demo's shared
// visitor. Recorded as the grant's own audit, beside its create and revoke.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	publishBusiness "github.com/akashc777/OneCamp/business/Publish"
	"github.com/akashc777/OneCamp/helpers"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// demoPublishMsg is why the demo's shared visitor can't put a page on the web.
const demoPublishMsg = "Everyone who opens the demo shares this account, so it can't put pages on the web. " +
	"Install OneCamp free to publish your own."

// Seams for tests: the publication, and the grant a collab exchange names.
var (
	publishLink   = guestBusiness.PublishLink
	unpublishLink = guestBusiness.UnpublishLink
	grantOfLink   = guestModel.GetByID
	collabGrant   = guestBusiness.ValidateCollabGrant
)

// linkView is a live link as its sharers see it, with its page when published.
func linkView(g *guestModel.GuestGrant, mine bool) GuestLinkView {
	v := GuestLinkView{ID: g.Id, Capability: g.Capability, ExpiresAt: g.ExpiresAt, CreatedAt: g.CreatedAt, Mine: mine,
		Indexable: g.Indexable, AllowCopies: g.AllowCopies, HasPassword: g.PasswordHash != nil}
	if g.Slug != nil {
		ref := publishBusiness.Ref{Slug: *g.Slug}
		v.Slug, v.URL, v.PageURL = *g.Slug, publishBusiness.WebAddress(ref, ""), publishBusiness.PageAddress(ref)
	}
	return v
}

// publishable is the link a publishing request names, for someone who may
// share what it opens; it answers, and reports false, otherwise.
func publishable(w http.ResponseWriter, r *http.Request) (userModels.UserInfo, *guestModel.GuestGrant, bool) {
	userInfo, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteUnauthorized(w)
		return userInfo, nil, false
	}
	if helpers.RefuseDemoVisitor(w, userInfo.UserPostgresInfo.EmailID, demoPublishMsg) {
		return userInfo, nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a link."})
		return userInfo, nil, false
	}
	g, err := grantOfLink(r.Context(), id)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Guest/publishable %s: %+v", id, err)
		helpers.WriteRetryLater(w, "Couldn't read the link. Try again in a moment.")
		return userInfo, nil, false
	}
	if g == nil || g.RevokedAt != nil || g.ResourceType == guestModel.ResourceMeeting {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That link isn't here any more."})
		return userInfo, nil, false
	}
	if !mayShare(w, r, userInfo, g.ResourceType, g.ResourceID, false) {
		return userInfo, nil, false
	}
	return userInfo, g, true
}

func publishFailed(w http.ResponseWriter, r *http.Request, where string, err error) {
	var invalid *guestBusiness.PublishInvalid
	switch {
	case errors.As(err, &invalid):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": invalid.Reason})
	case errors.Is(err, guestBusiness.ErrGuestDisabled):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Guest access is off for this workspace.", "status": "guest_off"})
	case errors.Is(err, guestBusiness.ErrPublishOff):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "This workspace doesn't let pages be published to the web. Ask an admin.", "status": "publish_off"})
	case errors.Is(err, guestBusiness.ErrNotPublishable):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Only a doc's view-only link can be published. A published page is read-only."})
	case errors.Is(err, guestBusiness.ErrSlugTaken):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "Another page has that address. Try another one.", "status": "slug_taken"})
	case errors.Is(err, guestBusiness.ErrInvalidGrant), errors.Is(err, guestBusiness.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That link isn't here any more."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Guest/%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "That couldn't be saved. Try again in a moment.")
	}
}

// PublishGuestLink PUT /guest/links/{id}/publish — publishes a doc's view
// link as a web page, or changes how: its address, search engines, copies,
// its password.
func PublishGuestLink(w http.ResponseWriter, r *http.Request) {
	userInfo, g, ok := publishable(w, r)
	if !ok {
		return
	}
	var in guestBusiness.PublishSettings
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That couldn't be read."})
		return
	}
	saved, err := publishLink(r.Context(), userInfo.UserPostgresInfo.Id, g.Id, in)
	if err != nil {
		publishFailed(w, r, "PublishGuestLink", err)
		return
	}
	summary := "Published a page to the web"
	if g.Slug != nil {
		summary = "Changed how a page is published to the web"
	}
	auditBusiness.Record(r, "guest.link.publish", auditBusiness.CategorySecurity, summary, map[string]interface{}{
		"resource_type": saved.ResourceType, "resource_id": saved.ResourceID, "grant_id": saved.Id.String(),
		"slug": *saved.Slug, "indexable": saved.Indexable, "allow_copies": saved.AllowCopies, "password": saved.PasswordHash != nil,
	})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": linkView(saved, saved.CreatedBy == userInfo.UserPostgresInfo.Id)})
}

// UnpublishGuestLink DELETE /guest/links/{id}/publish — takes a link's page
// off the web at once; the link's token keeps working until it is turned off.
func UnpublishGuestLink(w http.ResponseWriter, r *http.Request) {
	userInfo, g, ok := publishable(w, r)
	if !ok {
		return
	}
	saved, err := unpublishLink(r.Context(), g.Id)
	if err != nil {
		publishFailed(w, r, "UnpublishGuestLink", err)
		return
	}
	slug := ""
	if g.Slug != nil {
		slug = *g.Slug
	}
	auditBusiness.Record(r, "guest.link.unpublish", auditBusiness.CategorySecurity, "Took a page off the web", map[string]interface{}{
		"resource_type": g.ResourceType, "resource_id": g.ResourceID, "grant_id": g.Id.String(), "slug": slug,
	})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": linkView(saved, saved.CreatedBy == userInfo.UserPostgresInfo.Id)})
}
