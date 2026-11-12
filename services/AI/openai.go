package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// OpenAIProvider implements LLMProvider and EmbeddingProvider using the OpenAI API
// (or any OpenAI /v1-compatible endpoint when a custom base URL is supplied).
// Supports GPT-4o, GPT-4o-mini, and text-embedding-3-small/large models.
type OpenAIProvider struct {
	apiKey         string
	model          string
	embeddingModel string
	embeddingDims  int
	client         *http.Client
	baseURL        string
	kind           ProviderType // ProviderOpenAI or ProviderOpenAICompatible
	streamOpts     ProviderOptions
	// reasoning is the admin "thinking" mode. When false (the default) we ask
	// reasoning-capable models to think least, so the answer (and any
	// tool_call) isn't truncated by the output-token budget and the trace
	// never leaks: the reasoning effort their family takes (reasoningOffParam),
	// and qwen3's "/no_think" soft switch.
	reasoning bool
	// maxOutput is the ceiling an admin recorded for this model (migration 140), or 0 when
	// unstated. A property of the model this client serves, so the clamp lives here and
	// covers every caller rather than each one remembering.
	maxOutput int
	// redactor, when non-nil, scrubs PII from outbound content before it is
	// sent. Set at construction ONLY for cloud endpoints with redaction on;
	// nil for local endpoints and when redaction is off (a no-op fast path).
	redactor *Redactor
}

// clampOutput caps a caller's requested output tokens at what this model will accept.
//
// The clamp matters because the OpenAI API REJECTS an over-large max_completion_tokens
// with a 400 rather than generating less, and callers here ask for values up to 8192 while
// plenty of models cap lower. maxOutput is what an admin recorded for this model on its
// allowlist row (migration 140); 0 means they did not say, and the caller's value stands
// exactly as before.
//
// Returns 0 when the caller asked for nothing, so the field stays omitted from the request
// and the provider applies its own default — unlike Anthropic, this API does not require
// it, and inventing a ceiling where the caller wanted none would cap answers that are
// currently unbounded.
func (o *OpenAIProvider) clampOutput(want int) int {
	if want <= 0 {
		return 0
	}
	if o.maxOutput > 0 && want > o.maxOutput {
		return o.maxOutput
	}
	return want
}

// thinksInBudget reports whether a call to model thinks out of its output budget, by the one rule
// (callThinks): OpenAI's own reasoning family and the families that always think, and a family that can
// think while the admin has thinking on. Qwen3 with thinking off (Groq's reasoning_effort "none", elsewhere
// "/no_think") and a model that doesn't think, whatever the switch says, don't.
func (o *OpenAIProvider) thinksInBudget(model string) bool {
	return callThinks(o.kind, model, o.reasoning, modelSupportsThinking(model))
}

// setSampling puts a chat call's sampling options on its request, the same for
// every chat call (plain, tools, stream): the least thinking where the model
// takes an effort (reasoningOffParam), the output budget (outputBudget), the seed, and
// temperature/top_p except for OpenAI's reasoning family, which rejects them,
// so a model-agnostic selection of o1/o3/o4/gpt-5 doesn't 400 every call.
func (o *OpenAIProvider) setSampling(req *openaiChatRequest, model string, opts ChatOptions) {
	if eff := o.reasoningOffParam(model, opts.LeastThinking); eff != "" {
		req.ReasoningEffort = &eff
	}
	reasoning := o.isReasoningModel(model)
	if opts.Temperature > 0 && !reasoning {
		req.Temperature = &opts.Temperature
	}
	if n := o.outputBudget(model, opts.MaxTokens); n > 0 {
		req.MaxTokens = &n
	}
	if opts.TopP > 0 && !reasoning {
		req.TopP = &opts.TopP
	}
	if opts.Seed != 0 {
		req.Seed = &opts.Seed
	}
}

// outputBudget is the max_completion_tokens every call to model sends: the
// caller's ask, plus the room a call that thinks gets (withThinkingRoom),
// capped by the ceiling an admin recorded (clampOutput). No ask stays no ask,
// so the field is omitted and the endpoint's own default applies.
func (o *OpenAIProvider) outputBudget(model string, want int) int {
	return o.clampOutput(withThinkingRoom(want, o.thinksInBudget(model)))
}

