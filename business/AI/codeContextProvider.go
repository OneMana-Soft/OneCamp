package business

// codeContextProvider is OneCamp's org-context source for the read-only code
// agent: it turns the workspace's *why* — remembered decisions, conventions
// (glossary), and the standing instructions given to the agent in the
// originating conversation — into permission-scoped fragments that ground a code
// analysis in the org's intent. This is the structural edge a repo-only or cloud
// coding agent structurally cannot have.
//
// It implements codeagent.OrgContextProvider and is registered at startup
// (RegisterCodeExecutors). It is:
//   - Permission-scoped: everything is retrieved AS the acting user via the same
//     ListWorkspaceMemory access rule the memory panel uses; it can never widen
//     what that user may see.
//   - OFF-safe: no actor on the subject, AI/memory disabled, or nothing found =>
//     it returns nil, and the code agent's prompt is byte-identical to before.
//   - Best-effort + bounded: any retrieval miss is swallowed; the final block is
//     capped by codepr.AssembleContext, so context can never fail or dominate an
//     analysis.

import (
	"context"
	"strings"

	codeagent "github.com/akashc777/OneCamp/business/CodeAgent"
	codepr "github.com/akashc777/OneCamp/business/CodePR"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// codeContextMemoryLimit bounds how many workspace-memory items we pull for one
// analysis before assembly trims further — small, since prompt tokens dominate
// latency and AssembleContext dedups/prioritizes anyway.
const codeContextMemoryLimit = 12

// codeContextConversationMessages bounds how many recent messages of the
// originating conversation we pull. The transcript reader is already
// token-bounded; AssembleContext fair-shares it further.
const codeContextConversationMessages = 20

// Fragment weights (AssembleContext orders by weight desc): the discussion that
// produced the request is the richest "why", then the standing instructions the
// team gave the agent, then durable decisions, then glossary/conventions.
const (
	weightOriginConversation  = 25
	weightStandingInstruction = 20
	weightDecision            = 10
	weightGlossary            = 5
)

type codeContextProvider struct{}

// runSponsor is whose agent the run making this call is: the sponsor its
// requester is bound to, or, when nobody else asked, actingUUID, the person
// the call executes as (the sponsor, in an agent's own runs; the member, when
// they use the assistant themselves). It is never the person the run acts
// for, whose instructions AgentScopedMemoryBlock adds itself.
func runSponsor(ctx context.Context, actingUUID string) string {
	if _, sponsor, forOther := ai.RunRequester(ctx); forOther && strings.TrimSpace(sponsor) != "" {
		return sponsor
	}
	return actingUUID
}

// Fragments returns permission-scoped org-context fragments for a code analysis.
// Returns nil (OFF-safe) unless there is an acting user, AI + the memory layer
// are enabled, and something relevant is found.
func (codeContextProvider) Fragments(ctx context.Context, subj codeagent.ContextSubject) []codepr.ContextFragment {
	actor := strings.TrimSpace(subj.ActorUUID)
	if actor == "" {
		return nil // no identity to scope retrieval by — stay OFF-safe
	}

	// Gate on the workspace having AI + the memory layer enabled, so a workspace
	// that opted out of memory never has it leak into coding prompts.
	settings, err := getAISettingsForMemory(ctx)
	if err != nil || !settings.enabled || !settings.memoryEnabled {
		return nil
	}

	userInfo, err := BuildUserInfoByUserUUID(ctx, actor)
	if err != nil || userInfo == nil {
		return nil
	}

	var fragments []codepr.ContextFragment

	// 1. Durable workspace decisions + conventions the acting user can see.
	//    These carry the org's settled choices ("we use X, never Y") that a
	//    fix must respect. Scoped by the user's accessible resources.
	if resp, lerr := ListWorkspaceMemory(ctx, userInfo,
		[]string{memoryModels.KindDecision, memoryModels.KindGlossary},
		[]string{memoryModels.StatusOpen}, codeContextMemoryLimit, ""); lerr == nil && resp != nil {
		for _, it := range resp.Items {
			body := strings.TrimSpace(it.Content)
			if body == "" {
				continue
			}
			weight := weightGlossary
			if it.Kind == memoryModels.KindDecision {
				weight = weightDecision
			}
			fragments = append(fragments, codepr.ContextFragment{
				Kind:   codepr.KindMemory,
				Title:  memoryFragmentTitle(it.Kind, it.ScopeLabel),
				Body:   body,
				Weight: weight,
			})
		}
	}

	// 2. Standing instructions the team gave the agent in THIS conversation
	//    (channel/DM the request came from), if we know it — directive "how the
	//    team wants the agent to behave here" facts. The ones the agent's
	//    sponsor gave, and the asker's when someone else asked
	//    (AgentScopedMemoryBlock).
	if block := AgentScopedMemoryBlock(ctx, subj.ChannelUUID, subj.ChatGrpID, runSponsor(ctx, actor)); strings.TrimSpace(block) != "" {
		fragments = append(fragments, codepr.ContextFragment{
			Kind:   codepr.KindMemory,
			Title:  "Standing instructions for this conversation",
			Body:   strings.TrimSpace(block),
			Weight: weightStandingInstruction,
		})
	}

	// 3. The ORIGINATING CONVERSATION — the recent discussion in the channel/DM
	//    the request came from. This is the structural edge: the "why" behind
	//    the request that a repo-only or cloud agent structurally cannot see.
	//    Permission-scoped + token-bounded by the transcript reader (returns ""
	//    when the user can't see the conversation or there are no messages).
	var transcript string
	switch {
	case subj.ChannelUUID != "":
		transcript = GetRecentChannelTranscript(ctx, userInfo, subj.ChannelUUID, codeContextConversationMessages)
	case subj.ChatGrpID != "":
		transcript = GetRecentChatTranscript(ctx, userInfo, subj.ChatGrpID, codeContextConversationMessages)
	}
	if strings.TrimSpace(transcript) != "" {
		fragments = append(fragments, codepr.ContextFragment{
			Kind:   codepr.KindOriginConversation,
			Title:  "Recent discussion in this conversation",
			Body:   strings.TrimSpace(transcript),
			Weight: weightOriginConversation,
		})
	}

	return fragments
}

// memoryFragmentTitle renders a compact, human header for a memory fragment:
// the scope (channel/project name) when known, else the kind.
func memoryFragmentTitle(kind, scopeLabel string) string {
	scopeLabel = strings.TrimSpace(scopeLabel)
	label := "Decision"
	if kind == memoryModels.KindGlossary {
		label = "Convention"
	}
	if scopeLabel != "" {
		return label + " · " + scopeLabel
	}
	return label
}
