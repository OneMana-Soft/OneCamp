//go:build integration
// +build integration

package integration_test

// Every change to the workspace's AI is in the admin audit log (V-269),
// against real Postgres 12: adding, editing and removing a custom provider,
// authorizing, limiting, switching off and revoking a model, deleting a model
// from the local Ollama, the workspace's AI limits, and the runners of the code
// sandbox and of code pull requests. These wrote only an "AI audit:" log line
// before, and five of them wrote nothing at all. A provider's API key is never
// in a row, nor a runner's token, nor a key in an address's credentials or query.
//
// Run: go test -tags=integration ./tests/integration/ -run TestEveryAIChangeIsAudited

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	aiController "github.com/akashc777/OneCamp/controllers/AI"
)

// aiAuditRow is one admin audit row: its category, who made the change, and
// its metadata, decoded and as stored.
type aiAuditRow struct {
	category, actorID, raw string
	meta                   map[string]any
}

// oneAuditRow waits until the admin audit log holds a row of action, and
// answers it. A second row of the same action fails the test: each change is
// recorded once.
func oneAuditRow(t *testing.T, db *sql.DB, action string) aiAuditRow {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		rows, err := db.Query(`SELECT category, COALESCE(actor_id::text, ''), COALESCE(metadata::text, '{}') FROM admin_audit_log WHERE action = $1`, action)
		if err != nil {
			t.Fatal(err)
		}
		var got []aiAuditRow
		for rows.Next() {
			var row aiAuditRow
			if err := rows.Scan(&row.category, &row.actorID, &row.raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(row.raw), &row.meta); err != nil {
				t.Fatalf("%s metadata is not a JSON object: %v (%s)", action, err, row.raw)
			}
			got = append(got, row)
		}
		rows.Close()
		switch {
		case len(got) > 1:
			t.Fatalf("the audit log has %d %s rows, want 1", len(got), action)
		case len(got) == 1:
			return got[0]
		case time.Now().After(deadline):
			t.Fatalf("the audit log has no %s row", action)
		}
	}
}