// reasoningOffParam returns the value for the request's reasoning_effort field:
// with the admin's thinking switch off, or when the call asks to think least
// (ChatOptions.LeastThinking), the effort that makes the model's family think
// least on this host, from the family table (leastEffort): gpt-oss "low", since
// it can't stop thinking and at its default spends the whole budget on it
// (V-811b); Groq's qwen3 "none", since it ignores /no_think there. Empty means
// "don't set the field" (leave the endpoint default): thinking on, and every
// family that takes no effort, so no host gets an unsupported-param 400.
func (o *OpenAIProvider) reasoningOffParam(model string, least bool) string {
	if o.reasoning && !least {
		return ""
	}
	return leastEffort(model, o.baseURL)
}

// canThinkLess reports whether a call to model at the least thinking (LeastThinking) is another request than
// one with opts: the one field LeastThinking changes is reasoning_effort (setSampling), so it is another request
// exactly when that effort differs. It doesn't with thinking off (the call already went at the least) or for a
// family that takes no effort on this host (chatStatusError).
func (o *OpenAIProvider) canThinkLess(model string, opts ChatOptions) bool {
	return o.reasoningOffParam(model, true) != o.reasoningOffParam(model, opts.LeastThinking)
}

// applyReasoningDirective appends qwen3's "/no_think" soft switch to the last
// user message when reasoning is OFF and the active model is a qwen variant,
// so qwen3 returns its answer directly instead of a long <think> trace. No-op
// for other models or when reasoning is enabled.
func (o *OpenAIProvider) applyReasoningDirective(messages []openaiChatMessage, model string) []openaiChatMessage {
	if o.reasoning || !strings.Contains(strings.ToLower(model), "qwen") {
		return messages
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			messages[i].Content = strings.TrimRight(messages[i].Content, " \n") + " /no_think"
			break
		}
	}
	return messages
}

// ProviderOptions carries production knobs common to the HTTP-based
// providers: SSRF guarding for admin-supplied custom endpoints and
// opt-in self-signed TLS for self-hosted servers.
type ProviderOptions struct {
	GuardSSRF          bool
	InsecureSkipVerify bool
}

// NewOpenAICompatibleProviderWithOpts is the full constructor. baseURL
// must include the /v1 suffix where applicable. embeddingDims pins the
// embedding dimension (0 = infer from the model name). opts applies the
// SSRF guard and/or self-signed TLS for admin-supplied custom endpoints.
func NewOpenAICompatibleProviderWithOpts(apiKey, model, embeddingModel, baseURL string, embeddingDims int, kind ProviderType, opts ProviderOptions) *OpenAIProvider {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	if kind == "" {
		kind = ProviderOpenAI
	}
	return &OpenAIProvider{
		apiKey:         apiKey,
		model:          model,
		embeddingModel: embeddingModel,
		embeddingDims:  embeddingDims,
		baseURL:        base,
		kind:           kind,
		client: newProviderHTTPClient(httpClientConfig{
			timeout:            60 * time.Second,
			insecureSkipVerify: opts.InsecureSkipVerify,
			guardSSRF:          opts.GuardSSRF,
		}),
		streamOpts: opts,
	}
}

func (o *OpenAIProvider) ProviderName() ProviderType {
	if o.kind != "" {
		return o.kind
	}
	return ProviderOpenAI
}

// redact scrubs PII from outbound messages when this provider targets a cloud
// endpoint (redactor non-nil). A no-op for local endpoints / redaction off.
// Fails closed (returns an error, never sends raw) on oversized content.
func (o *OpenAIProvider) redact(ctx context.Context, msgs []ChatMessage) ([]ChatMessage, error) {
	if o.redactor == nil {
		return msgs, nil
	}
	red, counts, err := o.redactor.ApplyMessages(msgs)
	if err != nil {
		return nil, err
	}
	logRedaction(ctx, string(o.ProviderName()), counts)
	return red, nil
}

// --- OpenAI API types ---

