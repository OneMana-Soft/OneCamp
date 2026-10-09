package business

// Per-agent knowledge sources: a curated set of channels/docs/projects the
// agent is always grounded on. At run time we pull text from each source AS THE
// OWNER (permissions re-checked by the same reads the assistant uses;
// inaccessible sources are silently skipped), bounded in size, and prepend it to
// the agent's system prompt. Additive: an agent with no sources is unchanged.
//
// The grounding is read for the run, so a run someone other than the owner
// asked for reads it within their reach too: a source they could not open
// themselves is skipped like any other inaccessible one, rather than being put
// in front of a model that answers them.

import (
	"context"
	"encoding/json"
	"strings"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	knowledgePerSourceMaxChars = 1500
	knowledgeTotalMaxChars     = 6000
	knowledgeChannelMaxMsgs    = 20
)

// buildKnowledgeContext returns the grounding block for an agent's knowledge
// sources, or "" when there are none or nothing is readable. Bounded in total
// size so it can't blow the context budget (which also re-bounds downstream).
func buildKnowledgeContext(ctx context.Context, agent *model.AiAgent) string {
	raw := strings.TrimSpace(agent.Knowledge)
	if raw == "" || raw == "[]" {
		return ""
	}
	var refs []KnowledgeRef
	if err := json.Unmarshal([]byte(raw), &refs); err != nil || len(refs) == 0 {
		return ""
	}
	ownerInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, agent.CreatedBy.String())
	if err != nil || ownerInfo == nil {
		return ""
	}

	gate := newRequesterGate(ctx)
	var b strings.Builder
	total := 0
	for _, ref := range refs {
		text := strings.TrimSpace(fetchKnowledge(ctx, gate, ownerInfo, ref))
		if text == "" {
			continue
		}
		if len(text) > knowledgePerSourceMaxChars {
			text = text[:knowledgePerSourceMaxChars] + "…"
		}
		label := strings.TrimSpace(ref.Label)
		if label == "" {
			label = ref.Type
		}
		block := "## " + label + "\n" + text + "\n\n"
		if total+len(block) > knowledgeTotalMaxChars {
			break
		}
		b.WriteString(block)
		total += len(block)
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\nReference knowledge you are grounded on (read-only context):\n" + b.String()
}

// fetchKnowledge reads one source AS THE OWNER, reusing the existing
// permission-checked read paths/executors. Unknown/inaccessible -> "". A channel
// is read through the semantic index, which narrows to the run's asker itself.
func fetchKnowledge(ctx context.Context, gate *requesterGate, ownerInfo *userModels.UserInfo, ref KnowledgeRef) string {
	switch ref.Type {
	case "channel":
		return aiBusiness.GetRecentChannelTranscript(ctx, ownerInfo, ref.Id, knowledgeChannelMaxMsgs)
	case "doc":
		return runReadExecutor(ctx, gate, ownerInfo, "read_doc", map[string]string{"doc_uuid": ref.Id})
	case "project":
		return runReadExecutor(ctx, gate, ownerInfo, "read_project", map[string]string{"project_uuid": ref.Id})
	}
	return ""
}

// runReadExecutor invokes a read-only tool executor as the owner (permissions
// re-checked inside the executor) and returns its text, or "" on any error or
// when the run's asker could not read it themselves.
func runReadExecutor(ctx context.Context, gate *requesterGate, ownerInfo *userModels.UserInfo, tool string, params map[string]string) string {
	action := ai.ProposedAction{ToolName: tool, Params: params}
	if gate.refusal(ctx, action) != "" {
		return ""
	}
	exec, ok := ai.GetExecutor(tool)
	if !ok {
		return ""
	}
	res, _, err := exec(ctx, action, ownerInfo.UserDgraphInfo.Uuid)
	if err != nil {
		return ""
	}
	return res
}
