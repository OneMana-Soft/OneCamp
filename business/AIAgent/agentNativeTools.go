package business

import (
	"os"
	"strings"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// Native function calling for agent runs.
//
// The text `<tool_call>` protocol asks a model to emit a JSON block in free
// text and parses it back out. Weak models fail this badly: they narrate a plan
// ("I'll search… please wait…"), dump chain-of-thought, or fabricate a result
// instead of emitting the block. Native function calling (the OpenAI/Groq/vLLM
// `tools` API, Anthropic tool_use, etc.) makes tool use a STRUCTURED turn: the
// model returns a tool_calls array, we run them and thread real results back as
// tool-role messages, and it cannot produce a final answer claiming results it
// was never given. This is how Claude/GPT agents avoid the fabrication you see
// with the text protocol.
//
// It is used only when the provider supports it (ai.ToolCallingProvider) and is
// enabled; otherwise the runner falls back to the text path unchanged.

// agentNativeToolsEnabled reports whether the agent loop should use native
// function calling when the provider supports it. Defaults ON (the reliable
// path); set AI_AGENT_NATIVE_TOOLS=false to force the legacy text protocol
// (e.g. to isolate a provider-side tools bug) without a redeploy of logic.
func agentNativeToolsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AI_AGENT_NATIVE_TOOLS")))
	return v != "false" && v != "0" && v != "off" && v != "no"
}

// nativeControlToolSpecs returns the two registry-free control tools the runner
// handles specially (needs_human, save_progress) as native tool specs, so an
// agent on the native path can still pause for a human or checkpoint progress —
// capabilities that are otherwise injected as text-prompt directives and would
// be unavailable once the text tool prompt is dropped for native mode.
//
// progress is false for a run someone other than the sponsor asked for, which
// may not write the notes every run of the agent reads (see the runner).
func nativeControlToolSpecs(scoped, progress bool) []ai.ToolSpec {
	specs := []ai.ToolSpec{
		{
			Name:        blockerToolName, // "needs_human"
			Description: "Pause and ask a human when you are blocked and cannot proceed without a decision or missing information. Do NOT guess or invent an answer — call this instead. Never ask for a password, key, token or other credential, and never ask for an internal id or UUID: find those with your tools.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"reason": map[string]interface{}{
						"type":        "string",
						"description": "What you need from a person, phrased as a clear question.",
					},
					// The MCP elicitation "Enum Schema", flattened to the single
					// field a chat reply can carry. Offering the answers turns a
					// question into something a person can tap, and turns their
					// reply into a fact instead of something to re-interpret.
					"options": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "Optional. When the answer is one of a few known possibilities, list 2 to 6 short labels for a person to choose from. Omit for an open question.",
					},
				},
				"required": []string{"reason"},
			},
		},
	}
	if progress {
		specs = append(specs, ai.ToolSpec{
			Name:        progressToolName, // "save_progress"
			Description: "Save a concise note of what is done and what remains, to carry progress across runs. Saves silently; you then continue.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"notes": map[string]interface{}{
						"type":        "string",
						"description": "A short, self-contained summary of progress so far and what remains.",
					},
				},
				"required": []string{"notes"},
			},
		})
	}
	// Conversational memory is only meaningful when the run has a channel/DM to
	// scope it to; advertise it only then so a scheduled/manual run doesn't
	// spend prompt tokens on an unusable tool.
	if scoped {
		specs = append(specs,
			ai.ToolSpec{
				Name:        rememberToolName, // "remember"
				Description: "Save a standing instruction, fact, OR data definition for THIS channel/DM so you follow it on future turns (e.g. 'keep replies short', 'always link the source'). IMPORTANT: also use this to record a corrected DATA DEFINITION whenever someone tells you how a term or metric should be computed (e.g. 'a qualified lead means Stage is SQL or later', 'active means logged in within 30 days') — recorded definitions are shown to you next time you read a table so your query answers use the agreed meaning, not a guess. Use only when a person tells you to remember something or corrects a definition.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The self-contained instruction or fact to remember for this conversation.",
						},
					},
					"required": []string{"content"},
				},
			},
			ai.ToolSpec{
				Name:        forgetToolName, // "forget"
				Description: "Remove standing instructions you previously remembered for THIS channel/DM. Provide a query to match specific ones, or leave it empty to clear them all.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "Text to match the instruction(s) to remove; empty removes all remembered instructions for this conversation.",
						},
					},
				},
			},
			ai.ToolSpec{
				Name:        createRoutineToolName, // "create_routine"
				Description: "Set up standing recurring work for THIS channel/DM (e.g. 'every weekday at 9am summarize open threads'). Use only when a person asks you to do something on a schedule.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"prompt": map[string]interface{}{
							"type":        "string",
							"description": "What to do on each run, self-contained (as if a person typed it here).",
						},
						"recurrence": map[string]interface{}{
							"type":        "string",
							"description": "Cadence as an RRULE-lite rule: FREQ=DAILY, FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR, or FREQ=HOURLY;INTERVAL=N (every N hours, for monitoring).",
						},
						"time": map[string]interface{}{
							"type":        "string",
							"description": "Fire time as 24h HH:MM. Pair with tz_offset_minutes for a local time; omit for 09:00 UTC.",
						},
						"tz_offset_minutes": map[string]interface{}{
							"type":        "string",
							"description": "The requester's UTC offset in minutes (e.g. -420 for US Pacific in summer), so 'time' is interpreted locally.",
						},
						"name": map[string]interface{}{
							"type":        "string",
							"description": "Optional short name for the routine; derived from the prompt when omitted.",
						},
					},
					"required": []string{"prompt", "recurrence"},
				},
			},
			ai.ToolSpec{
				Name:        listRoutinesToolName, // "list_routines"
				Description: "List the recurring routines set up for THIS channel/DM (name, cadence, id), so a person can review or cancel them.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
			ai.ToolSpec{
				Name:        cancelRoutineToolName, // "cancel_routine"
				Description: "Stop a recurring routine in THIS channel/DM. Prefer its id (from list_routines); a unique name also works.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"routine_id": map[string]interface{}{
							"type":        "string",
							"description": "The routine's id (from list_routines). Preferred — unambiguous.",
						},
						"name": map[string]interface{}{
							"type":        "string",
							"description": "The routine's name, used only when no id is given and the name is unique here.",
						},
					},
				},
			},
		)
	}
	return specs
}