type openaiChatRequest struct {
	Model         string              `json:"model"`
	Messages      []openaiChatMessage `json:"messages"`
	Stream        bool                `json:"stream"`
	StreamOptions *openaiStreamOpts   `json:"stream_options,omitempty"`
	Temperature   *float64            `json:"temperature,omitempty"`
	MaxTokens     *int                `json:"max_completion_tokens,omitempty"`
	TopP          *float64            `json:"top_p,omitempty"`
	Seed          *int                `json:"seed,omitempty"`
	// ReasoningEffort is the least thinking the model's family takes, while
	// thinking is off (reasoningOffParam): gpt-oss "low", Groq-hosted qwen3
	// "none" (the prompt-level /no_think switch is not honored by Groq), so the
	// answer + tool_call fit the budget instead of a long reasoning trace.
	// omitempty keeps it off every other request.
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`

	// ResponseFormat constrains the output (e.g. JSON object) when JSONMode is
	// requested. omitempty keeps it off standard requests.
	ResponseFormat *openaiResponseFormat `json:"response_format,omitempty"`

	// Tools + ToolChoice drive native function calling. omitempty leaves plain
	// chat requests (no tools) exactly as before.
	Tools      []openaiTool `json:"tools,omitempty"`
	ToolChoice string       `json:"tool_choice,omitempty"`
}

// support it (OpenAI, Groq, vLLM). omitempty leaves standard requests untouched.
type openaiResponseFormat struct {
	Type string `json:"type"`
}

// openaiStreamOpts asks the API to emit a final usage frame on streamed
// responses (OpenAI + most compatible servers honor this) so we can
// calibrate token estimation from real prompt-token counts.
type openaiStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type openaiChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Native function-calling fields. ToolCalls appears on an assistant turn
	// that invoked tools; ToolCallID + Name appear on a "tool" role result
	// turn. omitempty keeps ordinary text messages byte-identical to before.
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
}

// openaiToolCall is one structured tool invocation in the OpenAI/Groq schema.
type openaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openaiTool advertises one callable function to the model.
type openaiTool struct {
	Type     string                 `json:"type"` // "function"
	Function openaiToolFunctionSpec `json:"function"`
}

type openaiToolFunctionSpec struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type openaiChatResponse struct {
	Choices []struct {
		Message struct {
			Content   string           `json:"content"`
			ToolCalls []openaiToolCall `json:"tool_calls"`
		} `json:"message"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type openaiEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openaiEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// --- LLMProvider implementation ---

// isReasoningModel reports whether a model id belongs to OpenAI's reasoning
// family (o1/o3/o4/gpt-5…). These models REJECT a custom `temperature` and
// `top_p` (only the default temperature=1 is allowed) and return HTTP 400 if
// either is sent. Since OneCamp is model-agnostic and an admin can select any
// model from the picker, we must omit those params for these families rather
// than break every AI call. Matched on a lowercased prefix so dated variants
// (e.g. "o3-mini-2025-01-31", "gpt-5-chat") are covered. Only applies to the
// first-party OpenAI provider; openai_compatible endpoints (vLLM, Groq, …)
// follow standard chat semantics and keep temperature.
func (o *OpenAIProvider) isReasoningModel(model string) bool {
	return isOpenAIReasoningModel(o.kind, model)
}

// isOpenAIReasoningModel is isReasoningModel for a provider of kind, for a
// caller that holds the kind rather than the client (the room-to-think rule).
func isOpenAIReasoningModel(kind ProviderType, model string) bool {
	if kind != ProviderOpenAI {
		return false
	}
	m := strings.ToLower(strings.TrimSpace(model))
	// Match an exact family id ("o3") or a dated/sized variant ("o3-mini",
	// "gpt-5-chat-2025-..."). We deliberately require the "-" boundary for
	// prefix matches so unrelated ids can't accidentally trip the guard.
	for _, p := range []string{"o1", "o3", "o4", "gpt-5"} {
		if m == p || strings.HasPrefix(m, p+"-") {
			return true
		}
	}
	return false
}

func (o *OpenAIProvider) Chat(ctx context.Context, messages []ChatMessage, opts ChatOptions) (string, error) {
	if err := guardTokenBudget(ctx); err != nil {
		return "", err
	}
	messages, err := o.redact(ctx, messages)
	if err != nil {
		return "", err
	}
	model := o.model
	if opts.Model != "" {
		model = opts.Model
	}

	oaiMessages := make([]openaiChatMessage, len(messages))
	for i, m := range messages {
		oaiMessages[i] = openaiChatMessage{Role: m.Role, Content: m.Content}
	}
	oaiMessages = o.applyReasoningDirective(oaiMessages, model)

	reqBody := openaiChatRequest{
		Model:    model,
		Messages: oaiMessages,
		Stream:   false,
	}
	if opts.JSONMode {
		reqBody.ResponseFormat = &openaiResponseFormat{Type: "json_object"}
	}
	o.setSampling(&reqBody, model, opts)

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("openai: failed to marshal request: %w", err)
	}

	resp, err := doHTTPWithRetry(ctx, o.client, func() (*http.Request, error) {
		r, rerr := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+o.apiKey)
		return r, nil
	}, retryConfigForOpts(opts))
	if err != nil {
		return "", fmt.Errorf("openai: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", chatStatusError("openai: API returned", resp.StatusCode, respBody, func() bool { return o.canThinkLess(model, opts) })
	}

	var chatResp openaiChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", FromProvider(fmt.Errorf("openai: failed to decode response: %w", err))
	}

	if chatResp.Error != nil {
		return "", FromProvider(fmt.Errorf("openai: API error: %s", chatResp.Error.Message))
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("openai: no choices returned")
	}

	// Feed the real prompt-token count back into the token calibrator so
	// EstimateTokens self-corrects for this provider/model over time, and meter
	// spend. Some OpenAI-COMPATIBLE endpoints (vLLM, LM Studio, gateways) omit
	// the usage object; without a fallback those completions would record zero
	// spend and escape the budget, so we estimate from the prompt + response.
	content := chatResp.Choices[0].Message.Content
	if chatResp.Usage != nil && (chatResp.Usage.PromptTokens > 0 || chatResp.Usage.CompletionTokens > 0) {
		RecordTokenUsage(string(o.ProviderName()), model, charsInMessages(messages), chatResp.Usage.PromptTokens)
		reportUsage(ctx, Usage{InputTokens: chatResp.Usage.PromptTokens, OutputTokens: chatResp.Usage.CompletionTokens})
		RecordTokenSpend(ctx, chatResp.Usage.PromptTokens+chatResp.Usage.CompletionTokens)
	} else {
		est := estimateMessagesTokens(messages) + EstimateTokens(content)
		reportUsage(ctx, Usage{InputTokens: estimateMessagesTokens(messages), OutputTokens: EstimateTokens(content)})
		RecordTokenSpend(ctx, est)
	}

	return content, nil
}

