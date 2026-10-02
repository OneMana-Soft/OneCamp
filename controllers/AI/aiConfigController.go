package controllers

// HTTP handlers for the admin-managed, model-agnostic AI configuration.
// All routes here are mounted under the admin router (gated by
// VerifyAdminAuthOnlyPostgres in router.go), so every handler assumes the
// caller is a verified admin. OneCamp is single-tenant, so there is one
// global AI configuration.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	business "github.com/akashc777/OneCamp/business/AI"
	mcpServerBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// aiAdminUserUUID extracts the calling admin's postgres UUID for rate
// limiting. Admin routes always set UserInfo in context.
func aiAdminUserUUID(r *http.Request) string {
	if ui, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo); ok {
		return ui.UserPostgresInfo.Id.String()
	}
	return ""
}

// aiAdminRateLimited enforces a per-admin, per-action fixed-window limit
// for AI config operations that make outbound network calls. Returns true
// (and writes a 429) when the caller should stop.
func aiAdminRateLimited(w http.ResponseWriter, r *http.Request, action string, max int) bool {
	uid := aiAdminUserUUID(r)
	if uid == "" {
		return false // auth middleware already guarantees identity; fail open
	}
	res := redisStore.AllowFixedWindow(r.Context(), registry.AIAdminRate, []string{action, uid}, max)
	if !res.Allowed {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", res.RetryAfterSeconds()))
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{
			"msg": "Too many requests. Please slow down.",
		})
		return true
	}
	return false
}

// GetAIConfig handles GET /admin/ai/config
func GetAIConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg, err := business.GetAIConfig(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetAIConfig failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load AI configuration", "err": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": cfg})
}

// ListProviderModels handles GET /admin/ai/providers/{providerId}/models
// Query param ?refresh=true bypasses the cache.
func ListProviderModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "providerId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid provider id"})
		return
	}
	refresh := r.URL.Query().Get("refresh") == "true"

	// Listing hits the provider API/Ollama; bound how often per admin.
	if aiAdminRateLimited(w, r, "list_models", 30) {
		return
	}

	models, err := business.ListProviderModels(ctx, id, refresh)
	if err != nil {
		// A KEY THAT WILL NOT DECRYPT IS NOT A BAD GATEWAY.
		//
		// 502 said the provider was unreachable, and the frontend renders msg, so the admin was
		// told "Could not list models — failed to list models" for a condition the server could
		// name exactly. Nothing was contacted: the request cannot be satisfied until a key is
		// re-entered, which is the workspace's own state, so 409 and the actionable sentence go
		// in msg where the existing UI already displays it. No frontend change needed.
		if errors.Is(err, business.ErrProviderKeyUnreadable) {
			// INFO, NOT ERROR, and the distinction is the general rule: a 4xx is the caller's
			// state, a 5xx is ours. This is the workspace's own configuration, the response tells
			// the admin exactly what to do, and it recurs on every visit to the page until they do
			// it — so logging it at ERROR turns an answered question into a standing alarm and
			// ships it to the collector on every page view.
			helpers.LogInfoWithContext(ctx,
				"controllers/ListProviderModels refused: %v (answered 409; admin must re-enter the key)", err)
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"msg":  err.Error(),
				"code": "provider_key_unreadable",
			})
			return
		}
		// Everything else is a genuine upstream or server failure and keeps ERROR.
		helpers.LogErrorWithContext(ctx, "controllers/ListProviderModels failed: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "failed to list models", "err": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": map[string]any{"models": models}})
}

// GetOllamaCatalog handles GET /admin/ai/providers/{providerId}/catalog
//
// Returns the curated, installable Ollama model catalog annotated with live
// installed-state and server-resource feasibility. Only valid for local
// Ollama providers.
func GetOllamaCatalog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "providerId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid provider id"})
		return
	}

	// Annotating with installed-state hits Ollama; bound per admin.
	if aiAdminRateLimited(w, r, "list_catalog", 30) {
		return
	}

	// ?refresh=true forces a remote-manifest re-fetch (bypassing the cache)
	// so a newly-published catalog is picked up on demand.
	var resp *adapter.OllamaCatalogResponse
	if r.URL.Query().Get("refresh") == "true" {
		resp, err = business.RefreshOllamaCatalog(ctx, id)
	} else {
		resp, err = business.GetOllamaCatalog(ctx, id)
	}
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}
func TestConnection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if aiAdminRateLimited(w, r, "test_connection", 20) {
		return
	}
	var req adapter.TestConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	resp, err := business.TestConnection(ctx, req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// CreateProvider handles POST /admin/ai/providers (custom endpoint).
func CreateProvider(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.CreateProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	v, err := business.CreateCustomProvider(ctx, req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s created custom provider id=%s label=%q base_url=%q",
		aiAdminUserUUID(r), v.ID, v.Label, v.BaseURL)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": v})
}

// UpdateProvider handles PATCH /admin/ai/providers/{providerId}
func UpdateProvider(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "providerId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid provider id"})
		return
	}
	var req adapter.UpdateProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	v, err := business.UpdateProvider(ctx, id, req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": v})
}

