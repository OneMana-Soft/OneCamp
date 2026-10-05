// Package controllers (Guest) exposes the guest-access surface:
//
//   - member (authed):  POST /meet/instant            → start a shareable meeting
//   - admin:            POST /admin/guest-access       → toggle workspace policy
//     GET  /admin/guest-grants       → list active grants
//     POST /admin/guest-grants/{id}/revoke
//   - public (no auth): GET  /guest/meet/{token}       → link status (no oracle)
//     POST /guest/meet/{token}/join  → mint single-room token
//
// The public endpoints never touch member auth, never set a session cookie, and
// map every "not available" reason (disabled / invalid / expired / revoked) to a
// single 404 so a guest cannot distinguish them.
package controllers

import (
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	tableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	fileBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// notAvailable is the single response for every guest-link failure (no oracle).
func notAvailable(w http.ResponseWriter) {
	helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{
		"msg":       "This link is no longer available.",
		"available": false,
	})
}

// CreateInstantMeeting POST /meet/instant — member starts a shareable meeting.
func CreateInstantMeeting(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if !settingsBusiness.GuestAccessEnabled() {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Guest access is disabled for this workspace."})
		return
	}

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	var body struct {
		AudioEnabled bool `json:"audio_enabled"`
		VideoEnabled bool `json:"video_enabled"`
	}
	// Body is optional; default audio/video off (the room UI toggles devices).
	_ = json.NewDecoder(r.Body).Decode(&body)

	meeting, err := guestBusiness.CreateInstantMeeting(ctx, &userInfo.UserDgraphInfo, body.AudioEnabled, body.VideoEnabled)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Guest/CreateInstantMeeting err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to start meeting"})
		return
	}

	auditBusiness.Record(r, "guest.meeting.create", auditBusiness.CategorySecurity,
		"Started an instant meeting with a guest link",
		map[string]interface{}{"room": meeting.Room, "grant_id": meeting.GrantID.String()})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"room":        meeting.Room,
		"host_token":  meeting.HostToken,
		"guest_token": meeting.GuestToken, // raw link token, shown once
		"grant_id":    meeting.GrantID.String(),
		"expires_at":  meeting.ExpiresAt,
	}})
}

// GetGuestMeeting GET /guest/meet/{token} — public link status. Returns 200
// {available:true} for a usable link, else a uniform 404.
func GetGuestMeeting(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")

	if _, err := guestBusiness.ValidateMeetingGrant(ctx, token); err != nil {
		notAvailable(w)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"available": true}})
}

