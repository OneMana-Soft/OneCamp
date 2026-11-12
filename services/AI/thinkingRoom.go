package ai

import "strings"

// Room to think: the one rule every provider that sends an output cap follows (the OpenAI family's
// max_completion_tokens, Ollama's num_predict), and that the window budget follows when it holds the
// answer's share back (ModelLimits.ResponseReserve), so what a call sends and what its prompt was fitted
// around are the same number.

// reasoningRoomTokens is the room a call that thinks gets on top of what its caller asked for. A model that
// thinks spends its thinking from the same output cap it answers from, so an ask sized for the answer alone
// comes back empty, and a host in JSON mode refuses the empty answer with 400 json_validate_failed (V-811:
// every "Plan my week" on the demo's gpt-oss-20b). Spend is metered on what the model uses (budget.go), so
// the room costs only the thinking the model does.
const reasoningRoomTokens = 1024

// thinkingFamily is a family of models that have a thinking trace. always: it thinks whatever OneCamp asks
// (no "think off" switch we send reaches it), so every call to it thinks. Otherwise it thinks only while
// thinking is switched on.
type thinkingFamily struct {
	hint   string // a lowercase substring of the model's name
	always bool
	// effort is the reasoning effort that makes the family think least, sent while the admin's thinking switch
	// is off and on ChatJSONWithRetry's one retry after a host refused an answer the model left empty
	// (LeastThinking): gpt-oss can't stop thinking but takes low|medium|high, so "low" keeps its thinking inside
	// the room (V-811b); qwen3 takes effortNone. "" sends none: the family takes no effort OneCamp knows.
	effort string
	// effortHost, when set, is a lowercase substring of the base URL of the one host that takes effort: qwen3's
	// "none" is Groq's, and another host may refuse the field with a 400. "" is every host.
	effortHost string
}

// effortNone is the reasoning effort that turns a family's thinking off (Groq's qwen3). Ollama's off is its
// think flag false.
const effortNone = "none"

// thinkingFamilies is the one list of model families that think, for every provider, and the one place a
// family's reasoning effort is kept. Matched as a case-insensitive substring, so sized, dated and hosted names
// ("gpt-oss:20b", "openai/gpt-oss-20b", "deepseek-r1-distill-llama-70b", "qwen3:8b-instruct") are covered.
// OLLAMA_THINKING_MODELS (comma-separated substrings) adds families that can think at deploy time, so a newly
// released one needs no code change. OpenAI's own reasoning family is matched by isOpenAIReasoningModel, on
// its own API only, and is sent no effort.
var thinkingFamilies = []thinkingFamily{
	{hint: "gpt-oss", always: true, effort: "low"}, {hint: "deepseek-r1", always: true}, {hint: "qwq", always: true},
	{hint: "qwen3", effort: effortNone, effortHost: "groq"}, {hint: "magistral"}, {hint: "phi4-reasoning"}, {hint: "phi4-mini-reasoning"},
	{hint: "cogito"}, {hint: "smallthinker"}, {hint: "granite3.2"},
}

// thinkingFamilyOf finds model's family: its row on the list, or one an operator added through
// OLLAMA_THINKING_MODELS, which thinks only while switched on and takes no effort. found is false when model
// has no family.
func thinkingFamilyOf(model string) (f thinkingFamily, found bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return thinkingFamily{}, false
	}
	for _, f := range thinkingFamilies {
		if strings.Contains(m, f.hint) {
			return f, true
		}
	}
	for _, h := range strings.Split(getEnvStr("OLLAMA_THINKING_MODELS", ""), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" && strings.Contains(m, h) {
			return thinkingFamily{hint: h}, true
		}
	}
	return thinkingFamily{}, false
}

// modelSupportsThinking reports whether model belongs to a family that can think. Unknown models are
// treated as not thinking: the safe default, which never sends a think flag a model would refuse.
func modelSupportsThinking(model string) bool {
	_, found := thinkingFamilyOf(model)
	return found
}

// modelAlwaysThinks reports whether model belongs to a family that thinks whatever it is asked.
func modelAlwaysThinks(model string) bool {
	f, _ := thinkingFamilyOf(model)
	return f.always
}

// leastEffort is the reasoning effort that makes model think least on the host at baseURL, from its family's
// row: "" when the family takes none, or takes it only on another host, so the field stays off a request the
// host would refuse for it.
func leastEffort(model, baseURL string) string {
	f, _ := thinkingFamilyOf(model)
	if f.effortHost != "" && !strings.Contains(strings.ToLower(baseURL), f.effortHost) {
		return ""
	}
	return f.effort
}

// callThinks is the one rule for whether a call spends thinking out of its output cap: the model belongs to
// OpenAI's own reasoning family or to a family that always thinks, or thinking is switched on for the call
// and the model can think. A model that doesn't think is never one, whatever the switch says. canThink is
// the provider's answer for the model: the family list for an OpenAI-family host; for Ollama, its engine,
// which the think flag it sends already asked. Anthropic calls never think, since OneCamp never sends that
// API's thinking parameter.
func callThinks(kind ProviderType, model string, thinkingOn, canThink bool) bool {
	if kind == ProviderAnthropic {
		return false
	}
	return isOpenAIReasoningModel(kind, model) || modelAlwaysThinks(model) || thinkingOn && canThink
}

// withThinkingRoom is the output a call sends for an ask of want: the ask, plus reasoningRoomTokens when the
// call thinks. No ask stays no ask, so a provider that omits the field for it still does.
func withThinkingRoom(want int, thinks bool) int {
	if want > 0 && thinks {
		return want + reasoningRoomTokens
	}
	return want
}
