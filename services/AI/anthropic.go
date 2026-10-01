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

// AnthropicProvider implements LLMProvider using the Anthropic Messages API.
// Note: Anthropic does not offer an embedding API, so this provider does NOT
// implement EmbeddingProvider. Use Ollama or OpenAI for embeddings.
type AnthropicProvider struct {
	apiKey     string
	model      string
	client     *http.Client
	baseURL    string
	streamOpts ProviderOptions
	// maxOutput is the ceiling an admin recorded for this model (migration 140), or 0 when
	// they did not state one. Set at construction because it is a property of the model
	// this client serves, which is why the clamp belongs here rather than at each caller.
	maxOutput int
	// redactor scrubs PII from outbound content before egress. Set at
	// construction only for cloud endpoints with redaction on; nil otherwise.
	redactor *Redactor
}

// NewAnthropicProviderWithOpts is the full constructor with SSRF/TLS opts
// for admin-supplied custom base URLs.
func NewAnthropicProviderWithOpts(apiKey, model, baseURL string, opts ProviderOptions) *AnthropicProvider {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		base = "https://api.anthropic.com/v1"
	}
	return &AnthropicProvider{
		apiKey:  apiKey,
		model:   model,
		baseURL: base,
		client: newProviderHTTPClient(httpClientConfig{
			timeout:            60 * time.Second,
			insecureSkipVerify: opts.InsecureSkipVerify,
			guardSSRF:          opts.GuardSSRF,
		}),
		streamOpts: opts,
	}
}

func (a *AnthropicProvider) ProviderName() ProviderType {
	return ProviderAnthropic
}

// redact scrubs PII from outbound messages when this provider targets a cloud
// endpoint (redactor non-nil). Fails closed on oversized content.
func (a *AnthropicProvider) redact(ctx context.Context, msgs []ChatMessage) ([]ChatMessage, error) {
	if a.redactor == nil {
		return msgs, nil
	}
	red, counts, err := a.redactor.ApplyMessages(msgs)
	if err != nil {
		return nil, err
	}
	logRedaction(ctx, string(a.ProviderName()), counts)
	return red, nil
}

// defaultAnthropicMaxTokens is the response-length cap applied when a caller
// doesn't set ChatOptions.MaxTokens. The Anthropic Messages API REQUIRES
// max_tokens, so unlike OpenAI we can't omit it. 1024 comfortably covers
// OneCamp's summaries/answers; callers that need longer output (doc AI) set
// MaxTokens explicitly. Kept conservative so a runaway generation can't burn
// an unbounded number of output tokens on a paid endpoint.
const defaultAnthropicMaxTokens = 1024

// clampOutput resolves the max_tokens to send: the caller's request, the documented
// default when it asked for nothing, and never more than this model will accept.
//
// (See openAICompatibleProvider.clampOutput for the same rule on that family; the two
// differ only in that Anthropic's API requires the field and so has a default.)
//
// The clamp matters because Anthropic REJECTS an over-large max_tokens with a 400 rather
// than quietly generating less — a model capped at 4096 asked for 8192 fails outright, and
// several callers here ask for 4096 or 8192. maxOutput is what an admin recorded for this
// model on its allowlist row (migration 140); 0 means they did not say, and then the
// caller's value stands exactly as before.
//
// Asking for more than a model produces never got more output. It only turned a working
// request into a failed one.
func (a *AnthropicProvider) clampOutput(want int) int {
	if want <= 0 {
		want = defaultAnthropicMaxTokens
	}
	if a.maxOutput > 0 && want > a.maxOutput {
		return a.maxOutput
	}
	return want
}

// --- Anthropic API types ---

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Stream      bool               `json:"stream"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type anthropicStreamEvent struct {
	Type  string `json:"type"`
	Delta *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta,omitempty"`
	// message_start carries the input-token count in message.usage; its
	// output_tokens is the (small) initial count.
	Message *struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message,omitempty"`
	// message_delta carries the running/final output-token count at the top
	// level, so streaming spend can be metered exactly (no estimate needed).
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// --- LLMProvider implementation ---

func (a *AnthropicProvider) Chat(ctx context.Context, messages []ChatMessage, opts ChatOptions) (string, error) {
	if err := guardTokenBudget(ctx); err != nil {
		return "", err
	}
	messages, err := a.redact(ctx, messages)
	if err != nil {
		return "", err
	}
	model := a.model
	if opts.Model != "" {
		model = opts.Model
	}

	maxTokens := a.clampOutput(opts.MaxTokens)

	// Extract system message (Anthropic uses a separate "system" field)
	var systemMsg string
	var conversationMsgs []anthropicMessage

	for _, m := range messages {
		if m.Role == "system" {
			systemMsg = m.Content
		} else {
			conversationMsgs = append(conversationMsgs, anthropicMessage{
				Role:    m.Role,
				Content: m.Content,
			})
		}
	}

	reqBody := anthropicRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    systemMsg,
		Messages:  conversationMsgs,
		Stream:    false,
	}

	if opts.Temperature > 0 {
		reqBody.Temperature = &opts.Temperature
	}
	if opts.TopP > 0 {
		reqBody.TopP = &opts.TopP
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("anthropic: failed to marshal request: %w", err)
	}

	resp, err := doHTTPWithRetry(ctx, a.client, func() (*http.Request, error) {
		r, rerr := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/messages", bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("x-api-key", a.apiKey)
		r.Header.Set("anthropic-version", "2023-06-01")
		return r, nil
	}, retryConfigForOpts(opts))
	if err != nil {
		return "", fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", providerStatusError("anthropic: API returned", resp.StatusCode, respBody)
	}

	var chatResp anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", fmt.Errorf("anthropic: failed to decode response: %w", err)
	}

	if chatResp.Error != nil {
		return "", fmt.Errorf("anthropic: API error: %s", chatResp.Error.Message)
	}

	if len(chatResp.Content) == 0 {
		return "", fmt.Errorf("anthropic: no content returned")
	}

	// Feed the real input-token count into the token calibrator so
	// EstimateTokens self-corrects for this provider/model over time.
	if chatResp.Usage != nil {
		RecordTokenUsage(string(a.ProviderName()), model, charsInMessages(messages), chatResp.Usage.InputTokens)
		reportUsage(ctx, Usage{InputTokens: chatResp.Usage.InputTokens, OutputTokens: chatResp.Usage.OutputTokens})
		RecordTokenSpend(ctx, chatResp.Usage.InputTokens+chatResp.Usage.OutputTokens)
	}

	return chatResp.Content[0].Text, nil
}