// JoinGuestMeeting POST /guest/meet/{token}/join — mint a single-room guest
// token after collecting a display name.
func JoinGuestMeeting(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")

	var body struct {
		DisplayName  string `json:"display_name"`
		AudioEnabled bool   `json:"audio_enabled"`
		VideoEnabled bool   `json:"video_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}

	grant, err := guestBusiness.ValidateMeetingGrant(ctx, token)
	if err != nil {
		notAvailable(w)
		return
	}

	liveToken, room, err := guestBusiness.IssueGuestMeetingToken(ctx, grant, body.DisplayName, body.AudioEnabled, body.VideoEnabled)
	if err != nil {
		if errors.Is(err, guestBusiness.ErrInvalidName) {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Please enter a display name."})
			return
		}
		// ErrGuestDisabled or any other → uniform not-available.
		notAvailable(w)
		return
	}

	auditBusiness.Record(r, "guest.join", auditBusiness.CategorySecurity,
		"A guest joined a meeting",
		map[string]interface{}{"room": room, "grant_id": grant.Id.String()})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"token": liveToken,
		"room":  room,
	}})
}

// SetGuestPolicy POST /admin/guest-access — toggle the workspace guest policy.
func SetGuestPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}
	if err := settingsBusiness.SetGuestAccessEnabled(body.Enabled); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Guest/SetGuestPolicy err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to update policy"})
		return
	}
	action := "guest.policy.disable"
	summary := "Disabled workspace guest access"
	if body.Enabled {
		action = "guest.policy.enable"
		summary = "Enabled workspace guest access"
	}
	auditBusiness.Record(r, action, auditBusiness.CategorySecurity, summary, nil)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"guest_access_enabled": body.Enabled}})
}

// ListGuestGrants GET /admin/guest-grants — active grants for the admin view.
func ListGuestGrants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	grants, err := guestBusiness.ListActiveGrants(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load guest grants"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": grants})
}

// RevokeGuestGrant POST /admin/guest-grants/{id}/revoke — revoke a grant.
func RevokeGuestGrant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid grant id"})
		return
	}
	if err := guestBusiness.RevokeGrant(ctx, id); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to revoke grant"})
		return
	}
	auditBusiness.Record(r, "guest.grant.revoke", auditBusiness.CategorySecurity,
		"Revoked a guest grant", map[string]interface{}{"grant_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Guest grant revoked"})
}

// ─── Phase 2: scoped doc/board guest links (live, read-only) ────────────

// CreateResourceGuestLink POST /guest/links — a member who can EDIT a doc or
// board mints a scoped, expiring, read-only external share link. Requiring edit
// (or ownership) keeps sharing a privileged action: a mere viewer can't
// re-share someone else's private resource outward.
func CreateResourceGuestLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if !settingsBusiness.GuestAccessEnabled() {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Guest access is disabled for this workspace."})
		return
	}
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	var body struct {
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
		Capability   string `json:"capability"` // "view" | "comment" (comment is doc-only)
		TTLHours     int    `json:"ttl_hours"`
		// NeverExpires makes a link that lasts until it is revoked. Opt-in and
		// never the default: expiry is the safe behaviour and stays the default
		// for anybody who does not ask.
		NeverExpires bool `json:"never_expires"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}

	// Verify the caller may share this resource: must have edit access or own
	// it. Reuses the same dgraph access projection the collab authorize uses.
	switch body.ResourceType {
	case "doc":
		d, err := docBusiness.GetDgraphDocByUUIDOnlyEditingInfo(ctx, body.ResourceID, userInfo.UserDgraphInfo.Uid)
		if err != nil || d == nil {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Document not found"})
			return
		}
		isOwner := d.CreatedBy != nil && d.CreatedBy.Uuid == userInfo.UserDgraphInfo.Uuid
		if d.HasEditAccess == 0 && !isOwner {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You need edit access to share this document."})
			return
		}
	case "board":
		b, err := boardBusiness.GetBasicBoardByUUID(ctx, body.ResourceID, userInfo.UserDgraphInfo.Uid)
		if err != nil || b == nil {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Board not found"})
			return
		}
		isOwner := b.CreatedBy != nil && b.CreatedBy.Uuid == userInfo.UserDgraphInfo.Uuid
		if b.HasEditAccess == 0 && !isOwner {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You need edit access to share this board."})
			return
		}
	case "table":
		tid, perr := uuid.Parse(body.ResourceID)
		if perr != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid table id"})
			return
		}
		// The caller must be able to MANAGE the table to share it externally.
		// GetBundle enforces view access; CanManage gates the privileged share.
		bundle, berr := tableBusiness.GetBundle(ctx, tid, tableBusiness.Actor{
			UserID:  userInfo.UserPostgresInfo.Id,
			IsAdmin: userInfo.UserPostgresInfo.IsAdmin,
		})
		if berr != nil {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Table not found"})
			return
		}
		if !bundle.CanManage {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You need manage access to share this table."})
			return
		}
	case "channel":
		// Inviting someone from outside into a channel is the channel admins'
		// call (or a workspace admin's), as adding a member is.
		cid, perr := uuid.Parse(body.ResourceID)
		if perr != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid channel id"})
			return
		}
		ch, cerr := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, cid, userInfo.UserDgraphInfo.Uid)
		if cerr != nil || ch == nil || ch.Uuid == "" {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Channel not found"})
			return
		}
		if ch.IsAdmin == 0 && !userInfo.UserPostgresInfo.IsAdmin {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the channel's admins can invite guests."})
			return
		}
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Unsupported resource type"})
		return
	}

	grant, err := guestBusiness.CreateResourceGrant(ctx, userInfo.UserPostgresInfo.Id, body.ResourceType, body.ResourceID, body.Capability, time.Duration(body.TTLHours)*time.Hour, body.NeverExpires)
	if err != nil {
		if errors.Is(err, guestBusiness.ErrGuestDisabled) {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Guest access is disabled for this workspace."})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/Guest/CreateResourceGuestLink err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to create share link"})
		return
	}

	auditBusiness.Record(r, "guest.link.create", auditBusiness.CategorySecurity,
		"Created an external share link",
		map[string]interface{}{"resource_type": grant.ResourceType, "resource_id": grant.ResourceID, "grant_id": grant.GrantID.String()})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"token":         grant.Token, // raw link token, shown once
		"grant_id":      grant.GrantID.String(),
		"resource_type": grant.ResourceType,
		"resource_id":   grant.ResourceID,
		"capability":    grant.Capability,
		"expires_at":    grant.ExpiresAt,
	}})
}

