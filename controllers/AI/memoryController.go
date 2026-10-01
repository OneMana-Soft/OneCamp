package controllers

// HTTP handlers for the Workspace Memory Layer.
//
// List/manage endpoints are user-facing (mounted under /ai, any
// authenticated user) and always permission-scoped to the caller's
// accessible channels/projects in the business layer. The enable toggle
// is admin-only (mounted under /admin/ai).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	business "github.com/akashc777/OneCamp/business/AI"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ListMemory handles GET /ai/memory?kind=decision,commitment&status=open
func ListMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	kinds := splitCSVParam(r.URL.Query().Get("kind"))
	statuses := splitCSVParam(r.URL.Query().Get("status"))
	// Default to open items when no status filter is given.
	if len(statuses) == 0 {
		statuses = []string{"open"}
	}
	// Optional channel scoping (permission-intersected in the business layer).
	scopeChannel := strings.TrimSpace(r.URL.Query().Get("channel"))

	resp, err := business.ListWorkspaceMemory(ctx, &userInfo, kinds, statuses, 100, scopeChannel)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListMemory failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "failed to load workspace memory", "err": err.Error(),
		})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// UpdateMemoryStatus handles POST /ai/memory/{id}/status
func UpdateMemoryStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid memory id"})
		return
	}
	var req adapter.UpdateMemoryStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.UpdateWorkspaceMemoryStatus(ctx, &userInfo, id, strings.TrimSpace(req.Status)); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not authorized") {
			status = http.StatusForbidden
		} else if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "memory updated"})
}

// UpdateMemoryDue handles POST /ai/memory/{id}/due — set or clear a
// commitment's due date. Body: {"due":"YYYY-MM-DD"} (empty clears it).
func UpdateMemoryDue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid memory id"})
		return
	}
	var req adapter.UpdateMemoryDueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.UpdateWorkspaceMemoryDue(ctx, &userInfo, id, req.Due); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not authorized") {
			status = http.StatusForbidden
		} else if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "due date updated"})
}

// DeleteMemory handles DELETE /ai/memory/{id}
func DeleteMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid memory id"})
		return
	}
	if err := business.DeleteWorkspaceMemory(ctx, &userInfo, id); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not authorized") {
			status = http.StatusForbidden
		} else if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "memory deleted"})
}

// CaptureMemory handles POST /ai/memory/capture — user-initiated "save
// this message to memory". Permission-checked in the business layer.
func CaptureMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.CaptureMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	view, err := business.CaptureWorkspaceMemory(ctx, &userInfo, req)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not authorized") {
			status = http.StatusForbidden
		} else if strings.Contains(err.Error(), "not enabled") {
			status = http.StatusConflict
		}
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": view})
}