// DeleteProvider handles DELETE /admin/ai/providers/{providerId}
func DeleteProvider(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "providerId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid provider id"})
		return
	}
	if err := business.DeleteCustomProvider(ctx, id); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s deleted provider id=%s", aiAdminUserUUID(r), id)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "provider deleted"})
}

// SetChatModel handles POST /admin/ai/chat-model
func SetChatModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetChatModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetChatModel(ctx, req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set chat model provider=%s model=%q",
		aiAdminUserUUID(r), req.ProviderID, req.Model)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "chat model updated"})
}

// SetVisionModel handles POST /admin/ai/vision-model. An empty provider_id or
// model clears the selection (image analysis disabled).
func SetVisionModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetVisionModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetVisionModel(ctx, req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set vision model provider=%s model=%q",
		aiAdminUserUUID(r), req.ProviderID, req.Model)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "vision model updated"})
}

// SetEmbeddingModel handles POST /admin/ai/embedding-model
func SetEmbeddingModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetEmbeddingModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetEmbeddingModel(ctx, req); err != nil {
		// A dimension-change-needs-reindex rejection is a 409 so the FE can
		// prompt the admin to confirm the reindex.
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "requires reindexing") {
			status = http.StatusConflict
		}
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set embedding model provider=%s model=%q dim=%d reindex=%v",
		aiAdminUserUUID(r), req.ProviderID, req.Model, req.Dimension, req.Reindex)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "embedding model updated"})
}

// SetEnabled handles POST /admin/ai/enabled
func SetEnabled(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetEnabledRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetEnabled(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set ai enabled=%v", aiAdminUserUUID(r), req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "AI toggled"})
}

// SetMeetingRecap handles POST /admin/ai/meeting-recap
func SetMeetingRecap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetMeetingRecapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetMeetingRecapEnabled(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set meeting_recap enabled=%v", aiAdminUserUUID(r), req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "meeting recap toggled"})
}

// SetMeetingNotesDoc handles POST /admin/ai/meeting-notes-doc
func SetMeetingNotesDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetMeetingRecapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetMeetingNotesDocEnabled(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set meeting_notes_doc enabled=%v", aiAdminUserUUID(r), req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "meeting notes document toggled"})
}

// SetMeetingRecapInstructions handles POST /admin/ai/meeting-recap/instructions
func SetMeetingRecapInstructions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetMeetingRecapInstructionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetMeetingRecapInstructions(ctx, req.Instructions); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set meeting_recap instructions (len=%d)", aiAdminUserUUID(r), len(strings.TrimSpace(req.Instructions)))
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "meeting recap instructions saved"})
}

// SetCoworker handles POST /admin/ai/coworker
func SetCoworker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetMeetingRecapRequest // reuse {enabled bool}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetCoworkerEnabled(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set coworker enabled=%v", aiAdminUserUUID(r), req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "coworker toggled"})
}

// SetRateLimit handles POST /admin/ai/rate-limit
func SetRateLimit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetRateLimitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetRateLimit(ctx, req.RateLimitPerMin); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "rate limit updated"})
}

// SetContextWindow handles POST /admin/ai/context-window
// Body: { context_window_tokens }. 0 = use env/default.
func SetContextWindow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetContextWindowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetContextWindow(ctx, req.ContextWindowTokens); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "context window updated"})
}

// SetWorkspaceTokenBudget handles POST /admin/ai/workspace-token-budget
// Body: { tokens }. 0 = unlimited.
func SetWorkspaceTokenBudget(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetTokenBudgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetWorkspaceDailyTokenBudget(ctx, req.Tokens); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "workspace token budget updated"})
}

