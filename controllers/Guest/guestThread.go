package controllers

// A doc's conversation with its guests, for its team, and a client's verdict
// on a doc (docs C5-2):
//
//	GET  /doc/guest-thread?doc_uuid=              the guests' comments with the team's replies, and what the reader may do
//	POST /doc/guest-thread/reply {doc_uuid, parent_id, body}   a reply the guest sees
//	POST /doc/guest-thread/remove {doc_uuid, comment_id}       a guest's comment off the doc (moderation; recorded)
//	GET  /doc/client-review?doc_uuid=             the doc's newest client verdict
//	POST /guest/doc-review/{token} {display_name, decision, note}   public: a client's verdict

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Seams for tests.
var (
	listGuestThread = guestBusiness.ListGuestDocComments
	replyToGuest    = guestBusiness.ReplyToGuest
	mayModerateDoc  = threadBusiness.CanModerateDoc
	removeGuestOne  = guestBusiness.RemoveGuestComment
	docDecisionOf   = func(r *http.Request, who accessBusiness.Subject, docUUID string) (accessBusiness.Decision, error) {
		return accessBusiness.Rules.Resolve(r.Context(), who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID})
	}
)

// threadRefused answers what the guest business layer refused, as itself.
func threadRefused(w http.ResponseWriter, r *http.Request, where string, err error) {
	var in *guestBusiness.ErrGuestInput
	switch {
	case errors.As(err, &in):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": in.Error()})
	case err.Error() == "empty comment":
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Write a reply first."})
	case errors.Is(err, guestBusiness.ErrGuestDisabled):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Guest access is off for this workspace."})
	case errors.Is(err, guestBusiness.ErrForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You can't reply to guests on this doc."})
	case errors.Is(err, guestBusiness.ErrNotFound), errors.Is(err, guestBusiness.ErrInvalidGrant):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That comment isn't here any more."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Guest/%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "That couldn't be done just now. Try again in a moment.")
	}
}

// GuestThread is GET /doc/guest-thread: for someone who may read the doc.
func GuestThread(w http.ResponseWriter, r *http.Request) {
	docUUID := r.URL.Query().Get("doc_uuid")
	_, who := accessBusiness.RequestSubject(r)
	d, err := docDecisionOf(r, who, docUUID)
	if err != nil || d.Deleted || d.Level < accessBusiness.LevelView {
		if err != nil && !errors.Is(err, guestBusiness.ErrNotFound) {
			helpers.LogErrorWithContext(r.Context(), "controllers/Guest/GuestThread decide %s: %+v", docUUID, err)
		}
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That doc isn't here."})
		return
	}
	comments, err := listGuestThread(r.Context(), docUUID)
	if err != nil {
		threadRefused(w, r, "GuestThread", err)
		return
	}
	out := make([]guestCommentItem, 0, len(comments))
	for _, c := range comments {
		out = append(out, guestCommentItemOf(c))
	}
	canModerate, _ := mayModerateDoc(r.Context(), who, docUUID)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{
		"comments":     out,
		"can_reply":    d.Level >= accessBusiness.LevelComment && who.AgentUUID == "",
		"can_moderate": canModerate,
	}})
}

// GuestThreadReply is POST /doc/guest-thread/reply.
func GuestThreadReply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DocUUID  string `json:"doc_uuid"`
		ParentID string `json:"parent_id"`
		Body     string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That couldn't be read."})
		return
	}
	parent, err := uuid.Parse(in.ParentID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That comment isn't here any more."})
		return
	}
	userInfo, who := accessBusiness.RequestSubject(r)
	name := userInfo.UserDgraphInfo.UserFullName
	if name == "" {
		name = userInfo.UserDgraphInfo.UserName
	}
	reply, err := replyToGuest(r.Context(), who, name, in.DocUUID, parent, in.Body)
	if err != nil {
		threadRefused(w, r, "GuestThreadReply", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": guestCommentItemOf(reply)})
}

// GuestThreadRemove is POST /doc/guest-thread/remove: for whoever may
// moderate the doc's comments; recorded.
func GuestThreadRemove(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DocUUID   string `json:"doc_uuid"`
		CommentID string `json:"comment_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That couldn't be read."})
		return
	}
	id, err := uuid.Parse(in.CommentID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That comment isn't here any more."})
		return
	}
	_, who := accessBusiness.RequestSubject(r)
	ok, err := mayModerateDoc(r.Context(), who, in.DocUUID)
	if err != nil {
		threadRefused(w, r, "GuestThreadRemove", err)
		return
	}
	if !ok {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the doc's owner, someone with full access or an admin can remove a guest's comment."})
		return
	}
	gone, err := removeGuestOne(r.Context(), in.DocUUID, id)
	if err != nil {
		threadRefused(w, r, "GuestThreadRemove", err)
		return
	}
	if !gone {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That comment isn't here any more."})
		return
	}
	auditBusiness.Record(r, "doc.guest_comment.remove", auditBusiness.CategorySecurity, "Removed a guest's comment from a doc",
		map[string]interface{}{"resource_type": "doc", "resource_id": in.DocUUID, "comment_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Removed"})
}

// DocClientReview is GET /doc/client-review: for someone who may read the doc.
func DocClientReview(w http.ResponseWriter, r *http.Request) {
	docUUID := r.URL.Query().Get("doc_uuid")
	_, who := accessBusiness.RequestSubject(r)
	d, err := docDecisionOf(r, who, docUUID)
	if err != nil || d.Deleted || d.Level < accessBusiness.LevelView {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That doc isn't here."})
		return
	}
	review, err := guestBusiness.LatestDocReview(docUUID)
	if err != nil {
		threadRefused(w, r, "DocClientReview", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"review": review}})
}

// ReviewGuestDoc is POST /guest/doc-review/{token}: a client's verdict on the
// doc their comment link opens. Public; rate-limited at the route.
func ReviewGuestDoc(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DisplayName string `json:"display_name"`
		Decision    string `json:"decision"`
		Note        string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That couldn't be read."})
		return
	}
	grant, err := guestBusiness.ValidateResourceGrant(r.Context(), chi.URLParam(r, "token"), "doc")
	if err != nil {
		guestReadFailed(w, r, "ReviewGuestDoc", err, "")
		return
	}
	review, err := guestBusiness.ReviewDocAsGuest(r.Context(), grant, in.DisplayName, in.Decision, in.Note)
	if err != nil {
		if errors.Is(err, guestBusiness.ErrForbidden) {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "This link is view only."})
			return
		}
		guestWriteFailed(w, r, "ReviewGuestDoc", err, "")
		return
	}
	auditBusiness.Record(r, "guest.doc.review", auditBusiness.CategorySecurity, "A client gave a verdict on a shared doc",
		map[string]interface{}{"resource_type": "doc", "resource_id": grant.ResourceID, "grant_id": grant.Id.String(), "decision": review.Decision})
	writeGuestRead(w, map[string]interface{}{"review": review})
}