// memoryActionStatus maps a business error to the right HTTP status.
func memoryActionStatus(err error) int {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not authorized"), strings.Contains(msg, "must be an admin"):
		return http.StatusForbidden
	case strings.Contains(msg, "not found"):
		return http.StatusNotFound
	case strings.Contains(msg, "not enabled"):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// CreateTaskFromMemory handles POST /ai/memory/{id}/create-task — turns a
// memory item (commitment/decision) into a project task, then resolves it.
func CreateTaskFromMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid memory id"})
		return
	}
	var req adapter.CreateTaskFromMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	resp, err := business.CreateTaskFromMemory(ctx, &userInfo, business.CreateTaskFromMemoryInput{
		MemoryID:     id,
		ProjectUUID:  strings.TrimSpace(req.ProjectUUID),
		AssigneeUUID: strings.TrimSpace(req.AssigneeUUID),
		Priority:     strings.TrimSpace(req.Priority),
	})
	if err != nil {
		helpers.WriteJSON(w, memoryActionStatus(err), helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// RemindAboutMemory handles POST /ai/memory/{id}/remind — sets a calendar
// reminder for the caller about a memory item.
func RemindAboutMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid memory id"})
		return
	}
	var req adapter.RemindAboutMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	resp, err := business.RemindAboutMemory(ctx, &userInfo, business.RemindAboutMemoryInput{
		MemoryID:  id,
		StartTime: strings.TrimSpace(req.StartTime),
	})
	if err != nil {
		helpers.WriteJSON(w, memoryActionStatus(err), helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// GetBriefing handles GET /ai/briefing — the personal "your world" card.
func GetBriefing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	resp, err := business.GetBriefing(ctx, &userInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetBriefing failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load briefing"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// GetWhatNeedsMe handles GET /ai/attention — the cross-surface "what needs me
// now" queue (pending approvals, overdue tasks/commitments, open questions, and
// upcoming calendar items), one prioritized list scoped to the caller.
func GetWhatNeedsMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	resp, err := business.GetWhatNeedsMe(ctx, &userInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetWhatNeedsMe failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load your attention queue"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// ProposeSchedule handles POST /ai/schedule/propose — given participants and a
// duration, returns candidate meeting times ranked by who's free. Read-only.
func ProposeSchedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.ScheduleProposeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	resp, err := business.ProposeSchedule(ctx, &userInfo, req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// ConfirmSchedule handles POST /ai/schedule/confirm — creates the chosen
// meeting AS the requester through the normal calendar create path.
func ConfirmSchedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.ScheduleConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	resp, err := business.ConfirmSchedule(ctx, &userInfo, req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// RescheduleOptions handles POST /ai/schedule/reschedule — for an existing
// event, report who has a conflict now and propose alternative times. Read-only.
func RescheduleOptions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.ScheduleRescheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	resp, err := business.RescheduleOptions(ctx, &userInfo, req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// ConfirmReschedule handles POST /ai/schedule/reschedule/confirm — move the
// event to the chosen slot (creator-only, via the normal calendar update path).
func ConfirmReschedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.ScheduleConfirmRescheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	resp, err := business.ConfirmReschedule(ctx, &userInfo, req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// MeetingPrep handles POST /ai/schedule/prep — generate an on-demand
// pre-meeting prep brief for an upcoming event. Read-only; visible to the
// event's creator or any participant.
func MeetingPrep(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.MeetingPrepRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	resp, err := business.MeetingPrepBrief(ctx, &userInfo, req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}
func GetChannelMemoryExclusion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	channelUUID := strings.TrimSpace(r.URL.Query().Get("channel"))
	if channelUUID == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "channel is required"})
		return
	}
	excluded, err := business.GetChannelMemoryExcluded(ctx, &userInfo, channelUUID)
	if err != nil {
		helpers.WriteJSON(w, memoryActionStatus(err), helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": map[string]bool{"excluded": excluded}})
}

// SetChannelMemoryExclusion handles POST /ai/memory/channel-exclusion
// body: { channel_uuid, excluded }. Channel-admin only (enforced in business).
func SetChannelMemoryExclusion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req struct {
		ChannelUUID string `json:"channel_uuid"`
		Excluded    bool   `json:"excluded"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if strings.TrimSpace(req.ChannelUUID) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "channel_uuid is required"})
		return
	}
	if err := business.SetChannelMemoryExcluded(ctx, &userInfo, strings.TrimSpace(req.ChannelUUID), req.Excluded); err != nil {
		helpers.WriteJSON(w, memoryActionStatus(err), helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: user=%s set channel %s memory excluded=%v",
		userInfo.UserPostgresInfo.Id.String(), req.ChannelUUID, req.Excluded)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "updated"})
}

// SetMemoryLayer handles POST /admin/ai/memory-layer (admin toggle).
func SetMemoryLayer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetMeetingRecapRequest // reuse {enabled bool}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetMemoryLayerEnabled(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set memory_layer enabled=%v", aiAdminUserUUID(r), req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "memory layer toggled"})
}

// SetTeamReport handles POST /admin/ai/team-report (admin toggle).
func SetTeamReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetMeetingRecapRequest // reuse {enabled bool}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetTeamReportEnabled(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set team_report enabled=%v", aiAdminUserUUID(r), req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "team report toggled"})
}

// RebuildMemory handles POST /admin/ai/memory/rebuild — kicks off a
// background backfill that extracts memory from historical content. Admin
// only, rate-limited (it's heavy), audit-logged. Returns 202 on start,
// 409 when one is already running, 400 when the feature is off.
func RebuildMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Heavy operation: at most a couple of kick-offs per admin per minute.
	if aiAdminRateLimited(w, r, "memory_rebuild", 3) {
		return
	}
	started, reason := business.RunMemoryBackfillAsync(ctx)
	if !started {
		status := http.StatusBadRequest
		if strings.Contains(reason, "already running") {
			status = http.StatusConflict
		}
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": reason})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s triggered memory backfill", aiAdminUserUUID(r))
	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"status": "success", "msg": "memory rebuild started"})
}

// GetMemoryBackfillStatus handles GET /admin/ai/memory/rebuild/status.
func GetMemoryBackfillStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := business.GetMemoryBackfillStatus(ctx)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": st})
}

// RunTeamReportNow handles POST /admin/ai/team-report/run — admin verify
// action: run the weekly team report immediately, bypassing the Monday/hour
// schedule and the per-period idempotency lock, so the admin can confirm it
// works end to end (it posts into the active channels). Rate-limited + audited.
func RunTeamReportNow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if aiAdminRateLimited(w, r, "team_report_run", 3) {
		return
	}
	posted, processed, err := business.RunTeamReportNowForVerify(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s ran team report now (posted=%d processed=%d)", aiAdminUserUUID(r), posted, processed)
	var msg string
	switch {
	case posted > 0:
		msg = fmt.Sprintf("Posted %d report(s) across %d active channel(s) — check those channels.", posted, processed)
	case processed > 0:
		msg = fmt.Sprintf("Checked %d active channel(s), but none had enough memory items yet (a report needs a few decisions/commitments/open questions). Add some activity or run a memory rebuild, then retry.", processed)
	default:
		msg = "No channels had recent activity to report on yet. The weekly report posts once channels are active and Workspace Memory has items."
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"msg":    msg,
		"data":   map[string]int{"posted": posted, "processed": processed},
	})
}

// SendTestDigest handles POST /admin/ai/memory/digest/test — emails the
// calling admin a one-off "open items" digest now, to verify email delivery
// (the daily digest is opt-in per user, so this is the way to confirm the
// pipeline works). Rate-limited + audited.
func SendTestDigest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if aiAdminRateLimited(w, r, "digest_test", 3) {
		return
	}
	uid := aiAdminUserUUID(r)
	if uid == "" {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "not authorized"})
		return
	}
	if err := business.SendTestMemoryDigest(ctx, uid); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s sent test memory digest", uid)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		// No longer hedged. The business layer dispatches synchronously and
		// returns an error when nothing was sent, so reaching here means the
		// email really was queued for delivery.
		"msg": "Test digest queued for delivery to your email.",
	})
}

// splitCSVParam splits a comma-separated query param into trimmed values.
func splitCSVParam(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// UnifiedSearch handles POST /ai/search — one query across the user's workspace
// content and their connected external accounts (Gmail, GitHub), grouped by
// source. Read-only; everything is permission/owner-scoped in the business layer.
func UnifiedSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	resp, err := business.UnifiedSearch(ctx, &userInfo, req.Query)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// UnifiedSearchAnswer handles POST /ai/search/answer — a grounded, cited AI
// answer synthesized over the same permission-scoped unified search corpus.
// Read-only; retrieval + synthesis are scoped/gated in the business layer.
func UnifiedSearchAnswer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	resp, err := business.AnswerFromSearch(ctx, &userInfo, req.Query)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// ExtractTasks handles POST /ai/extract-tasks — extract candidate action items
// from a conversation (channel/dm/group) or pasted text. Read-only; the source
// transcript is permission-scoped in the business layer.
func ExtractTasks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req struct {
		SourceType string `json:"source_type"` // channel | dm | group | text
		SourceID   string `json:"source_id"`
		Text       string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	resp, err := business.ExtractActionItems(ctx, &userInfo, req.SourceType, req.SourceID, req.Text)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// CreateTasksFromExtraction handles POST /ai/extract-tasks/create — create the
// approved tasks in the chosen project AS the user (project-admin enforced).
func CreateTasksFromExtraction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req struct {
		ProjectUUID string                  `json:"project_uuid"`
		Tasks       []business.ProposedTask `json:"tasks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	created, failed, err := business.CreateTasksFromProposals(ctx, &userInfo, req.ProjectUUID, req.Tasks)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": map[string]int{"created": created, "failed": failed}})
}

// AIActivityProof handles GET /ai/activity/proof — the record of what the AI
// did in the caller's name, as a document they can download and a stranger can
// check.
//
// Scoped to the caller with no way to ask for anybody else's: the principal
// comes from the session, never from the request. Served as a download because
// the point of it is to leave this workspace and still mean something.
func AIActivityProof(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	proof, err := auditBusiness.BuildMemberProof(ctx,
		userInfo.UserPostgresInfo.Id,
		userInfo.UserPostgresInfo.EmailID,
		business.AIActivityActionPrefixes(),
		proofLimitFrom(r),
	)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="onecamp-ai-record.json"`)
	// Indented, because the first thing anybody does with this file is open it
	// and look for the row they remember.
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if eerr := enc.Encode(proof); eerr != nil {
		helpers.LogErrorWithContext(ctx, "AIActivityProof encode: %v", eerr)
	}
}

// proofLimitFrom reads an optional row cap, bounded by the business layer.
func proofLimitFrom(r *http.Request) int {
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// AIActivity handles GET /admin/ai/activity — the unified AI activity timeline
// (agent runs + AI-attributable audit entries), newest-first. Admin-only
// (mounted under /admin/ai); the business layer scopes audit entries to admins.
func AIActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	ownerID := userInfo.UserPostgresInfo.Id
	items, err := business.AIActivity(ctx, userInfo.UserPostgresInfo.IsAdmin, &ownerID, limit)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": items})
}