// SetUserTokenBudget handles POST /admin/ai/user-token-budget
// Body: { tokens }. 0 = unlimited.
func SetUserTokenBudget(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetTokenBudgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetUserDailyTokenBudget(ctx, req.Tokens); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "user token budget updated"})
}

// SetReasoning handles POST /admin/ai/reasoning
// Body: { enabled }. Toggles "thinking" mode for reasoning-capable models.
func SetReasoning(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetReasoningRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetReasoning(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set reasoning enabled=%v", aiAdminUserUUID(r), req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "reasoning updated"})
}

// SetLocalOnly handles POST /admin/ai/local-only
// Body: { enabled }. Toggles the data-residency guarantee (no content to a
// cloud model). Enabling is refused while an active endpoint is non-local.
func SetLocalOnly(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetReasoningRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetLocalOnly(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set local-only enabled=%v", aiAdminUserUUID(r), req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "local-only mode updated"})
}

// SetPIIRedaction handles POST /admin/ai/pii-redaction
// Body: { enabled }. Toggles PII redaction before cloud egress.
func SetPIIRedaction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetReasoningRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetPIIRedaction(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set pii-redaction enabled=%v", aiAdminUserUUID(r), req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "PII redaction updated"})
}

// SetPIIPatterns handles POST /admin/ai/pii-patterns
// Body: { patterns }. Sets the admin-defined redaction regexes (one per line).
func SetPIIPatterns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetPIIPatternsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetPIICustomPatterns(ctx, req.Patterns); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s updated pii custom patterns", aiAdminUserUUID(r))
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "PII patterns updated"})
}

// DeleteModel handles POST /admin/ai/models/delete
func DeleteModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.DeleteModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	id, err := uuid.Parse(req.ProviderID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid provider id"})
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "model is required"})
		return
	}
	if err := business.DeleteModel(ctx, id, req.Model); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s deleted model provider=%s model=%q",
		aiAdminUserUUID(r), id, req.Model)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "model deleted"})
}

// GetSystemStats handles GET /admin/ai/system
func GetSystemStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats := business.GetSystemStats(ctx)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": stats})
}

// GetReindexStatus handles GET /admin/ai/reindex/status
func GetReindexStatus(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": ai.GetReindexState()})
}

// PullModelStream handles POST /admin/ai/models/pull
// Installs a model on a local provider (Ollama), streaming download
// progress to the client via Server-Sent Events.
//
// The pull is tied to the request context, so closing the tab cancels the
// in-flight download. This is acceptable because Ollama caches completed
// layers: re-initiating the pull resumes from where it left off rather
// than restarting. A 30-minute deadline bounds multi-GB downloads.
func PullModelStream(w http.ResponseWriter, r *http.Request) {
	// Generous deadline: model downloads can be large. 30 min covers
	// multi-GB models on a typical connection.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	var req adapter.PullModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	id, err := uuid.Parse(req.ProviderID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid provider id"})
		return
	}
	model, err := business.ValidatePullModelName(req.Model)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	req.Model = model

	// Pulls are heavy (multi-GB downloads); strict per-admin cap.
	if aiAdminRateLimited(w, r, "pull_model", 10) {
		return
	}

	manager, _, err := business.GetManagerForPull(ctx, id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	// SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "streaming not supported"})
		return
	}

	progress, err := manager.PullModel(ctx, req.Model)
	if err != nil {
		// Distinguish the actionable "update Ollama" case.
		payload := map[string]any{"error": err.Error(), "done": true}
		if isUpdateRequired(err) {
			payload["update_required"] = true
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		flusher.Flush()
		return
	}

	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s pulling model provider=%s model=%q",
		aiAdminUserUUID(r), id, req.Model)

	for p := range progress {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		flusher.Flush()
		if p.Done {
			break
		}
	}

	// Refresh the model cache so the picker shows the newly-installed model.
	business.RefreshProviderModelCache(context.Background(), id)
}

// isUpdateRequired reports whether the error is the Ollama-too-old case.
func isUpdateRequired(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "update required")
}

// ─── Authorized models (allowlist) ─────────────────────────────────────

// ListAuthorizedModels handles GET /admin/ai/authorized-models
func ListAuthorizedModels(w http.ResponseWriter, r *http.Request) {
	models, err := business.ListAuthorizedModels(r.Context())
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": models})
}