// SupportsToolCalling reports that this provider can use native structured tool
// calls. OpenAI, Groq, and vLLM all implement the OpenAI `tools` schema.
func (o *OpenAIProvider) SupportsToolCalling() bool { return true }

// ChatWithTools is the native function-calling path: it advertises the given
// tools and returns the model's text plus any STRUCTURED tool calls it made
// (no `<tool_call>` text parsing). When toolCalls is empty, content is the
// final answer. The caller threads tool results back as role:"tool" messages
// (ChatMessage.ToolCallID) so the model can only produce a final answer after
// real results exist — fabrication ("assuming the tool ran…") is structurally
// impossible.
func (o *OpenAIProvider) ChatWithTools(ctx context.Context, messages []ChatMessage, tools []ToolSpec, opts ChatOptions) (string, []ToolCall, error) {
	if err := guardTokenBudget(ctx); err != nil {
		return "", nil, err
	}
	messages, err := o.redact(ctx, messages)
	if err != nil {
		return "", nil, err
	}
	model := o.model
	if opts.Model != "" {
		model = opts.Model
	}

	oaiMessages := toOpenAIToolMessages(messages)
	oaiMessages = o.applyReasoningDirective(oaiMessages, model)

	reqBody := openaiChatRequest{
		Model:    model,
		Messages: oaiMessages,
		Stream:   false,
	}
	if len(tools) > 0 {
		reqBody.Tools = make([]openaiTool, 0, len(tools))
		for _, t := range tools {
			reqBody.Tools = append(reqBody.Tools, openaiTool{
				Type: "function",
				Function: openaiToolFunctionSpec{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Parameters,
				},
			})
		}
		reqBody.ToolChoice = "auto"
	}
	o.setSampling(&reqBody, model, opts)

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, fmt.Errorf("openai: failed to marshal tools request: %w", err)
	}

	resp, err := doHTTPWithRetry(ctx, o.client, func() (*http.Request, error) {
		r, rerr := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+o.apiKey)
		return r, nil
	}, retryConfigForOpts(opts))
	if err != nil {
		return "", nil, fmt.Errorf("openai: tools request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		// Resilience for the Llama-on-Groq tool-calling quirk: some
		// Llama-family models emit a tool call in the "<function=name{json}>"
		// python-tag TEXT format instead of the structured tool_calls array.
		// The OpenAI-compatible endpoint (Groq) rejects it with HTTP 400
		// code=tool_use_failed but echoes the model's raw output in
		// error.failed_generation. Rather than fail the whole agent run, we
		// salvage the intended call from that echo so the loop can proceed.
		// No-op on providers that never emit this (e.g. real OpenAI).
		if resp.StatusCode == http.StatusBadRequest && len(tools) > 0 {
			allowed := make(map[string]bool, len(tools))
			for _, t := range tools {
				allowed[t.Name] = true
			}
			if tc, ok := recoverToolCallFromFailedGeneration(respBody, allowed); ok {
				helpers.LogInfoWithContext(ctx, "openai: recovered a text-format tool call (%s) from a tool_use_failed 400", tc.Name)
				return "", []ToolCall{tc}, nil
			}
		}
		return "", nil, providerStatusError("openai: API returned", resp.StatusCode, respBody)
	}

	var chatResp openaiChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", nil, FromProvider(fmt.Errorf("openai: failed to decode tools response: %w", err))
	}
	if chatResp.Error != nil {
		return "", nil, FromProvider(fmt.Errorf("openai: API error: %s", chatResp.Error.Message))
	}
	if len(chatResp.Choices) == 0 {
		return "", nil, fmt.Errorf("openai: no choices returned")
	}

	content := chatResp.Choices[0].Message.Content
	var calls []ToolCall
	for _, tc := range chatResp.Choices[0].Message.ToolCalls {
		calls = append(calls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}

	if chatResp.Usage != nil && (chatResp.Usage.PromptTokens > 0 || chatResp.Usage.CompletionTokens > 0) {
		RecordTokenUsage(string(o.ProviderName()), model, charsInMessages(messages), chatResp.Usage.PromptTokens)
		reportUsage(ctx, Usage{InputTokens: chatResp.Usage.PromptTokens, OutputTokens: chatResp.Usage.CompletionTokens})
		RecordTokenSpend(ctx, chatResp.Usage.PromptTokens+chatResp.Usage.CompletionTokens)
	} else {
		est := estimateMessagesTokens(messages) + EstimateTokens(content)
		reportUsage(ctx, Usage{InputTokens: estimateMessagesTokens(messages), OutputTokens: EstimateTokens(content)})
		RecordTokenSpend(ctx, est)
	}

	return content, calls, nil
}