// GuestCollabToken POST /guest/collab/{token} — public. Exchanges a raw share
// link token (+ optional display name) for a short-lived collab JWT the guest's
// browser uses to connect to the collaboration service read-only. Returns the
// fully-formed Hocuspocus document name so the client connects without needing
// to know the board: prefix convention.
func GuestCollabToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")

	var body struct {
		DisplayName string `json:"display_name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // body optional

	grant, err := guestBusiness.ValidateCollabGrant(ctx, token)
	if err != nil {
		notAvailable(w)
		return
	}

	collabToken, err := guestBusiness.IssueGuestCollabToken(ctx, grant, body.DisplayName)
	if err != nil {
		notAvailable(w)
		return
	}

	documentName := grant.ResourceID
	if grant.ResourceType == "board" {
		documentName = "board:" + grant.ResourceID
	}

	// R4.4: record the guest access (resource + time), content-free and with no
	// member actor. Display name is whatever the guest supplied (already only
	// used for the collab awareness label, never resolved to a member).
	auditBusiness.Record(r, "guest.resource.access", auditBusiness.CategorySecurity,
		"A guest opened a shared "+grant.ResourceType,
		map[string]interface{}{
			"resource_type": grant.ResourceType,
			"resource_id":   grant.ResourceID,
			"grant_id":      grant.Id.String(),
			"display_name":  guestBusiness.SanitizeGuestName(body.DisplayName),
		})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"collab_token":  collabToken,
		"document_name": documentName,
		"resource_type": grant.ResourceType,
		"resource_id":   grant.ResourceID,
		"capability":    grant.Capability,
	}})
}

// GuestDocColabAuthorize POST /docColab/guestAuthorize — called server-to-server
// by the collaboration service when a connection presents a guest token. Public
// (no member auth); the guest JWT is verified and the grant re-validated here.
// Always read-only.
func GuestDocColabAuthorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	var body struct {
		DocUUID string `json:"doc_uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}
	if _, err := guestBusiness.AuthorizeGuestCollab(ctx, bearer, "doc", body.DocUUID); err != nil {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Unauthorized"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Authorised", "canEdit": false})
}

