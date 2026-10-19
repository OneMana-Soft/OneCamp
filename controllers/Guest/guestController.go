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
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	tableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// recordGuestAccess writes the audit row for a guest opening a shared
// resource (what it is, which link, and the name the guest gave when there is
// one), when guestBusiness.AuditAccessDue says it is due.
func recordGuestAccess(r *http.Request, grant *guestModel.GuestGrant, what, guestName string) {
	if !guestBusiness.AuditAccessDue(r.Context(), grant.Id, guestName) {
		return
	}
	details := map[string]interface{}{
		"resource_type": grant.ResourceType,
		"resource_id":   grant.ResourceID,
		"grant_id":      grant.Id.String(),
	}
	if guestName != "" {
		details["display_name"] = guestName
	}
	auditBusiness.Record(r, "guest.resource.access", auditBusiness.CategorySecurity, "A guest opened a shared "+what, details)
}

// notAvailable is the one answer for a link that is off, invalid, expired,
// revoked or gone, whichever it is (no oracle). A server that couldn't answer
// gets guestReadFailed's 503 instead.
func notAvailable(w http.ResponseWriter) {
	helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{
		"msg":       "This link is no longer available.",
		"available": false,
	})
}

// guestReadFailed answers a guest request that couldn't be served. An answer
// about the link or what it opens (guestBusiness.IsUnavailable) is the uniform
// "not available", or goneMsg for one item of what the link opens. Anything
// else is a store or service that didn't answer (Postgres, Dgraph, the call
// server), and gets a 503 the guest's page retries: those used to read as "not
// available" too, and a client's page gave up on a link that still worked.
func guestReadFailed(w http.ResponseWriter, r *http.Request, where string, err error, goneMsg string) {
	switch {
	case guestBusiness.IsUnavailable(err) && goneMsg != "":
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": goneMsg})
	case guestBusiness.IsUnavailable(err):
		notAvailable(w)
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Guest/%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
	}
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
		// What the host speaks, for the server's transcriber. The green room's
		// audio_enabled / video_enabled may still be sent and are not read.
		Language string `json:"language"`
	}
	// Body is optional.
	_ = json.NewDecoder(r.Body).Decode(&body)

	meeting, err := guestBusiness.CreateInstantMeeting(ctx, &userInfo.UserDgraphInfo, body.Language)
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
		"meeting_id":  meeting.MeetingID,
		"host_token":  meeting.HostToken,
		"guest_token": meeting.GuestToken, // raw link token, shown once
		"grant_id":    meeting.GrantID.String(),
		"expires_at":  meeting.ExpiresAt,
	}})
}

// GetGuestMeeting GET /guest/meet/{token} — public link status. Returns 200
// {available:true, workspace_name} for a usable link, else the uniform 404, or
// a 503 when the server couldn't answer.
func GetGuestMeeting(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")

	grant, err := guestBusiness.ValidateMeetingGrant(ctx, token)
	if err != nil {
		guestReadFailed(w, r, "GetGuestMeeting", err, "")
		return
	}
	// What the meeting has on, so the guest is told before joining that it
	// is recorded or transcribed (no names of who is in it: a guest learns
	// those by joining). A call server that can't answer leaves the notice
	// to the call itself rather than failing the page.
	read := map[string]interface{}{"available": true}
	if view, verr := liveKitBusiness.ReadCallView(ctx, grant.ResourceID, false); verr == nil {
		read["recording"] = view.Recording
		read["transcription"] = view.Transcription
	}
	writeGuestRead(w, read)
}