// toOpenAIToolMessages maps our ChatMessages to the wire format, carrying the
// native function-calling fields (assistant tool_calls and tool-result
// messages) so a multi-turn tool conversation serializes correctly.
func toOpenAIToolMessages(messages []ChatMessage) []openaiChatMessage {
	out := make([]openaiChatMessage, len(messages))
	for i, m := range messages {
		om := openaiChatMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
		for _, tc := range m.ToolCalls {
			oc := openaiToolCall{ID: tc.ID, Type: "function"}
			oc.Function.Name = tc.Name
			oc.Function.Arguments = tc.Arguments
			om.ToolCalls = append(om.ToolCalls, oc)
		}
		out[i] = om
	}
	return out
}

func (o *OpenAIProvider) ChatStream(ctx context.Context, messages []ChatMessage, opts ChatOptions) (<-chan StreamChunk, error) {
	if err := guardTokenBudget(ctx); err != nil {
		return nil, err
	}
	messages, err := o.redact(ctx, messages)
	if err != nil {
		return nil, err
	}
	model := o.model
	if opts.Model != "" {
		model = opts.Model
	}

	oaiMessages := make([]openaiChatMessage, len(messages))
	for i, m := range messages {
		oaiMessages[i] = openaiChatMessage{Role: m.Role, Content: m.Content}
	}
	oaiMessages = o.applyReasoningDirective(oaiMessages, model)

	reqBody := openaiChatRequest{
		Model:         model,
		Messages:      oaiMessages,
		Stream:        true,
		StreamOptions: &openaiStreamOpts{IncludeUsage: true},
	}
	o.setSampling(&reqBody, model, opts)

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("openai: failed to marshal request: %w", err)
	}

	streamClient := newProviderHTTPClient(httpClientConfig{
		timeout:            0, // streaming: rely on context cancellation
		insecureSkipVerify: o.streamOpts.InsecureSkipVerify,
		guardSSRF:          o.streamOpts.GuardSSRF,
	})
	resp, err := doHTTPWithRetry(ctx, streamClient, func() (*http.Request, error) {
		r, rerr := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+o.apiKey)
		return r, nil
	}, interactiveRetryConfig())
	if err != nil {
		return nil, fmt.Errorf("openai: stream request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, providerStatusError("openai: API returned", resp.StatusCode, respBody)
	}

	ch := make(chan StreamChunk, 64)

	helpers.GoSafeNamed("services/AI/openai:ChatStream", func() {
		defer close(ch)
		defer resp.Body.Close()

		var out strings.Builder
		var exactPrompt, exactCompletion int
		metered := false
		meter := func() {
			if metered {
				return
			}
			metered = true
			if exactPrompt+exactCompletion > 0 {
				RecordTokenSpend(ctx, exactPrompt+exactCompletion)
				return
			}
			RecordTokenSpend(ctx, estimateMessagesTokens(messages)+EstimateTokens(out.String()))
		}
		defer meter()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				ch <- StreamChunk{Done: true, Error: ctx.Err()}
				return
			default:
			}

			line := scanner.Text()

			// OpenAI SSE format: "data: {json}" or "data: [DONE]"
			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				meter()
				ch <- StreamChunk{Done: true}
				return
			}

			var chunk openaiChatResponse
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				helpers.LogErrorWithContext(ctx, "openai: failed to parse stream chunk: %v", err)
				continue
			}

			// The final usage frame (from stream_options.include_usage)
			// has no choices but carries prompt_tokens — feed it to the
			// token calibrator and use it for exact spend metering.
			if chunk.Usage != nil && chunk.Usage.PromptTokens > 0 {
				RecordTokenUsage(string(o.ProviderName()), model, charsInMessages(messages), chunk.Usage.PromptTokens)
				exactPrompt = chunk.Usage.PromptTokens
				exactCompletion = chunk.Usage.CompletionTokens
			}

			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				out.WriteString(chunk.Choices[0].Delta.Content)
				ch <- StreamChunk{Content: chunk.Choices[0].Delta.Content}
			}
		}

		if err := scanner.Err(); err != nil {
			ch <- StreamChunk{Done: true, Error: FromProvider(fmt.Errorf("openai: stream read error: %w", err))}
		}
	})

	return ch, nil
}

