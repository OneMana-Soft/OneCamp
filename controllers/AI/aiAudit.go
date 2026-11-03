package controllers

import (
	"net/http"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// aiMeta is an AI change's audit metadata: ids, flags, sizes and limits,
// never a secret (no API key, header, pattern text or instruction text).
type aiMeta = map[string]interface{}

// recordAIChange keeps a change to the workspace's AI in the admin audit log
// (V-269): which provider or model was added, authorized, limited, switched or
// removed, and every AI setting set, as action "ai.<what>" in the settings
// category, by whoever made the request. It also writes the "AI audit:" log
// line those changes have always had. TestEveryAIChangeIsRecorded holds that
// every handler here named for a write (aiWriteVerb) calls it, unless
// aiWritesNotRecorded says why not, and that every handler calling it is named
// for a write, so no recorded change can lose its row unseen.
func recordAIChange(r *http.Request, action, summary string, meta aiMeta) {
	helpers.LogInfoWithContext(r.Context(), "AI audit: %s by=%s %v", action, aiAdminUserUUID(r), meta)
	auditBusiness.Record(r, action, auditBusiness.CategorySettings, summary, meta)
}

// providerUpdateMeta is a provider edit's audit metadata: the provider and
// what the edit set. A new API key is recorded as changed, never its value,
// and an address without its credentials or query (auditBusiness.URLForRecord).
func providerUpdateMeta(id uuid.UUID, req adapter.UpdateProviderRequest) aiMeta {
	meta := aiMeta{"provider_id": id.String(), "api_key_changed": req.APIKey != nil}
	if req.Label != nil {
		meta["label"] = *req.Label
	}
	if req.BaseURL != nil {
		meta["base_url"] = auditBusiness.URLForRecord(*req.BaseURL)
	}
	if req.Enabled != nil {
		meta["enabled"] = *req.Enabled
	}
	if req.InsecureTLS != nil {
		meta["insecure_tls"] = *req.InsecureTLS
	}
	return meta
}

// runnerMeta is a runner setting's audit metadata, for the code sandbox and for
// code pull requests alike: whether it is on, the runner's address without its
// credentials, query or fragment (auditBusiness.URLForRecord), and whether the
// request set or cleared the runner's token, never the token itself. A runner
// address may carry a token (https://user:token@runner/?key=...), so both
// runner routes record through here.
func runnerMeta(enabled bool, runnerURL, runnerToken string, clearToken bool) aiMeta {
	return aiMeta{
		"enabled":              enabled,
		"runner_url":           auditBusiness.URLForRecord(runnerURL),
		"runner_token_changed": clearToken || strings.TrimSpace(runnerToken) != "",
	}
}