// JoinGuestMeeting POST /guest/meet/{token}/join — mint a single-room guest
// token after collecting a display name.
func JoinGuestMeeting(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")

	var body struct {
		DisplayName string `json:"display_name"`
		// What the guest speaks (their browser's), for the server's
		// transcriber. audio_enabled / video_enabled may still be sent and are
		// not read.
		Language string `json:"language"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}

	grant, err := guestBusiness.ValidateMeetingGrant(ctx, token)
	if err != nil {
		guestReadFailed(w, r, "JoinGuestMeeting", err, "")
		return
	}

	liveToken, room, err := guestBusiness.IssueGuestMeetingToken(ctx, grant, body.DisplayName, body.Language)
	if err != nil {
		if errors.Is(err, guestBusiness.ErrInvalidName) {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Please enter a display name."})
			return
		}
		// ErrGuestDisabled → uniform not-available; a call server that didn't
		// answer → try again.
		guestReadFailed(w, r, "JoinGuestMeeting", err, "")
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
	if !body.Enabled {
		// Guests already in a meeting are put out of it: the policy was
		// re-read only when a guest joined, and nothing put out the ones in.
		helpers.GoSafeNamed("guest.policy-off", func() {
			guestBusiness.RemoveAllMeetingGuests(context.WithoutCancel(ctx))
		})
	}
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

// hostedMeeting answers the room of the instant meeting in the URL for the
// person who started it, or writes the refusal: 404 "no_meeting" for an id no
// meeting ever had, 404 "ended" for a meeting that is over (no live link),
// 403 for anyone else's. Both 404s keep the status an older page reads as
// "This meeting has ended"; the code is what tells them apart (F-54).
func hostedMeeting(w http.ResponseWriter, r *http.Request) (userModels.UserInfo, string, time.Time, bool) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return userInfo, "", time.Time{}, false
	}
	room, ok := guestBusiness.MeetingRoom(chi.URLParam(r, "meeting_id"))
	if !ok {
		writeNoMeeting(w)
		return userInfo, "", time.Time{}, false
	}
	until, err := guestBusiness.HostedMeeting(ctx, room, &userInfo.UserDgraphInfo)
	switch {
	case errors.Is(err, guestBusiness.ErrNotHost):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "not_host", "msg": "Only the person who started this meeting can open it here."})
		return userInfo, "", time.Time{}, false
	// Before ErrNotFound, which it also is.
	case errors.Is(err, guestBusiness.ErrNoMeeting):
		writeNoMeeting(w)
		return userInfo, "", time.Time{}, false
	case errors.Is(err, guestBusiness.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"code": "ended", "msg": "This meeting has ended."})
		return userInfo, "", time.Time{}, false
	case err != nil:
		helpers.LogErrorWithContext(ctx, "controllers/Guest/hostedMeeting err: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return userInfo, "", time.Time{}, false
	}
	return userInfo, room, until, true
}

// writeNoMeeting answers a meeting id that no meeting ever had.
func writeNoMeeting(w http.ResponseWriter) {
	helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"code": "no_meeting", "msg": "We couldn't find this meeting."})
}

// GetInstantMeeting GET /meet/instant/{meeting_id} — an instant meeting's
// stable page, for the person who started it: whether it is on, what it has
// switched on, who is in it, and when its links stop working.
func GetInstantMeeting(w http.ResponseWriter, r *http.Request) {
	_, room, until, ok := hostedMeeting(w, r)
	if !ok {
		return
	}
	view, err := liveKitBusiness.ReadCallView(r.Context(), room, true)
	if err != nil {
		helpers.WriteRetryLater(w, "The call server couldn't answer just now.")
		return
	}
	// The host may put people out of their own meeting.
	view.CanRemove = true
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"room":          view.Room,
		"active":        view.Active,
		"recording":     view.Recording,
		"transcription": view.Transcription,
		"participants":  view.Participants,
		"can_remove":    view.CanRemove,
		"expires_at":    until,
	}})
}

// RejoinInstantMeeting POST /meet/instant/{meeting_id}/join — the host's token
// again, for the person who started the meeting and nobody else.
//
// The host's token and the guest link lived only in the page that started the
// meeting, so leaving or reloading it lost both for good: guests stayed in a
// meeting nobody could host, and opening the guest link made the host a guest.
func RejoinInstantMeeting(w http.ResponseWriter, r *http.Request) {
	userInfo, room, _, ok := hostedMeeting(w, r)
	if !ok {
		return
	}
	var body struct {
		Language string `json:"language"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	token, err := guestBusiness.RejoinMeeting(r.Context(), room, &userInfo.UserDgraphInfo, body.Language)
	if err != nil {
		if errors.Is(err, liveKitBusiness.ErrRemovedFromCall) {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "removed_from_call", "msg": liveKitBusiness.RemovedFromCallMsg})
			return
		}
		helpers.LogErrorWithContext(r.Context(), "controllers/Guest/RejoinInstantMeeting err: %+v", err)
		helpers.WriteRetryLater(w, "The call server couldn't answer just now.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"room":       room,
		"host_token": token,
	}})
}