// --- EmbeddingProvider implementation ---

func (o *OpenAIProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if o.redactor != nil {
		red, counts, err := o.redactor.ApplyTexts(texts)
		if err != nil {
			return nil, err
		}
		logRedaction(ctx, string(o.ProviderName()), counts)
		texts = red
	}
	reqBody := openaiEmbedRequest{
		Model: o.embeddingModel,
		Input: texts,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("openai: failed to marshal embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: failed to create embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: embed request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, providerStatusError("openai: embed API returned", resp.StatusCode, respBody)
	}

	var embedResp openaiEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("openai: failed to decode embed response: %w", err)
	}

	if embedResp.Error != nil {
		return nil, FromProvider(fmt.Errorf("openai: embed API error: %s", embedResp.Error.Message))
	}

	embeddings := make([][]float32, len(embedResp.Data))
	for i, d := range embedResp.Data {
		embeddings[i] = d.Embedding
	}

	return embeddings, nil
}

func (o *OpenAIProvider) Dimensions() int {
	if o.embeddingDims > 0 {
		return o.embeddingDims
	}
	// text-embedding-3-small: 1536, text-embedding-3-large: 3072
	if strings.Contains(o.embeddingModel, "large") {
		return 3072
	}
	return 1536
}

// --- ModelLister implementation ---

type openaiModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ListModels hits GET {baseURL}/models. Works for OpenAI and any
// compatible endpoint (vLLM, LM Studio, OpenRouter, ...). Endpoints that
// don't implement /models return an error the caller surfaces to the
// admin (who can then type the model id manually).
func (o *OpenAIProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("openai: build models request: %w", err)
	}
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: models request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, FromProvider(fmt.Errorf("openai: models returned %d: %s", resp.StatusCode, string(b)))
	}

	var mr openaiModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, fmt.Errorf("openai: decode models: %w", err)
	}
	if mr.Error != nil {
		return nil, FromProvider(fmt.Errorf("openai: models error: %s", mr.Error.Message))
	}

	out := make([]ModelInfo, 0, len(mr.Data))
	for _, m := range mr.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, ModelInfo{
			ID:        m.ID,
			Installed: true, // cloud/remote: always available, nothing to install
			Embedding: strings.Contains(strings.ToLower(m.ID), "embed"),
		})
	}
	return out, nil
}

// --- Llama/Groq malformed tool-call recovery ------------------------------
//
// Some Llama-family models (notably llama-3.x served via Groq's OpenAI-
// compatible API) intermittently emit a tool call in the "python tag" TEXT
// format — e.g. `<function=name{...json...}>` — instead of the structured
// tool_calls array. The endpoint then returns HTTP 400 with
// code="tool_use_failed" and echoes the model's raw output in
// error.failed_generation. The helpers below salvage the intended call from
// that echo so a single malformed generation doesn't fail the whole agent run.
// They are pure and provider-agnostic (a no-op on providers that never emit
// this error), and unit-tested against the real shapes seen in production.

// groqFailure mirrors the relevant subset of the 400 error envelope: its code
// and what the model generated.
type groqFailure struct {
	Error struct {
		Code             string `json:"code"`
		FailedGeneration string `json:"failed_generation"`
	} `json:"error"`
}

// recoverToolCallFromFailedGeneration parses a tool_use_failed 400 body and
// returns the intended tool call when it can be salvaged. It only returns a
// call whose name is one the caller actually offered (allowed), so we never
// invent or execute a tool the run didn't advertise.
func recoverToolCallFromFailedGeneration(body []byte, allowed map[string]bool) (ToolCall, bool) {
	var f groqFailure
	if err := json.Unmarshal(body, &f); err != nil {
		return ToolCall{}, false
	}
	if f.Error.Code != "tool_use_failed" || strings.TrimSpace(f.Error.FailedGeneration) == "" {
		return ToolCall{}, false
	}
	name, args, ok := parseTagFunctionCall(f.Error.FailedGeneration)
	if !ok || !allowed[name] {
		return ToolCall{}, false
	}
	return ToolCall{ID: "recovered_" + name, Name: name, Arguments: args}, true
}

// emptyJSONAnswer reports whether a 400's body is a host in JSON mode refusing an
// answer the model left empty: Groq's json_validate_failed with nothing in
// failed_generation (V-811b). One with a generation is a model that answered
// badly, which the same request at less thinking would not fix.
func emptyJSONAnswer(body []byte) bool {
	var f groqFailure
	return json.Unmarshal(body, &f) == nil && f.Error.Code == "json_validate_failed" && strings.TrimSpace(f.Error.FailedGeneration) == ""
}

// parseTagFunctionCall extracts (name, argsJSON) from a Llama "python tag" tool
// call. It tolerates the several shapes seen in the wild:
//
//	<function=NAME{...json...}>            <function=NAME{...json...}</function>
//	<function=NAME>{...json...}</function> <function=NAME({...json...})>
//
// The name is read after "<function=" up to the first structural char
// ('{', '(', '>' or whitespace); the arguments are the first balanced {...}
// JSON object ("{}" when the call takes none). Pure; ok=false when no function
// tag is present or the name is empty.
func parseTagFunctionCall(s string) (string, string, bool) {
	const marker = "<function="
	i := strings.Index(s, marker)
	if i < 0 {
		return "", "", false
	}
	rest := s[i+len(marker):]
	end := strings.IndexAny(rest, "{(> \t\n\r")
	if end <= 0 {
		return "", "", false
	}
	name := strings.TrimSpace(rest[:end])
	if name == "" {
		return "", "", false
	}
	args := extractFirstJSONObject(rest)
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	return name, args, true
}

// extractFirstJSONObject returns the first balanced {...} substring of s
// (string-literal and escape aware so a "}" inside a string doesn't close the
// object early), or "" when there is none.
func extractFirstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