// GuestBoardColabAuthorize POST /boardColab/guestAuthorize — board variant.
func GuestBoardColabAuthorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	var body struct {
		BoardUUID string `json:"board_uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}
	if _, err := guestBusiness.AuthorizeGuestCollab(ctx, bearer, "board", body.BoardUUID); err != nil {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Unauthorized"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Authorised", "canEdit": false})
}

// GuestBoardAttachment GET /guest/board-attachment/{token}/{obj_uuid} — public.
// Serves a board image to a guest, authorized by the share-link grant rather
// than a member session. The attachment must belong to the grant's board, so a
// board grant can't be used to fetch another board's images. Presigned redirect
// (no raw bytes proxied).
func GuestBoardAttachment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")
	objUUID := chi.URLParam(r, "obj_uuid")
	if objUUID == "" {
		notAvailable(w)
		return
	}

	grant, err := guestBusiness.ValidateResourceGrant(ctx, token, "board")
	if err != nil {
		notAvailable(w)
		return
	}

	att, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, objUUID, postgressStruct.ATTACHMENT_SRC_BOARD)
	if err != nil {
		notAvailable(w)
		return
	}
	if att.SrcKey != postgressStruct.ATTACHMENT_SRC_BOARD || att.SrcValue != grant.ResourceID {
		notAvailable(w)
		return
	}

	url, err := fileBusiness.GetFileURLByObjectName(ctx, att.ObjKey)
	if err != nil {
		notAvailable(w)
		return
	}
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

// GuestTable GET /guest/table/{token} — public. Returns the read-only table
// bundle (header + fields + rows) for a guest, authorized by the share-link
// grant rather than a member session. Tables are Postgres-backed (not in the
// collaboration service), so this is a direct read; the bundle carries no MQTT
// topic, so the guest UI never attempts a member-only live subscription.
func GuestTable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")

	grant, err := guestBusiness.ValidateResourceGrant(ctx, token, "table")
	if err != nil {
		notAvailable(w)
		return
	}
	id, err := uuid.Parse(grant.ResourceID)
	if err != nil {
		notAvailable(w)
		return
	}
	bundle, err := tableBusiness.GetGuestBundle(ctx, id)
	if err != nil {
		notAvailable(w)
		return
	}
	// R4.4: record the guest table access (resource + time), no member actor.
	auditBusiness.Record(r, "guest.resource.access", auditBusiness.CategorySecurity,
		"A guest opened a shared table",
		map[string]interface{}{
			"resource_type": "table",
			"resource_id":   grant.ResourceID,
			"grant_id":      grant.Id.String(),
		})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": bundle})
}

// ─── Phase 2: guest doc comments (capability = comment) ─────────────────

// guestCommentItem is the wire shape for one guest comment in the guest viewer.
// Body is plain text; the FE escapes on render.
type guestCommentItem struct {
	ID        string    `json:"id"`
	GuestName string    `json:"guest_name"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// GuestDocComments GET /guest/doc-comments/{token} — public. Returns the guest
// feedback thread for the shared doc (guest comments only; the internal member
// comment thread is deliberately NOT exposed to an external guest). Any active
// doc grant may read its own guest thread; posting requires the comment
// capability (see CreateGuestDocComment).
func GuestDocComments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")

	grant, err := guestBusiness.ValidateResourceGrant(ctx, token, "doc")
	if err != nil {
		notAvailable(w)
		return
	}
	comments, err := guestBusiness.ListGuestDocComments(ctx, grant.ResourceID)
	if err != nil {
		notAvailable(w)
		return
	}
	out := make([]guestCommentItem, 0, len(comments))
	for _, c := range comments {
		out = append(out, guestCommentItem{
			ID:        c.ID.String(),
			GuestName: c.GuestName,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"capability": grant.Capability,
		"comments":   out,
	}})
}

