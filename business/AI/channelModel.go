package business

// Which model AI uses INSIDE a channel.
//
// Migration 121 lets an admin pin a model per channel — a cheap fast one for a
// routine-digest channel, a stronger one for a customer-facing channel. The agent
// runner honoured it. Nothing else did.
//
// So an admin could pin a model to #support, watch an agent run there use it, and have
// every other AI feature in that same channel quietly use the workspace default:
// summarising the channel, and answering an @mention in it (streaming or not). From the
// admin's side the setting is named for the channel and appears to apply to the channel,
// which makes the exceptions impossible to discover.
//
// It matters more than a preference for two reasons. Cost, because the channel a
// workspace most wants on a cheap model is usually its busiest, and summarise/@mention
// are its highest-volume AI calls — precisely the ones that were ignoring the setting.
// And residency: the reason to pin a channel to a locally-hosted model is often that its
// content must not leave the network, and ResolveExplicitModel already honours a
// local-only constraint. A setting that holds for agent runs and not for summaries is a
// setting nobody should rely on for either.
//
// ONE RESOLVER, SHARED. The runner's version was fifteen lines of nested lookups inline
// in the middle of run setup. Copying that into three more call sites would be four
// places to keep saying the same thing about precedence and fallback, so it moved here
// and the runner now calls it too.
//
// FALLS BACK RATHER THAN FAILING, at every step: no pin, an unparseable id, a model the
// admin has since removed from the allowlist, a model that is not usable, a residency
// rule that forbids it. Every one of those returns the workspace default. A channel
// setting is a preference about WHICH model, never a veto on whether AI works at all —
// degrading to the default is always better than refusing to summarise.

import (
	"context"

	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// ChannelScopedLLM returns the client an AI call made INSIDE a channel should use,
// together with its circuit breaker, the model's label, and that model's TOKEN LIMITS.
//
// Always returns a usable client: the channel's pinned model when one is set, allowlisted
// and permitted, and the workspace default otherwise. label is the pinned model's name
// when the override took effect and "" when the default is in use, so a caller can report
// which model answered without asking again.
//
// The limits are returned rather than left for the caller to look up because every
// return path here already knows which model won, and a caller that has to ask again is
// a caller that can forget to. That is not hypothetical: the prompt budget used to be
// read from the workspace config on exactly these paths, so a channel pinned to a
// large-window model was budgeted as though it were the default one. The limits come
// back with the client so using the right client and the wrong budget is no longer
// something a call site can do by omission.
//
// A blank channelUUID is not an error — plenty of AI calls have no channel scope — and
// simply yields the default.
func ChannelScopedLLM(ctx context.Context, channelUUID string) (ai.LLMProvider, *ai.CircuitBreaker, string, ai.ModelLimits) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, nil, "", ai.WorkspaceLimits()
	}
	defLLM, defCB := svc.LLM, svc.Resiliency.CB
	defLimits := ai.WorkspaceLimits()

	chID, err := uuid.Parse(channelUUID)
	if err != nil {
		return defLLM, defCB, "", defLimits
	}

	// Read from the domain rather than business/Channel: this needs one column, and
	// depending on the channel business layer from the AI business layer would couple
	// two large packages for a single lookup.
	mid, merr := channelDomain.GetChannelAIModel(ctx, chID)
	if merr != nil || mid == nil {
		return defLLM, defCB, "", defLimits
	}

	// The pin is a SOFT reference into the admin allowlist (no FK, per migration 121),
	// so the model may have been disabled or deleted since it was pinned. Both cases
	// degrade to the default rather than failing the call.
	am, aerr := aiModels.GetAuthorizedModel(ctx, *mid)
	if aerr != nil || am == nil || !am.Usable() {
		return defLLM, defCB, "", defLimits
	}

	// ResolveExplicitModel reports usedDefault=true when it declined — including when a
	// residency rule forbids this model — and in that case there is no override to
	// report, so the label stays empty and the caller says the default answered. The
	// limits must follow the same fork: a refused pin means the DEFAULT model is
	// answering, so budgeting for the pinned model's window would size the prompt for a
	// model that is not going to see it.
	llm, cb, usedDefault := svc.ResolveExplicitModel(ctx, am.ProviderID, am.Model)
	if usedDefault {
		return defLLM, defCB, "", defLimits
	}
	return llm, cb, am.Model, ai.LimitsForModel(am.ProviderKind, am.Model, am.ContextWindowTokens, am.MaxOutputTokens)
}