func TestEveryAIChangeIsAudited(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("APP_SECRET_KEK", "ai-admin-audit-integration-kek")
	// The local Ollama a model is deleted from: the built-in provider reads
	// its address from OLLAMA_HOST.
	var mu sync.Mutex
	var removed []string
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/delete" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		removed = append(removed, body.Model)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ollama.Close()
	t.Setenv("OLLAMA_HOST", ollama.URL)

	lr := setupLandReady(t)
	db := lr.env.PG
	owner := lr.owner.UserPostgresInfo.Id.String()
	const firstKey, rotatedKey = "sk-v269-first-key-never-recorded", "sk-v269-rotated-key-never-recorded"

	// check holds a row to the settings category, the owner as its actor, and
	// the metadata values want names.
	check := func(t *testing.T, action string, want map[string]any) aiAuditRow {
		t.Helper()
		row := oneAuditRow(t, db, action)
		if row.category != "settings" {
			t.Fatalf("%s is filed under %q, want settings", action, row.category)
		}
		if row.actorID != owner {
			t.Fatalf("%s names actor %q, want the owner %s", action, row.actorID, owner)
		}
		for k, v := range want {
			if got := fmt.Sprint(row.meta[k]); got != fmt.Sprint(v) {
				t.Fatalf("%s metadata %s = %q, want %q (%s)", action, k, got, fmt.Sprint(v), row.raw)
			}
		}
		return row
	}

	var providerID, modelID string
	t.Run("a custom provider added", func(t *testing.T) {
		act(t, lr, aiController.CreateProvider, http.MethodPost, map[string]any{
			"label": "Team gateway", "base_url": "https://models.example.test/v1?key=sk-v269-query-key", "api_key": firstKey,
		})
		if err := db.QueryRow(`SELECT id::text FROM ai_providers WHERE label = 'Team gateway'`).Scan(&providerID); err != nil {
			t.Fatal(err)
		}
		check(t, "ai.provider.created", map[string]any{
			"provider_id": providerID, "label": "Team gateway", "base_url": "https://models.example.test/v1",
		})
	})
	t.Run("a model authorized, switched off and limited", func(t *testing.T) {
		act(t, lr, aiController.AuthorizeModel, http.MethodPost, map[string]any{
			"provider_id": providerID, "model": "team-large", "label": "Large",
		})
		if err := db.QueryRow(`SELECT id::text FROM ai_authorized_models WHERE provider_id = $1 AND model = 'team-large'`, providerID).Scan(&modelID); err != nil {
			t.Fatal(err)
		}
		check(t, "ai.model.authorized", map[string]any{"provider_id": providerID, "model": "team-large"})
		act(t, lr, aiController.SetAuthorizedModelEnabled, http.MethodPatch, map[string]any{"enabled": false}, "id", modelID)
		check(t, "ai.model.enabled", map[string]any{"model_id": modelID, "enabled": false})
		act(t, lr, aiController.SetAuthorizedModelLimits, http.MethodPatch, map[string]any{
			"context_window_tokens": 32768, "max_output_tokens": 4096,
		}, "id", modelID)
		check(t, "ai.model.limits", map[string]any{"model_id": modelID, "context_window": 32768, "max_output": 4096})
	})
	t.Run("the model revoked", func(t *testing.T) {
		act(t, lr, aiController.RevokeAuthorizedModel, http.MethodDelete, nil, "id", modelID)
		check(t, "ai.model.revoked", map[string]any{"model_id": modelID})
	})
	t.Run("the provider edited, its key changed, and removed", func(t *testing.T) {
		act(t, lr, aiController.UpdateProvider, http.MethodPatch, map[string]any{
			"label": "Team gateway (EU)", "api_key": rotatedKey, "enabled": false,
		}, "providerId", providerID)
		check(t, "ai.provider.updated", map[string]any{
			"provider_id": providerID, "label": "Team gateway (EU)", "enabled": false, "api_key_changed": true,
		})
		act(t, lr, aiController.DeleteProvider, http.MethodDelete, nil, "providerId", providerID)
		check(t, "ai.provider.deleted", map[string]any{"provider_id": providerID})
	})
	t.Run("a model deleted from the local Ollama", func(t *testing.T) {
		const localOllama = "00000000-0000-0000-0000-0000000000a1"
		act(t, lr, aiController.DeleteModel, http.MethodDelete, map[string]any{"provider_id": localOllama, "model": "old-model:7b"})
		mu.Lock()
		got := fmt.Sprint(removed)
		mu.Unlock()
		if got != "[old-model:7b]" {
			t.Fatalf("Ollama was asked to delete %s, want [old-model:7b]", got)
		}
		check(t, "ai.model.deleted", map[string]any{"provider_id": localOllama, "model": "old-model:7b"})
	})
	t.Run("the workspace's AI limits, which recorded nothing before", func(t *testing.T) {
		act(t, lr, aiController.SetRateLimit, http.MethodPost, map[string]any{"rate_limit_per_min": 120})
		check(t, "ai.rate_limit.set", map[string]any{"per_minute": 120})
		act(t, lr, aiController.SetContextWindow, http.MethodPost, map[string]any{"context_window_tokens": 8192})
		check(t, "ai.context_window.set", map[string]any{"tokens": 8192})
		act(t, lr, aiController.SetWorkspaceTokenBudget, http.MethodPost, map[string]any{"tokens": 500000})
		check(t, "ai.workspace_token_budget.set", map[string]any{"tokens": 500000})
		act(t, lr, aiController.SetUserTokenBudget, http.MethodPost, map[string]any{"tokens": 50000})
		check(t, "ai.user_token_budget.set", map[string]any{"tokens": 50000})
	})
	// A runner's address may carry its token in the credentials or the query;
	// each route keeps the address without them, and the token as changed.
	t.Run("the code sandbox's runner set, a token in its address", func(t *testing.T) {
		act(t, lr, aiController.SetSandboxConfig, http.MethodPost, map[string]any{
			"enabled": false, "runner_url": "https://runner:sk-v269-sandbox-pass@sandbox.example.test/run?key=sk-v269-sandbox-query",
			"runner_token": "sk-v269-sandbox-token",
		})
		check(t, "ai.sandbox.set", map[string]any{
			"enabled": false, "runner_url": "https://sandbox.example.test/run", "runner_token_changed": true,
		})
	})
	t.Run("code pull requests' runner set, a token in its address", func(t *testing.T) {
		act(t, lr, aiController.SetCodePRConfig, http.MethodPost, map[string]any{
			"enabled": false, "runner_url": "https://runner:sk-v269-code-pass@code.example.test/pr?key=sk-v269-code-query",
			"runner_token": "sk-v269-code-token",
		})
		check(t, "ai.code_pr.set", map[string]any{
			"enabled": false, "runner_url": "https://code.example.test/pr", "runner_token_changed": true,
		})
	})
	t.Run("no row carries a key", func(t *testing.T) {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE metadata::text LIKE '%sk-v269%' OR summary LIKE '%sk-v269%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d audit rows carry a provider's API key or a runner's token", n)
		}
	})
}