// CreateGuestDocComment POST /guest/doc-comments/{token} — public. Posts a
// comment as the badged guest, after the business layer verifies the grant
// carries the comment capability for that doc. The body is HTML-stripped and
// length-capped server-side. On success the new comment is broadcast to any
// members currently viewing the doc (with a guest marker) so they see it live.
func CreateGuestDocComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")

	var body struct {
		DisplayName string `json:"display_name"`
		Body        string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}

	grant, err := guestBusiness.ValidateResourceGrant(ctx, token, "doc")
	if err != nil {
		notAvailable(w)
		return
	}

	comment, err := guestBusiness.CreateGuestDocComment(ctx, grant, body.DisplayName, body.Body)
	if err != nil {
		switch {
		case errors.Is(err, guestBusiness.ErrForbidden):
			// View-only grant trying to comment.
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "This link is view only."})
		case err.Error() == "empty comment":
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Please enter a comment."})
		default:
			notAvailable(w)
		}
		return
	}

	// Broadcast to members viewing the doc. Body is HTML-escaped here because
	// the doc comment UI renders body_text as HTML; a guest is untrusted, so
	// this prevents stored/reflected XSS in an authenticated member session.
	createdAt := comment.CreatedAt
	mqttGuestComment := mqttStruct.MqttDocComment{
		Type:        mqttStruct.TYPE_CREATE,
		DocUuid:     grant.ResourceID,
		CommentUuid: comment.ID.String(),
		CreatedAt:   &createdAt,
		HTMLText:    "<p>" + html.EscapeString(comment.Body) + "</p>",
		UserUuid:    "guest-" + grant.Id.String(),
		UserName:    "Guest: " + comment.GuestName,
		IsGuest:     true,
	}
	go mqttBusiness.PublishDocComment(&mqttGuestComment, grant.ResourceID)

	auditBusiness.Record(r, "guest.comment.create", auditBusiness.CategorySecurity,
		"A guest commented on a shared doc",
		map[string]interface{}{"resource_type": "doc", "resource_id": grant.ResourceID, "grant_id": grant.Id.String()})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": guestCommentItem{
		ID:        comment.ID.String(),
		GuestName: comment.GuestName,
		Body:      comment.Body,
		CreatedAt: comment.CreatedAt,
	}})
}

// ─── Channel guests (capability = view | post) ─────────────────────────

func channelGrant(w http.ResponseWriter, r *http.Request) (*guestModel.GuestGrant, bool) {
	grant, err := guestBusiness.ValidateResourceGrant(r.Context(), chi.URLParam(r, "token"), "channel")
	if err != nil {
		notAvailable(w)
		return nil, false
	}
	return grant, true
}

// GuestChannel GET /guest/channel/{token}?before=RFC3339 — public. A page of
// the shared channel's messages, newest first, as plain text.
func GuestChannel(w http.ResponseWriter, r *http.Request) {
	grant, ok := channelGrant(w, r)
	if !ok {
		return
	}
	before, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("before"))
	view, err := guestBusiness.GetGuestChannel(r.Context(), grant, before)
	if err != nil {
		notAvailable(w)
		return
	}
	if before.IsZero() {
		auditBusiness.Record(r, "guest.resource.access", auditBusiness.CategorySecurity,
			"A guest opened a shared channel",
			map[string]interface{}{"resource_type": "channel", "resource_id": grant.ResourceID, "grant_id": grant.Id.String()})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": view})
}

// GuestChannelThread GET /guest/channel/{token}/thread/{post_id} — public.
func GuestChannelThread(w http.ResponseWriter, r *http.Request) {
	grant, ok := channelGrant(w, r)
	if !ok {
		return
	}
	t, err := guestBusiness.GetGuestThread(r.Context(), grant, chi.URLParam(r, "post_id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message isn't here any more."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": t})
}

// GuestChannelPost POST /guest/channel/{token} {display_name, text, reply_to}
// — public. Posts as the guest, or replies in a thread.
func GuestChannelPost(w http.ResponseWriter, r *http.Request) {
	grant, ok := channelGrant(w, r)
	if !ok {
		return
	}
	var body struct {
		DisplayName string `json:"display_name"`
		Text        string `json:"text"`
		ReplyTo     string `json:"reply_to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}
	err := guestBusiness.PostAsGuest(r.Context(), grant, body.DisplayName, body.Text, body.ReplyTo)
	var in *guestBusiness.ErrGuestInput
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Sent"})
	case errors.As(err, &in):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": in.Msg})
	case errors.Is(err, guestBusiness.ErrForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "This link is read only."})
	case errors.Is(err, guestBusiness.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That message isn't here any more."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Guest/GuestChannelPost err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't send that. Try again."})
	}
}
