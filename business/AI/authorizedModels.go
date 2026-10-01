package business

// Business logic for the admin-authorized model allowlist (migration 81) and
// per-user model choice.
//
// The admin authorizes a SET of models drawn from the configured providers;
// members may then pick one for their personal AI assistant. The workspace
// default (ai_settings chat selection) is always usable and is the fallback
// whenever a member has no pick or their pick was revoked. These functions are
// thin, validated wrappers over the model layer so controllers stay free of
// SQL and the validation lives in one place.

import (
	"context"
	"fmt"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// ListAuthorizedModels returns the full allowlist for the admin UI (includes
// disabled rows and rows whose provider is disabled, so the admin sees the
// real state).
func ListAuthorizedModels(ctx context.Context) ([]*aiModels.AuthorizedModel, error) {
	return aiModels.ListAuthorizedModels(ctx)
}

// AuthorizeModel adds (or re-enables) a model in the allowlist. The provider
// must exist; re-authorizing an existing (provider, model) is idempotent.
func AuthorizeModel(ctx context.Context, providerID uuid.UUID, model, label string) (*aiModels.AuthorizedModel, error) {
	return aiModels.CreateAuthorizedModel(ctx, providerID, model, label)
}

// SetAuthorizedModelEnabled toggles one allowlist entry.
func SetAuthorizedModelEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	return aiModels.SetAuthorizedModelEnabled(ctx, id, enabled)
}

// SetAuthorizedModelLimits records a model's own token limits (migration 140). 0 for
// either value means "inherit the workspace window", which is also the default.
//
// Reloads the AI service, unlike enabling or revoking a model. Those two are re-checked
// per request, so a cached client stays correct. A context window is not: it is baked
// into the client at construction (Ollama applies num_ctx there), and the resolver caches
// clients for the life of a service instance. Without the reload an admin could raise a
// model's window, see the new value in the UI, and have every request keep running at the
// old one until the next restart — the exact "the setting exists but does nothing" defect
// this whole change is fixing.
func SetAuthorizedModelLimits(ctx context.Context, id uuid.UUID, contextWindowTokens, maxOutputTokens int) error {
	if err := aiModels.SetAuthorizedModelLimits(ctx, id, contextWindowTokens, maxOutputTokens); err != nil {
		return err
	}
	return ai.ReloadAIService(ctx)
}

// GetAuthorizedModelForAdmin returns one allowlist entry for the admin UI, including
// disabled rows — the admin edits limits on models that are currently switched off too.
func GetAuthorizedModelForAdmin(ctx context.Context, id uuid.UUID) (*aiModels.AuthorizedModel, error) {
	return aiModels.GetAuthorizedModel(ctx, id)
}

// DiscoverAuthorizedModelLimits asks a model's provider what its token limits are, so an
// admin does not have to know them from memory.
//
// Returns a suggestion, never a write. Discovery reaches an external endpoint whose answer
// may be stale, may describe a different model than a gateway actually routes to, or may
// be absent entirely — OpenAI publishes no windows. All three are fine when a person is
// deciding, and none of them is fine applied silently.
func DiscoverAuthorizedModelLimits(ctx context.Context, id uuid.UUID) (ai.DiscoveredLimits, error) {
	m, err := aiModels.GetAuthorizedModel(ctx, id)
	if err != nil {
		return ai.DiscoveredLimits{}, err
	}
	ep, cfg, err := ai.EndpointForAuthorizedModel(ctx, m.ProviderID, m.Model)
	if err != nil {
		return ai.DiscoveredLimits{Note: "Could not resolve this model's provider: " + err.Error()}, nil
	}
	return ai.DiscoverModelLimits(ctx, ep, cfg), nil
}

// RevokeAuthorizedModel removes a model from the allowlist; affected users
// silently revert to the workspace default (FK ON DELETE SET NULL).
func RevokeAuthorizedModel(ctx context.Context, id uuid.UUID) error {
	return aiModels.DeleteAuthorizedModel(ctx, id)
}

