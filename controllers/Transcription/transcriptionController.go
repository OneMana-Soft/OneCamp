// Package controllers (Transcription) exposes the admin transcription-config
// surface plus the internal endpoint the Python transcription agent reads at
// room-join.
//
//   - GET  /admin/transcription/config → redacted admin view (admin-gated)
//   - POST /admin/transcription/config → save config (admin-gated)
//   - GET  /livekit/transcription-config → resolved config WITH decrypted
//     secrets (internal-secret gated, server-to-server only)
//
// The client-facing transcription mode is surfaced through
// controllers/Settings.GetClientConfig (so the FE reads it over the same
// /config/client channel it already uses), not here.
package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	business "github.com/akashc777/OneCamp/business/Transcription"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// GetConfig handles GET /admin/transcription/config.
func GetConfig(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.GetStatus()})
}

// UpdateConfig handles POST /admin/transcription/config. Partial: omitted
// fields are left unchanged; secret fields follow omit=keep / ""=clear /
// value=set semantics.
func UpdateConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var body struct {
		Mode              *string `json:"mode,omitempty"`
		STTProvider       *string `json:"stt_provider,omitempty"`
		STTModel          *string `json:"stt_model,omitempty"`
		STTBaseURL        *string `json:"stt_base_url,omitempty"`
		STTLanguage       *string `json:"stt_language,omitempty"`
		STTAPIKey         *string `json:"stt_api_key,omitempty"`
		GoogleCredentials *string `json:"google_credentials,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}

	// Validate enums up-front so the admin gets a precise 400 instead of a
	// silently-coerced value.
	if body.Mode != nil {
		m := strings.ToLower(strings.TrimSpace(*body.Mode))
		if m != business.ModeFrontend && m != business.ModeBackend && m != business.ModeOff {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "mode must be one of: frontend, backend, off"})
			return
		}
	}
	if body.STTProvider != nil && !business.IsValidSTTProvider(*body.STTProvider) {
		// Listed from the allow-list, not typed out here: the hardcoded version
		// of this message went stale the moment a provider was added, and told
		// an admin that the option the UI had just offered was not accepted.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "stt_provider must be one of: " + strings.Join(business.ValidSTTProviders(), ", "),
		})
		return
	}

	// A provider with no credential of its own must not leave one behind.
	// Switching to the bundled server otherwise keeps an encrypted Deepgram key
	// at rest that nothing will ever send anywhere, which is a secret held for
	// no reason. Same discipline the base URL already follows, and the cost is
	// the same: the key is re-entered if the admin switches back, which is
	// already how every secret on this page behaves.
	effectiveProvider := business.STTProvider()
	if body.STTProvider != nil {
		effectiveProvider = strings.ToLower(strings.TrimSpace(*body.STTProvider))
	}
	if !business.UsesAPIKey(effectiveProvider) {
		cleared := ""
		body.STTAPIKey = &cleared
	}
	// A custom base URL is only meaningful for the openai-compatible kind, and
	// must be a safe outbound URL (SSRF guard) — same discipline as the LLM
	// custom-endpoint and webhook paths.
	if body.STTBaseURL != nil {
		if u := strings.TrimSpace(*body.STTBaseURL); u != "" {
			if _, err := helpers.ValidateOutboundURL(u, false); err != nil {
				helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "stt_base_url is not a valid/allowed URL: " + err.Error()})
				return
			}
		}
	}

	if err := business.Save(business.SaveInput{
		Mode:              body.Mode,
		STTProvider:       body.STTProvider,
		STTModel:          body.STTModel,
		STTBaseURL:        body.STTBaseURL,
		STTLanguage:       body.STTLanguage,
		STTAPIKey:         body.STTAPIKey,
		GoogleCredentials: body.GoogleCredentials,
	}); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Transcription/UpdateConfig err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to save transcription config"})
		return
	}

	// Audit each changed field (secrets are never logged — only the fact of change).
	if body.Mode != nil {
		auditBusiness.Record(r, "transcription.mode", auditBusiness.CategorySettings,
			"Changed transcription mode", map[string]interface{}{"mode": strings.ToLower(strings.TrimSpace(*body.Mode))})
	}
	if body.STTProvider != nil {
		auditBusiness.Record(r, "transcription.stt_provider", auditBusiness.CategorySettings,
			"Changed STT provider", map[string]interface{}{"stt_provider": strings.ToLower(strings.TrimSpace(*body.STTProvider))})
	}
	if body.STTModel != nil {
		auditBusiness.Record(r, "transcription.stt_model", auditBusiness.CategorySettings,
			"Changed STT model", map[string]interface{}{"stt_model": strings.TrimSpace(*body.STTModel)})
	}
	if body.STTBaseURL != nil {
		auditBusiness.Record(r, "transcription.stt_base_url", auditBusiness.CategorySettings,
			"Changed STT base URL", map[string]interface{}{"stt_base_url": strings.TrimSpace(*body.STTBaseURL)})
	}
	if body.STTLanguage != nil {
		auditBusiness.Record(r, "transcription.stt_language", auditBusiness.CategorySettings,
			"Changed STT language", map[string]interface{}{"stt_language": strings.TrimSpace(*body.STTLanguage)})
	}
	if body.STTAPIKey != nil {
		auditBusiness.Record(r, "transcription.stt_api_key", auditBusiness.CategorySecurity,
			auditBusiness.SecretChangeSummary("STT API key", body.STTAPIKey), nil)
	}
	if body.GoogleCredentials != nil {
		auditBusiness.Record(r, "transcription.google_credentials", auditBusiness.CategorySecurity,
			auditBusiness.SecretChangeSummary("Google credentials", body.GoogleCredentials), nil)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.GetStatus()})
}

// GetAgentConfig handles GET /livekit/transcription-config — read by the Python
// transcription agent at room-join. Internal-secret gated (mounted under the
// VerifyInternalServiceRequest middleware in the router), so it is the only
// surface allowed to return decrypted STT secrets.
func GetAgentConfig(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, business.GetAgentConfig())
}

// TestConfig handles POST /admin/transcription/test — runs a live
// credential/connectivity probe against the SAVED STT config so the admin can
// verify setup before relying on it. Returns 200 with {ok:false, message} for a
// bad config (not a 5xx), and 500 only on an internal failure.
func TestConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Bound the probe rate per admin — it makes an outbound network call.
	if uid := transcriptionAdminUUID(r); uid != "" {
		res := redisStore.AllowFixedWindow(ctx, registry.TranscriptionAdminRate, []string{"test", uid}, 20)
		if !res.Allowed {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", res.RetryAfterSeconds()))
			helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": "Too many requests. Please slow down."})
			return
		}
	}

	result, err := business.TestConfig(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Transcription/TestConfig err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to run transcription test"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": result})
}

// transcriptionAdminUUID resolves the calling admin's UUID from context for
// rate-limiting. Returns "" when unavailable (the admin auth middleware already
// guarantees identity, so the limiter fails open rather than blocking).
func transcriptionAdminUUID(r *http.Request) string {
	if u, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo); ok {
		return u.UserDgraphInfo.Uuid
	}
	return ""
}