// AuthorizeModel handles POST /admin/ai/authorized-models
func AuthorizeModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.AuthorizeModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	providerID, err := uuid.Parse(strings.TrimSpace(req.ProviderID))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid provider id"})
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "model is required"})
		return
	}
	m, err := business.AuthorizeModel(ctx, providerID, strings.TrimSpace(req.Model), strings.TrimSpace(req.Label))
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s authorized model %s/%s", aiAdminUserUUID(r), req.ProviderID, req.Model)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": m})
}

// SetAuthorizedModelEnabled handles POST /admin/ai/authorized-models/{id}/enabled
func SetAuthorizedModelEnabled(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid model id"})
		return
	}
	var req adapter.SetEnabledToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetAuthorizedModelEnabled(ctx, id, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set authorized model %s enabled=%v", aiAdminUserUUID(r), id, req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "authorized model updated"})
}

// DiscoverAuthorizedModelLimits handles GET /admin/ai/authorized-models/{id}/discover-limits
//
// Asks the model's own provider what its limits are and returns the answer as a
// SUGGESTION. It does not write anything: the admin sees the numbers, the field they came
// from, and saves them if they agree.
//
// Suggest-rather-than-apply is deliberate. Writing a probed value straight to the row
// would re-size prompts without anyone deciding to, which is the one thing migration 140
// was careful to avoid, and a gateway's answer may describe a different model than the one
// it actually routes to. A zero result with a note is a valid, useful outcome — OpenAI
// publishes no windows at all, and saying so beats leaving an empty field that looks like
// an oversight.
func DiscoverAuthorizedModelLimits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid model id"})
		return
	}
	found, err := business.DiscoverAuthorizedModelLimits(ctx, id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "discovery complete", "data": found})
}

// SetAuthorizedModelLimits handles POST /admin/ai/authorized-models/{id}/limits
//
// Body: { context_window_tokens, max_output_tokens }. Either may be 0 to clear it back
// to inheriting the workspace window, and either may be omitted to leave it unchanged —
// which is why the request fields are pointers: 0 is a meaningful value here, so
// "omitted" and "set to zero" have to be distinguishable.
//
// A rejected value comes back 400 with the model layer's message, which names the valid
// range. The database CHECK constraints are the real enforcement; the Go guard exists so
// an admin sees a sentence rather than a driver error.
func SetAuthorizedModelLimits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid model id"})
		return
	}
	var req adapter.SetModelLimitsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	// Read the current row so an omitted field keeps its stored value instead of being
	// silently zeroed — a PATCH-shaped body must not clear what it does not mention.
	current, err := business.GetAuthorizedModelForAdmin(ctx, id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": err.Error()})
		return
	}
	window, output := current.ContextWindowTokens, current.MaxOutputTokens
	if req.ContextWindowTokens != nil {
		window = *req.ContextWindowTokens
	}
	if req.MaxOutputTokens != nil {
		output = *req.MaxOutputTokens
	}

	if err := business.SetAuthorizedModelLimits(ctx, id, window, output); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set authorized model %s limits window=%d output=%d",
		aiAdminUserUUID(r), id, window, output)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "model limits updated"})
}

// RevokeAuthorizedModel handles DELETE /admin/ai/authorized-models/{id}
func RevokeAuthorizedModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid model id"})
		return
	}
	if err := business.RevokeAuthorizedModel(ctx, id); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s revoked authorized model %s", aiAdminUserUUID(r), id)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "authorized model revoked"})
}

// ─── AI self-test ("Test AI" in the admin panel) ───────────────────────

// RunSelfTest handles POST /admin/ai/self-test
// Starts an async self-test of the configured AI (real model calls), so the
// admin can validate it from the dashboard instead of a terminal CLI. An
// optional {model_id} tests a specific authorized model; omit it to test the
// workspace default. Returns 202 on start, 409 when one is already running.
func RunSelfTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req adapter.RunSelfTestRequest
	// Body is optional; ignore a decode error on an empty body.
	_ = json.NewDecoder(r.Body).Decode(&req)

	var modelID *uuid.UUID
	if trimmed := strings.TrimSpace(req.ModelID); trimmed != "" {
		id, err := uuid.Parse(trimmed)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid model id"})
			return
		}
		modelID = &id
	}

	started, reason := business.RunSelfTestAsync(ctx, modelID)
	if !started {
		code := http.StatusConflict
		if reason == "AI service is not enabled" {
			code = http.StatusBadRequest
		}
		helpers.WriteJSON(w, code, helpers.Envolope{"msg": reason})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s started AI self-test (model_id=%q)", aiAdminUserUUID(r), req.ModelID)
	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"status": "success", "msg": "self-test started"})
}

