package business

// What each call of one turn showed the model.
//
// The runner sends a turn's results back in one of two shapes. A model with
// native tool calls gets one tool message per call, under the call's own id.
// Any other model gets one "Tool results:" turn with a line per call. Both used
// to be built from lists appended to as each call finished, and the native
// messages paired the nth entry with the nth call. A needs_human call refused
// for asking a person for a credential or an internal id appended nothing. So
// every later result went back under the id of the call before it, the last
// call got an empty result, and on either path the model never read the
// refusal it was meant to correct itself from.
//
// So each result is kept by its call's place in the turn, and the follow-up is
// built from that.

import (
	"fmt"
	"strings"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// turnResults is what each call of one turn showed the model, by the call's
// place in the turn.
type turnResults struct {
	lines []string // "tool -> observation", in the order the calls were made
	obs   []string // by call index
}

func newTurnResults(calls int) *turnResults {
	return &turnResults{obs: make([]string, calls)}
}

// set records what call i, a call of tool, showed the model.
func (r *turnResults) set(i int, tool, obs string) {
	r.lines = append(r.lines, fmt.Sprintf("%s -> %s", tool, obs))
	if i >= 0 && i < len(r.obs) {
		r.obs[i] = obs
	}
}

// controlObservation is what a control tool's call shows the model: its
// error, why it was skipped, or its result.
func controlObservation(rec toolCallRecord) string {
	switch {
	case rec.Error != "":
		return "error: " + rec.Error
	case rec.Skipped != "":
		return "skipped: " + rec.Skipped
	}
	return rec.Result
}

// followUp is the turn as the model is shown it next. On the native path,
// answer carrying calls, then one tool message per call, each under its own
// id. On the text path, answer, then one user turn listing the results.
func (r *turnResults) followUp(native bool, answer string, calls []ai.ToolCall, actions []ai.ProposedAction) []ai.ChatMessage {
	if !native {
		return []ai.ChatMessage{
			{Role: "assistant", Content: answer},
			{Role: "user", Content: ai.ToolResultsTurn(strings.Join(r.lines, "\n"),
				"Continue if more steps are needed, otherwise reply with a short final summary and no tool call.")},
		}
	}
	out := []ai.ChatMessage{{Role: "assistant", Content: answer, ToolCalls: calls}}
	for idx, a := range actions {
		id := ""
		if idx < len(calls) {
			id = calls[idx].ID
		}
		obs := ""
		if idx < len(r.obs) {
			obs = r.obs[idx]
		}
		out = append(out, ai.ChatMessage{Role: "tool", ToolCallID: id, Name: a.ToolName, Content: obs})
	}
	return out
}
