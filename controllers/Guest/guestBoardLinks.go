package controllers

// A board link's change by its manager, and its guest's comment (miro-33
// HEAD A). Who may do what is in business/Guest/boardLinks.go.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ChangeResourceGuestLink PATCH /guest/links/{id}: a board's owner or
// co-owner changes a live link's role, password or end. Its guests' live
// sessions end, so each tab is authorised afresh. Audited, never the password.
func ChangeResourceGuestLink(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a link."})
		return
	}
	var body struct {
		Capability   *string `json:"capability"`
		Password     *string `json:"password"`
		TTLHours     *int    `json:"ttl_hours"`
		NeverExpires bool    `json:"never_expires"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}
	g, err := guestModel.GetByID(r.Context(), id)
	if err != nil || g == nil || g.ResourceType != guestModel.ResourceBoard || g.RevokedAt != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That link isn't here any more."})
		return
	}
	if !mayShare(w, r, userInfo, g.ResourceType, g.ResourceID, false) {
		return
	}
	in := guestBusiness.BoardLinkChange{Capability: body.Capability, Password: body.Password, NeverExpires: body.NeverExpires}
	if body.TTLHours != nil {
		ttl := time.Duration(*body.TTLHours) * time.Hour
		in.TTL = &ttl
	}
	saved, err := guestBusiness.ChangeBoardLink(r.Context(), id, in)
	var invalid *guestBusiness.PublishInvalid
	switch {
	case errors.As(err, &invalid):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": invalid.Reason})
		return
	case errors.Is(err, guestBusiness.ErrNotABoardLink):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That link isn't here any more."})
		return
	case errors.Is(err, guestBusiness.ErrLinkChangedMeanwhile):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "Someone changed this link just now. Look at it again, then make your change."})
		return
	case errors.Is(err, guestBusiness.ErrGuestDisabled):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Guest access is disabled for this workspace."})
		return
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/Guest/ChangeResourceGuestLink err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't change the link. Try again."})
		return
	}
	details := map[string]interface{}{"resource_type": saved.ResourceType, "resource_id": saved.ResourceID, "grant_id": id.String(),
		"capability": saved.Capability, "password": saved.PasswordHash != nil, "expires_at": saved.ExpiresAt}
	auditBusiness.Record(r, "guest.link.change", auditBusiness.CategorySecurity, "Changed an external share link", details)
	recordItemLinkRow(r, linkChanged, saved.ResourceType, saved.ResourceID, id)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": linkView(saved, saved.CreatedBy == userInfo.UserPostgresInfo.Id)})
}

// GuestBoardComment POST /guest/board-comment: a board link's guest comments,
// replies or deletes their own message, as the live token the server signed
// names them (guest-<grant id>, "Guest: <name>"). Whatever the body says of
// an author is never read. The link is checked as every live connection is.
func GuestBoardComment(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	var body struct {
		BoardID   string   `json:"board_id"`
		Action    string   `json:"action"`
		CommentID string   `json:"comment_id"`
		MessageID string   `json:"message_id"`
		X         *float64 `json:"x"`
		Y         *float64 `json:"y"`
		Text      string   `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteRefusal(w, boardBusiness.ErrCommentInput)
		return
	}
	guest, err := guestBusiness.AuthorizeBoardGuest(r.Context(), bearer, body.BoardID)
	switch {
	case err == nil:
	case guestBusiness.IsUnavailable(err) || errors.Is(err, guestBusiness.ErrInvalidGrant):
		helpers.WriteUnauthorized(w)
		return
	default:
		guestReadFailed(w, r, "GuestBoardComment", err, "")
		return
	}
	if !guest.CanComment() {
		helpers.WriteForbidden(w, "This link lets you view the board, not comment on it.")
		return
	}
	answer, err := boardBusiness.CommentAsGuest(r.Context(), guest.Grant.ResourceID, guest.ID, guest.Name, boardBusiness.BoardCommentRequest{
		Action: body.Action, CommentID: body.CommentID, MessageID: body.MessageID, X: body.X, Y: body.Y, Text: body.Text,
	})
	var refused boardBusiness.Refusal
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Success", "data": answer})
	case errors.As(err, &refused):
		helpers.WriteForbidden(w, refused.Error())
	case errors.Is(err, boardBusiness.ErrBoardNotFound):
		notAvailable(w)
	case helpers.WriteRefusal(w, err):
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Guest/GuestBoardComment err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't comment just now. Try again."})
	}
}

// lockedRefused answers a password link's refusal (guestBusiness.OpenLocked)
// in the docs' page words (C6), true when err was one.
func lockedRefused(w http.ResponseWriter, err error) bool {
	for _, l := range lockedAnswers {
		if errors.Is(err, l.err) {
			helpers.WriteJSON(w, l.code, helpers.Envolope{"msg": l.msg, "status": l.status})
			return true
		}
	}
	return false
}

// lockedAnswers are a password link's refusals: the page's (C6) 401 and 429,
// with a status word the guest's page reads.
var lockedAnswers = []struct {
	err         error
	code        int
	msg, status string
}{
	{guestBusiness.ErrPasswordNeeded, http.StatusUnauthorized, "This link needs its password.", "password_needed"},
	{guestBusiness.ErrWrongPassword, http.StatusUnauthorized, "That password doesn't open this link.", "wrong_password"},
	{guestBusiness.ErrTooManyTries, http.StatusTooManyRequests, "Too many tries. Wait a few minutes and try again.", "rate_limited"},
}