func (a *AnthropicProvider) ChatStream(ctx context.Context, messages []ChatMessage, opts ChatOptions) (<-chan StreamChunk, error) {
	if err := guardTokenBudget(ctx); err != nil {
		return nil, err
	}
	messages, err := a.redact(ctx, messages)
	if err != nil {
		return nil, err
	}
	model := a.model
	if opts.Model != "" {
		model = opts.Model
	}

	maxTokens := a.clampOutput(opts.MaxTokens)

	var systemMsg string
	var conversationMsgs []anthropicMessage

	for _, m := range messages {
		if m.Role == "system" {
			systemMsg = m.Content
		} else {
			conversationMsgs = append(conversationMsgs, anthropicMessage{
				Role:    m.Role,
				Content: m.Content,
			})
		}
	}

	reqBody := anthropicRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    systemMsg,
		Messages:  conversationMsgs,
		Stream:    true,
	}

	if opts.Temperature > 0 {
		reqBody.Temperature = &opts.Temperature
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("anthropic: failed to marshal request: %w", err)
	}

	streamClient := newProviderHTTPClient(httpClientConfig{
		timeout:            0, // streaming: rely on context cancellation
		insecureSkipVerify: a.streamOpts.InsecureSkipVerify,
		guardSSRF:          a.streamOpts.GuardSSRF,
	})
	resp, err := doHTTPWithRetry(ctx, streamClient, func() (*http.Request, error) {
		r, rerr := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/messages", bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("x-api-key", a.apiKey)
		r.Header.Set("anthropic-version", "2023-06-01")
		return r, nil
	}, interactiveRetryConfig())
	if err != nil {
		return nil, fmt.Errorf("anthropic: stream request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, providerStatusError("anthropic: API returned", resp.StatusCode, respBody)
	}

	ch := make(chan StreamChunk, 64)

	go func() {
		defer close(ch)
		defer resp.Body.Close()

		var out strings.Builder
		var exactInput, exactOutput int
		metered := false
		meter := func() {
			if metered {
				return
			}
			metered = true
			// Prefer Anthropic's exact reported counts (input from message_start,
			// output from message_delta); fall back to the calibrated estimator
			// only if the stream ended without usage.
			if exactInput+exactOutput > 0 {
				RecordTokenSpend(ctx, exactInput+exactOutput)
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

			// Anthropic SSE format: "event: <type>\ndata: {json}"
			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")

			var event anthropicStreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				helpers.LogErrorWithContext(ctx, "anthropic: failed to parse stream event: %v", err)
				continue
			}

			switch event.Type {
			case "message_start":
				// Carries the prompt's input-token count — calibrate + meter.
				if event.Message != nil && event.Message.Usage != nil && event.Message.Usage.InputTokens > 0 {
					RecordTokenUsage(string(a.ProviderName()), model, charsInMessages(messages), event.Message.Usage.InputTokens)
					exactInput = event.Message.Usage.InputTokens
					exactOutput = event.Message.Usage.OutputTokens
				}
			case "message_delta":
				// Running/final output-token count — keep the latest.
				if event.Usage != nil && event.Usage.OutputTokens > 0 {
					exactOutput = event.Usage.OutputTokens
				}
			case "content_block_delta":
				if event.Delta != nil && event.Delta.Text != "" {
					out.WriteString(event.Delta.Text)
					ch <- StreamChunk{Content: event.Delta.Text}
				}
			case "message_stop":
				meter()
				ch <- StreamChunk{Done: true}
				return
			case "error":
				errMsg := "unknown error"
				if event.Error != nil {
					errMsg = event.Error.Message
				}
				ch <- StreamChunk{Done: true, Error: fmt.Errorf("anthropic: stream error: %s", errMsg)}
				return
			}
		}

		if err := scanner.Err(); err != nil {
			ch <- StreamChunk{Done: true, Error: fmt.Errorf("anthropic: stream read error: %w", err)}
		}
	}()

	return ch, nil
}

// --- ModelLister implementation ---

type anthropicModelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ListModels hits GET {baseURL}/models. Anthropic returns models with
// the most recently released first. Anthropic has no embedding API, so
// every entry is a chat model.
func (a *AnthropicProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("anthropic: build models request: %w", err)
	}
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: models request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic: models returned %d: %s", resp.StatusCode, string(b))
	}

	var mr anthropicModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, fmt.Errorf("anthropic: decode models: %w", err)
	}
	if mr.Error != nil {
		return nil, fmt.Errorf("anthropic: models error: %s", mr.Error.Message)
	}

	out := make([]ModelInfo, 0, len(mr.Data))
	for _, m := range mr.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, ModelInfo{ID: m.ID, Installed: true})
	}
	return out, nil
}