// GetSelfTestStatus handles GET /admin/ai/self-test/status
func GetSelfTestStatus(w http.ResponseWriter, r *http.Request) {
	status := business.GetSelfTestStatus(r.Context())
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": status})
}

// SetCodeAnalysisMaxFiles handles POST /admin/ai/code-analysis-max-files
// Sets the per-analysis file budget for the code-aware bug agent (cost lever).
func SetCodeAnalysisMaxFiles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetCodeAnalysisMaxFilesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if req.MaxFiles < 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "max_files cannot be negative"})
		return
	}
	if err := business.SetCodeAnalysisMaxFiles(ctx, req.MaxFiles); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set code_analysis_max_files=%d", aiAdminUserUUID(r), req.MaxFiles)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "code analysis budget updated"})
}

// SetIssueTriage handles POST /admin/ai/issue-triage
func SetIssueTriage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetEnabledToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetIssueTriageEnabled(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set issue_triage enabled=%v", aiAdminUserUUID(r), req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "issue triage toggled"})
}

// SetWebSearch handles POST /admin/ai/web-search — configure the
// provider-agnostic web search (provider, base URL, API key, on/off).
func SetWebSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetWebSearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetWebSearch(ctx, req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set web_search provider=%q enabled=%v", aiAdminUserUUID(r), req.Provider, req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "web search updated"})
}

// SetSandboxConfig handles POST /admin/ai/sandbox — configure the agent
// execution sandbox (runner URL, token, image digest, daily budgets, on/off).
func SetSandboxConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetSandboxConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetSandboxConfig(ctx, req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set sandbox enabled=%v runner=%q", aiAdminUserUUID(r), req.Enabled, req.RunnerURL)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "sandbox updated"})
}

// SetSandboxEnabled handles POST /admin/ai/sandbox/enabled — the instant kill
// switch that toggles only the sandbox master flag without touching config.
func SetSandboxEnabled(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetEnabledToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetSandboxEnabled(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set sandbox enabled=%v (kill switch)", aiAdminUserUUID(r), req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "sandbox toggled"})
}

// SetCodePRConfig handles POST /admin/ai/code-pr — configure the agent code-PR
// feature (runner URL, token, egress allowlist, out-of-scope policy,
// draft-on-red, daily budgets, on/off).
func SetCodePRConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetCodePRConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetCodePRConfig(ctx, req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set code-pr enabled=%v runner=%q", aiAdminUserUUID(r), req.Enabled, req.RunnerURL)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "code PR settings updated"})
}

// SetCodePREnabled handles POST /admin/ai/code-pr/enabled — the instant kill
// switch that toggles only the code-PR master flag without touching config.
func SetCodePREnabled(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetEnabledToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetCodePREnabled(ctx, req.Enabled); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set code-pr enabled=%v (kill switch)", aiAdminUserUUID(r), req.Enabled)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "code PR toggled"})
}

// SetCodePRModel handles POST /admin/ai/code-pr/model — set (or clear) the
// optional dedicated model the coding runner uses. An empty provider_id or
// model clears it (code runs then use the chat model).
func SetCodePRModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetCodePRModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetCodePRModel(ctx, req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set code-pr model provider=%s model=%q",
		aiAdminUserUUID(r), req.ProviderID, req.Model)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "code-run model updated"})
}

// GetCodePRScorecard handles GET /admin/ai/code-pr/scorecard?days=N — the
// honest reliability view of the coding agent (open/verify/in-scope/draft rates
// + ground-truth merge rate + a min-sample-guarded grade). days<=0 => all time.
func GetCodePRScorecard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	days := 0
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	view, err := business.CodePRScorecard(ctx, days)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": view})
}