// UserModelsResponse is the user-facing picker payload: the models a member
// may choose from (usable only) plus their current selection. SelectedModelID
// is empty when the member is on the workspace default.
type UserModelsResponse struct {
	Models          []adapter.UserModelOption `json:"models"`
	SelectedModelID string                    `json:"selected_model_id"`
}

// ListUsableModelsForUser returns the picker payload for a member: every
// authorized model whose provider is also enabled, plus the member's current
// selection (empty = workspace default).
func ListUsableModelsForUser(ctx context.Context, userUUID string) (*UserModelsResponse, error) {
	all, err := aiModels.ListAuthorizedModels(ctx)
	if err != nil {
		return nil, err
	}
	out := &UserModelsResponse{Models: make([]adapter.UserModelOption, 0, len(all))}
	for _, m := range all {
		if !m.Usable() {
			continue
		}
		out.Models = append(out.Models, adapter.UserModelOption{
			ID:           m.ID.String(),
			Model:        m.Model,
			Label:        m.DisplayLabel(),
			ProviderName: m.ProviderName,
			ProviderKind: m.ProviderKind,
		})
	}

	pref, err := aiModels.GetUserModelPreference(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	if pref != nil {
		out.SelectedModelID = pref.ID.String()
	}
	return out, nil
}

// SetUserModelPreference sets (or clears, when modelID is nil) a member's
// chosen model. The model layer validates that modelID is an authorized,
// usable entry, so a member can never bind to an unauthorized model.
func SetUserModelPreference(ctx context.Context, userUUID string, modelID *uuid.UUID) error {
	return aiModels.SetUserModelPreference(ctx, userUUID, modelID)
}

// maxUserInstructionsLen bounds a member's personal AI custom instructions so
// they can't blow the assistant prompt budget or become an injection lever.
const maxUserInstructionsLen = 2000

// GetUserCustomInstructions returns a member's personal AI custom instructions
// ("" when unset).
func GetUserCustomInstructions(ctx context.Context, userUUID string) (string, error) {
	return aiModels.GetUserCustomInstructions(ctx, userUUID)
}

// SetUserCustomInstructions stores a member's personal AI custom instructions,
// trimmed and length-capped. An empty string clears them.
func SetUserCustomInstructions(ctx context.Context, userUUID, instructions string) error {
	instructions = strings.TrimSpace(instructions)
	if len(instructions) > maxUserInstructionsLen {
		instructions = instructions[:maxUserInstructionsLen]
	}
	return aiModels.SetUserCustomInstructions(ctx, userUUID, instructions)
}

// applyUserCustomInstructions appends a member's personal custom instructions to
// a base system prompt, clearly delimited and placed AFTER the base rules so a
// member can shape tone/role/defaults but never override the assistant's safety,
// grounding, or tool-use rules. Returns base unchanged when instructions are
// blank. Generic: any prompt-building path can reuse it. Pure.
func applyUserCustomInstructions(base, instructions string) string {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return base
	}
	if len(instructions) > maxUserInstructionsLen {
		instructions = instructions[:maxUserInstructionsLen]
	}
	return base + "\n\nThe user has set personal preferences for how you respond (apply them only where they do not conflict with the rules above):\n" + instructions
}

// getAuthorizedModelForTest fetches an allowlist entry by id and ensures it is
// actually usable (it and its provider enabled) before the self-test exercises
// it, giving a clear error otherwise.
func getAuthorizedModelForTest(ctx context.Context, id uuid.UUID) (*aiModels.AuthorizedModel, error) {
	m, err := aiModels.GetAuthorizedModel(ctx, id)
	if err != nil {
		return nil, err
	}
	if !m.Usable() {
		return nil, fmt.Errorf("model %q is not available (it or its provider is disabled)", m.DisplayLabel())
	}
	return m, nil
}