// NewInstantMeetingLink POST /meet/instant/{meeting_id}/link — another guest
// link to the meeting, for its host (a link is shown once and only its hash is
// kept). With revoke_previous the earlier links stop working, and everyone
// still in the meeting from them is put out of it: how a host keeps out a
// guest they removed, whose next join would otherwise just work.
func NewInstantMeetingLink(w http.ResponseWriter, r *http.Request) {
	if !settingsBusiness.GuestAccessEnabled() {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Guest access is disabled for this workspace."})
		return
	}
	userInfo, room, _, ok := hostedMeeting(w, r)
	if !ok {
		return
	}
	var body struct {
		RevokePrevious bool `json:"revoke_previous"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	link, err := guestBusiness.NewMeetingLink(r.Context(), room, &userInfo.UserDgraphInfo, body.RevokePrevious)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Guest/NewInstantMeetingLink err: %+v", err)
		helpers.WriteRetryLater(w, "The server couldn't answer just now.")
		return
	}
	summary := "Made another guest link to an instant meeting"
	if body.RevokePrevious {
		summary = "Replaced the guest links to an instant meeting"
	}
	auditBusiness.Record(r, "guest.meeting.link", auditBusiness.CategorySecurity, summary,
		map[string]interface{}{"room": room, "grant_id": link.GrantID.String(), "revoke_previous": body.RevokePrevious})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"guest_token": link.GuestToken,
		"grant_id":    link.GrantID.String(),
		"expires_at":  link.ExpiresAt,
	}})
}

// ─── Phase 2: scoped doc/board guest links (live, read-only) ────────────

// CreateResourceGuestLink POST /guest/links — a member who may share a
// resource mints a scoped, expiring, read-only external share link (mayShare
// says who: a doc's owner; a board's editors; a table's managers; a channel's
// or project's admins). Keeping sharing a privileged action means a mere
// viewer can't re-share someone else's private resource outward.
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

	if !mayShare(w, r, userInfo, body.ResourceType, body.ResourceID, true) {
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
		guestReadFailed(w, r, "GuestCollabToken", err, "")
		return
	}

	collabToken, err := guestBusiness.IssueGuestCollabToken(ctx, grant, body.DisplayName)
	if err != nil {
		guestReadFailed(w, r, "GuestCollabToken", err, "")
		return
	}

	documentName := grant.ResourceID
	if grant.ResourceType == "board" {
		documentName = "board:" + grant.ResourceID
	}

	// R4.4: record the guest access (resource + time), content-free and with no
	// member actor. Display name is whatever the guest supplied (already only
	// used for the collab awareness label, never resolved to a member).
	recordGuestAccess(r, grant, grant.ResourceType, guestBusiness.SanitizeGuestName(body.DisplayName))

	writeGuestRead(w, map[string]interface{}{
		"collab_token":  collabToken,
		"document_name": documentName,
		"resource_type": grant.ResourceType,
		"resource_id":   grant.ResourceID,
		"capability":    grant.Capability,
		"title":         guestBusiness.SharedTitle(ctx, grant),
	})
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
		// A doc deleted since the link was made is gone for the guest too.
		if errors.Is(err, guestBusiness.ErrNotFound) {
			notAvailable(w)
			return
		}
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
// Serves a board image to a guest, authorised by the share-link grant rather
// than a member session (guestAttachment, guestDocAttachment.go).
var GuestBoardAttachment = guestAttachment("board", postgressStruct.ATTACHMENT_SRC_BOARD, "GuestBoardAttachment")

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
		guestReadFailed(w, r, "GuestTable", err, "")
		return
	}
	id, err := uuid.Parse(grant.ResourceID)
	if err != nil {
		notAvailable(w)
		return
	}
	// The guest's time zone, where a formula's TODAY() is.
	bundle, err := tableBusiness.GetGuestBundle(tableBusiness.WithZone(ctx, r.URL.Query().Get("tz")), id)
	if err != nil {
		if tableBusiness.IsNotFound(err) {
			err = guestBusiness.ErrNotFound
		}
		guestReadFailed(w, r, "GuestTable", err, "")
		return
	}
	// R4.4: record the guest table access (resource + time), no member actor.
	recordGuestAccess(r, grant, "table", "")
	writeGuestRead(w, bundle)
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
		guestReadFailed(w, r, "GuestDocComments", err, "")
		return
	}
	comments, err := guestBusiness.ListGuestDocComments(ctx, grant.ResourceID)
	if err != nil {
		guestReadFailed(w, r, "GuestDocComments", err, "")
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
	writeGuestRead(w, map[string]interface{}{
		"capability": grant.Capability,
		"comments":   out,
	})
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
		guestReadFailed(w, r, "CreateGuestDocComment", err, "")
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
			guestWriteFailed(w, r, "CreateGuestDocComment", err, "")
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

// grantFor is the active grant behind the {token} in the URL for a kind of
// resource, or the answer written for the caller: the uniform "not
// available", or a 503 when the server couldn't answer.
func grantFor(w http.ResponseWriter, r *http.Request, resourceType string) (*guestModel.GuestGrant, bool) {
	grant, err := guestBusiness.ValidateResourceGrant(r.Context(), chi.URLParam(r, "token"), resourceType)
	if err != nil {
		guestReadFailed(w, r, "grantFor", err, "")
		return nil, false
	}
	return grant, true
}

func channelGrant(w http.ResponseWriter, r *http.Request) (*guestModel.GuestGrant, bool) {
	return grantFor(w, r, guestModel.ResourceChannel)
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
		guestReadFailed(w, r, "GuestChannel", err, "")
		return
	}
	if before.IsZero() {
		recordGuestAccess(r, grant, "channel", "")
	}
	writeGuestRead(w, view)
}

// GuestChannelThread GET /guest/channel/{token}/thread/{post_id} — public.
func GuestChannelThread(w http.ResponseWriter, r *http.Request) {
	grant, ok := channelGrant(w, r)
	if !ok {
		return
	}
	t, err := guestBusiness.GetGuestThread(r.Context(), grant, chi.URLParam(r, "post_id"))
	if err != nil {
		guestReadFailed(w, r, "GuestChannelThread", err, "That message isn't here any more.")
		return
	}
	writeGuestRead(w, t)
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
	if err != nil {
		guestWriteFailed(w, r, "GuestChannelPost", err, "That message isn't here any more.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Sent"})
}

// guestWriteFailed answers a guest's failed write: their own mistake in their
// words, a read-only link, something gone, or a retry for anything else.
func guestWriteFailed(w http.ResponseWriter, r *http.Request, where string, err error, goneMsg string) {
	var in *guestBusiness.ErrGuestInput
	switch {
	case errors.As(err, &in):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": in.Msg})
	case errors.Is(err, guestBusiness.ErrForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "This link is read only."})
	case errors.Is(err, guestBusiness.ErrNotFound) && goneMsg != "":
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": goneMsg})
	case guestBusiness.IsUnavailable(err):
		notAvailable(w)
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Guest/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't send that. Try again."})
	}
}

// GuestProject GET /guest/project/{token} — public. The shared project's tasks
// by status.
func GuestProject(w http.ResponseWriter, r *http.Request) {
	grant, ok := grantFor(w, r, guestModel.ResourceProject)
	if !ok {
		return
	}
	view, err := guestBusiness.GetGuestProject(r.Context(), grant)
	if err != nil {
		guestReadFailed(w, r, "GuestProject", err, "")
		return
	}
	if r.URL.Query().Get("refresh") == "" {
		recordGuestAccess(r, grant, "project", "")
	}
	writeGuestRead(w, view)
}

// GuestProjectTask GET /guest/project/{token}/task/{task_id} — public.
func GuestProjectTask(w http.ResponseWriter, r *http.Request) {
	grant, ok := grantFor(w, r, guestModel.ResourceProject)
	if !ok {
		return
	}
	t, err := guestBusiness.GetGuestTask(r.Context(), grant, chi.URLParam(r, "task_id"))
	if err != nil {
		guestReadFailed(w, r, "GuestProjectTask", err, "That task isn't here any more.")
		return
	}
	writeGuestRead(w, t)
}

// GuestProjectComment POST /guest/project/{token}/task/{task_id}/comment
// {display_name, text} — public.
func GuestProjectComment(w http.ResponseWriter, r *http.Request) {
	grant, ok := grantFor(w, r, guestModel.ResourceProject)
	if !ok {
		return
	}
	var body struct {
		DisplayName string `json:"display_name"`
		Text        string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}
	if err := guestBusiness.CommentAsGuest(r.Context(), grant, chi.URLParam(r, "task_id"), body.DisplayName, body.Text); err != nil {
		guestWriteFailed(w, r, "GuestProjectComment", err, "That task isn't here any more.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Sent"})
}

// GuestProjectReview POST /guest/project/{token}/task/{task_id}/review
// {display_name, decision: approved|changes, note} — public. The client's
// verdict on a task.
func GuestProjectReview(w http.ResponseWriter, r *http.Request) {
	grant, ok := grantFor(w, r, guestModel.ResourceProject)
	if !ok {
		return
	}
	var body struct {
		DisplayName string `json:"display_name"`
		Decision    string `json:"decision"`
		Note        string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}
	review, err := guestBusiness.ReviewAsGuest(r.Context(), grant, chi.URLParam(r, "task_id"), body.DisplayName, body.Decision, body.Note)
	if err != nil {
		guestWriteFailed(w, r, "GuestProjectReview", err, "That task isn't here any more.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": review})
}

// TaskClientReview GET /task/clientReview/{task_uuid} — the client's newest
// verdict on a task, for anyone who can see the task. Null when there's none.
func TaskClientReview(w http.ResponseWriter, r *http.Request) {
	taskID, _, ok := projectaccess.RequireTask(w, r, chi.URLParam(r, "task_uuid"), workAccess.NeedView)
	if !ok {
		return
	}
	review, err := guestBusiness.LatestReview(taskID)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Guest/TaskClientReview err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't load the client's verdict."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": review})
}

// mayShare says whether the caller may share a resource with people outside
// the workspace (and so see and turn off its links), writing the refusal when
// not. It reuses each resource's own access checks. newLink is a link about to
// be made, which a deleted doc takes no more of; its owner may still see and
// turn off the ones it has.
func mayShare(w http.ResponseWriter, r *http.Request, userInfo userModels.UserInfo, resourceType, resourceID string, newLink bool) bool {
	ctx := r.Context()
	switch resourceType {
	case "doc":
		status, refusal, err := docShareRefusal(ctx, userInfo, resourceID, newLink)
		if err != nil {
			helpers.WriteDocReadFailure(w, r, err, "controllers/mayShare")
			return false
		}
		if status != 0 {
			helpers.WriteJSON(w, status, refusal)
			return false
		}
	case "board":
		b, err := boardBusiness.GetBasicBoardByUUID(ctx, resourceID, userInfo.UserDgraphInfo.Uid)
		if err != nil || b == nil {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Board not found"})
			return false
		}
		isOwner := b.CreatedBy != nil && b.CreatedBy.Uuid == userInfo.UserDgraphInfo.Uuid
		if b.HasEditAccess == 0 && !isOwner {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You need edit access to share this board."})
			return false
		}
	case "table":
		tid, perr := uuid.Parse(resourceID)
		if perr != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid table id"})
			return false
		}
		// The caller must be able to MANAGE the table to share it externally:
		// one they can't see is not found, one they can only see is refused.
		actor := tableBusiness.Actor{UserID: userInfo.UserPostgresInfo.Id, IsAdmin: userInfo.UserPostgresInfo.IsAdmin}
		if tableBusiness.CanView(ctx, tid, actor) != nil {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Table not found"})
			return false
		}
		if tableBusiness.CanManage(ctx, tid, actor) != nil {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You need manage access to share this table."})
			return false
		}
	case "channel":
		// Inviting someone from outside into a channel is the channel admins'
		// call (or a workspace admin's), as adding a member is.
		cid, perr := uuid.Parse(resourceID)
		if perr != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid channel id"})
			return false
		}
		ch, cerr := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, cid, userInfo.UserDgraphInfo.Uid)
		if cerr != nil || ch == nil || ch.Uuid == "" {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Channel not found"})
			return false
		}
		if ch.IsAdmin == 0 && !userInfo.UserPostgresInfo.IsAdmin {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the channel's admins can invite guests."})
			return false
		}
	case "project":
		// Showing a project to a client is the project admins' call (or a
		// workspace admin's), as adding a member is.
		if _, perr := uuid.Parse(resourceID); perr != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid project id"})
			return false
		}
		p, perr := projectBusiness.GetBasicDgraphProjectInfo(ctx, resourceID, userInfo.UserDgraphInfo.Uid)
		if perr != nil || p == nil || p.Uuid == "" || helpers.IsSoftDeleted(p.DeletedAt) ||
			(!p.ReaderIsIn() && !userInfo.UserPostgresInfo.IsAdmin) {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Project not found"})
			return false
		}
		// Its admins by Work Access's rule (a role, not only the edge), or a
		// workspace admin; making a link is recorded either way.
		if !userInfo.UserPostgresInfo.IsAdmin {
			standing, serr := workAccess.Of(ctx, userInfo.UserDgraphInfo.Uuid, p)
			if serr != nil {
				helpers.LogErrorWithContext(ctx, "controllers/Guest mayShare couldn't read the caller's role err: %+v", serr)
				helpers.WriteRetryLater(w, "Couldn't check what you can do in this project.")
				return false
			}
			if !standing.Can(workAccess.NeedManage) {
				helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Only the project's admins can share it with a client."})
				return false
			}
		}
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Unsupported resource type"})
		return false
	}
	return true
}

// docShareRefusal is mayShare's answer for a doc, without writing it: 0 when
// the caller may share it, and otherwise the status and body to refuse with.
// err is a read that failed, which is neither a yes nor a no: the caller
// answers it with helpers.WriteDocReadFailure.
//
// Sharing a doc outside the workspace is its owner's call, as sharing it
// inside is (only the owner changes who it's shared with). Editors could mint
// links: a link to a private doc opened it to anyone holding the link, though
// its owner had shared it with no member beyond its editors (decided 11 Oct
// 2026). Links editors made before keep working until the owner, an admin, or
// whoever made them turns them off.
//
// A doc the person can't open, or couldn't before it was deleted, is to them
// one that isn't there (helpers.DocNotFound): it used to say only its owner
// could share it, which said it was there.
func docShareRefusal(ctx context.Context, userInfo userModels.UserInfo, docID string, newLink bool) (int, helpers.Envolope, error) {
	notThere := helpers.Envolope{"msg": helpers.DocNotFound}
	d, err := docBusiness.GetDgraphDocByUUIDOnlyEditingInfo(ctx, docID, userInfo.UserDgraphInfo.Uid)
	switch {
	case errors.Is(err, docBusiness.ErrNotFound), err == nil && d == nil:
		return http.StatusNotFound, notThere, nil
	case err != nil:
		return 0, nil, err
	case !docBusiness.CouldRead(d, userInfo.UserDgraphInfo.Uuid):
		return http.StatusNotFound, notThere, nil
	case !docBusiness.IsOwner(d, userInfo.UserDgraphInfo.Uid):
		return http.StatusForbidden, helpers.Envolope{"msg": "Only the doc's owner can share it outside the workspace."}, nil
	case newLink && docBusiness.IsDeleted(d):
		return http.StatusGone, helpers.Envolope{"msg": "This doc was deleted.", "deleted": true}, nil
	}
	return 0, nil, nil
}

// GuestLinkView is one live link to a resource, for the people who may share it.
type GuestLinkView struct {
	ID         uuid.UUID  `json:"id"`
	Capability string     `json:"capability"`
	ExpiresAt  *time.Time `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
	Mine       bool       `json:"mine"`
}

// ListResourceGuestLinks GET /guest/links?resource_type=&resource_id= — the
// live links to one resource, for anyone who may share it.
//
// A doc's links are all its owner's to see. Anyone else sees only the links
// they made, and none when they made none: editors could make links until
// 11 Oct 2026, those links still work, and whoever made one may always turn it
// off (RevokeResourceGuestLink), which takes finding it. Refusing them here
// left an editor's link open with no way for them to find it.
func ListResourceGuestLinks(w http.ResponseWriter, r *http.Request) {
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	q := r.URL.Query()
	resourceType, resourceID := q.Get("resource_type"), q.Get("resource_id")
	onlyMine := false
	if resourceType == guestModel.ResourceDoc {
		status, _, err := docShareRefusal(r.Context(), userInfo, resourceID, false)
		if err != nil {
			helpers.WriteDocReadFailure(w, r, err, "controllers/ListResourceGuestLinks")
			return
		}
		onlyMine = status != 0
	} else if !mayShare(w, r, userInfo, resourceType, resourceID, false) {
		return
	}
	grants, err := guestModel.ListActiveForResource(r.Context(), resourceType, resourceID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't load the links. Try again."})
		return
	}
	out := make([]GuestLinkView, 0, len(grants))
	for _, g := range grants {
		mine := g.CreatedBy == userInfo.UserPostgresInfo.Id
		if onlyMine && !mine {
			continue
		}
		out = append(out, GuestLinkView{ID: g.Id, Capability: g.Capability, ExpiresAt: g.ExpiresAt, CreatedAt: g.CreatedAt, Mine: mine})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// RevokeResourceGuestLink POST /guest/links/{id}/revoke — turns a link off,
// for anyone who may share what it opens, and for whoever made the link (a
// workspace admin can also do it from admin settings). Turning a link off only
// narrows who sees what it opens, so its maker keeps that, whatever they may
// share today.
func RevokeResourceGuestLink(w http.ResponseWriter, r *http.Request) {
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a link."})
		return
	}
	g, err := guestModel.GetByID(r.Context(), id)
	if err != nil || g == nil || g.ResourceType == guestModel.ResourceMeeting {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That link isn't here any more."})
		return
	}
	if g.CreatedBy != userInfo.UserPostgresInfo.Id && !mayShare(w, r, userInfo, g.ResourceType, g.ResourceID, false) {
		return
	}
	if err := guestModel.Revoke(r.Context(), id); err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't turn the link off. Try again."})
		return
	}
	auditBusiness.Record(r, "guest.link.revoke", auditBusiness.CategorySecurity, "Turned off an external share link",
		map[string]interface{}{"resource_type": g.ResourceType, "resource_id": g.ResourceID, "grant_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Turned off"})
}