// codePRLLMProxyRequest is the code-runner sidecar's completion request.
type codePRLLMProxyRequest struct {
	JobID    string `json:"job_id"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
}

// CodePRLLMProxy handles POST /internal/code-run/llm — the internal, token-gated
// endpoint the code-runner sidecar calls for model completions during a coding
// run. NOT admin-gated (the runner is not a user); authenticated by the shared
// runner token inside the business layer. Always 200 with {content} or {error}
// so the sidecar handles failures cleanly.
func CodePRLLMProxy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req codePRLLMProxyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"error": "bad request"})
		return
	}
	msgs := make([]ai.ChatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, ai.ChatMessage{Role: m.Role, Content: m.Content})
	}
	token := strings.TrimSpace(r.Header.Get("X-Runner-Token"))
	answer, err := business.CodePRLLMProxy(ctx, token, msgs, req.MaxTokens, req.Temperature)
	if err != nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"content": answer})
}

// TestCodePRRunner handles POST /admin/ai/code-pr/test — probe the configured
// coding-runner deployment (reachability + token set) without triggering a real
// coding run. Always returns 200 with a structured result.
func TestCodePRRunner(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	result := business.TestCodePRRunner(ctx)
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s tested code-pr runner ok=%v status=%s", aiAdminUserUUID(r), result.Ok, result.Status)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "result": result})
}

// ListCodePRRuns handles GET /admin/ai/code-pr/runs?limit=N — the recent coding
// runs ledger for the admin runs view (transparency).
func ListCodePRRuns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := business.RecentCodePRRuns(ctx, limit)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": runs})
}

// TestSandbox handles POST /admin/ai/sandbox/test — run a trivial probe against
// the configured code-runner sidecar to validate the deployment without
// enabling the sandbox. Always returns 200 with a structured result.
func TestSandbox(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	result := business.TestSandbox(ctx)
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s ran sandbox self-test ok=%v status=%s", aiAdminUserUUID(r), result.Ok, result.Status)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "result": result})
}

// SetAgentDelegation handles POST /admin/ai/agent-delegation
// Body: { enabled, max_hops, surfaces }.
//
// Sets the agent-to-agent delegation policy: whether one AI teammate may hand work
// to another, how deep a chain may go, and where it is permitted (comma-separated
// surface keys — "<channel-uuid>" or "task:<task-uuid>" — or "*").
//
// All three travel in one request because they are one policy. Applying them
// separately would let a save half-land: enabling delegation while a stale surface
// list is still stored opens places the admin did not just choose.
//
// Audited like every other AI governance change, and with the surfaces included,
// because "who widened this, to where" is the question asked after an incident.
func SetAgentDelegation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetAgentDelegationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetAgentDelegation(ctx, req.Enabled, req.MaxHops, req.Surfaces); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set agent delegation enabled=%v max_hops=%d surfaces=%q",
		aiAdminUserUUID(r), req.Enabled, req.MaxHops, req.Surfaces)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "agent delegation updated"})
}

// GetModelRouting handles GET /admin/ai/model-routing: the kinds of background
// work and the allowlisted model each one runs on.
func GetModelRouting(w http.ResponseWriter, r *http.Request) {
	view, err := business.GetModelRouting(r.Context())
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/GetModelRouting: %v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load model routing"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": view})
}

// SetModelRouting handles POST /admin/ai/model-routing {routes: {purpose: {provider_id, model}}}.
// A purpose left out, or with an empty model, uses the workspace default.
func SetModelRouting(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Routes map[string]aiModels.RouteTarget `json:"routes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetModelRouting(ctx, req.Routes); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set model routing %v", aiAdminUserUUID(r), req.Routes)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "model routing updated"})
}

// GetMCPServer handles GET /admin/ai/mcp-server — the current admission-control decision
// plus the tool groups that exist.
//
// The available groups come from the running registry rather than being a constant in the
// client, so a group added by a new tool appears in the admin UI with nothing to remember.
// Same reasoning as the audit log serving its own categories.
func GetMCPServer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	settings, groups, err := mcpServerBusiness.Admission(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load MCP settings"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"enabled":          settings.Enabled,
		"tool_groups":      settings.ToolGroups,
		"available_groups": groups,
	}})
}

// SetMCPServer handles POST /admin/ai/mcp-server.
//
// Audited through the same admin log every other AI governance change uses, and worth
// auditing specifically: this is the setting that decides whether external agents can
// reach the workspace at all, so "who opened this and when" is a question someone will
// eventually ask.
func SetMCPServer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req adapter.SetMCPServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := mcpServerBusiness.SetAdmission(ctx, req.Enabled, req.ToolGroups); err != nil {
		// The business error names the valid groups, so it is forwarded rather than
		// replaced: an admin who mistyped a group needs the list, not "bad request".
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.LogInfoWithContext(ctx, "AI audit: admin=%s set MCP server enabled=%v tool_groups=%q",
		aiAdminUserUUID(r), req.Enabled, req.ToolGroups)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "MCP server updated"})
}
