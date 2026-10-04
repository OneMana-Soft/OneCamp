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
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// OllamaProvider implements LLMProvider and EmbeddingProvider using a local Ollama server.
// Ollama runs models like Llama 3, Mistral, and nomic-embed-text locally with zero API costs.
type OllamaProvider struct {
	host           string
	model          string
	embeddingModel string
	embeddingDims  int
	keepAlive      string
	numThread      int
	numCtx         int
	reasoning      bool // admin "thinking" mode for reasoning-capable models
	// redactor scrubs PII from outbound content for cloud endpoints (nil =
	// no-op: local endpoint or redaction off). An Ollama host is normally
	// local, so this is usually nil.
	redactor *Redactor
	client   *http.Client
}

// NewOllamaProviderWithDims is like NewOllamaProvider but lets the caller
// pin the embedding dimension (which must match the OpenSearch k-NN
// index). Defaults to 768 (nomic-embed-text) when dims <= 0. Runtime
// tuning params (keep-alive, threads, context window) are read from the
// process env once at construction so the provider has no dependency on
// a mutable global.
func NewOllamaProviderWithDims(host, model, embeddingModel string, dims int) *OllamaProvider {
	if dims <= 0 {
		dims = 768
	}
	return &OllamaProvider{
		host:           strings.TrimRight(host, "/"),
		model:          model,
		embeddingModel: embeddingModel,
		embeddingDims:  dims,
		keepAlive:      getEnvStr("OLLAMA_KEEP_ALIVE", "5m"),
		numThread:      getEnvInt("OLLAMA_NUM_THREAD", 0),
		numCtx:         getEnvInt("OLLAMA_NUM_CTX", 8192),
		// Route through the shared chokepoint so local-only mode's dial guard
		// applies to Ollama too (an Ollama endpoint pointed at a public host is
		// correctly refused under local-only).
		client: newProviderHTTPClient(httpClientConfig{timeout: 120 * time.Second}),
	}
}

func (o *OllamaProvider) ProviderName() ProviderType {
	return ProviderOllama
}

// redact scrubs PII from outbound messages when this provider targets a cloud
// endpoint (redactor non-nil). Normally a no-op since Ollama is local. Fails
// closed on oversized content.
func (o *OllamaProvider) redact(ctx context.Context, msgs []ChatMessage) ([]ChatMessage, error) {
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

// resolveThink decides the Ollama "think" flag for a request. It is sent ONLY
// for models that actually support a reasoning trace: newer Ollama returns
// HTTP 400 ("<model> does not support thinking") when `think` is sent to a
// non-thinking model (e.g. llama3.2/llama3.3), which previously made the
// rate-limit fallback to a local model fail every time. For a thinking-capable
// model an explicit per-request *true/*false from the caller wins; otherwise the
// admin-configured reasoning setting applies. For every other model we omit the
// field entirely (nil) so the request is always accepted.
func (o *OllamaProvider) resolveThink(want *bool) *bool {
	if !o.canThink() {
		return nil
	}
	if want != nil {
		return want
	}
	v := o.reasoning
	return &v
}

// ollamaThinkingModelHints are substrings of Ollama model names whose families
// support a "thinking"/reasoning trace. Only these receive the `think` flag
// (see resolveThink). Extendable at deploy time via OLLAMA_THINKING_MODELS
// (comma-separated substrings) so a newly-released thinking model can be enabled
// without a code change.
var ollamaThinkingModelHints = []string{
	"deepseek-r1", "qwq", "qwen3", "magistral", "phi4-reasoning",
	"phi4-mini-reasoning", "gpt-oss", "cogito", "smallthinker", "granite3.2",
}

// modelSupportsThinking reports whether an Ollama model name belongs to a known
// reasoning-capable family (built-in hints plus any from OLLAMA_THINKING_MODELS).
// Case-insensitive substring match so dated/sized variants (e.g.
// "qwen3:8b-instruct") are covered. Unknown models are treated as NON-thinking
// (the safe default: thinking off, and never a 400).
func modelSupportsThinking(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	for _, h := range ollamaThinkingModelHints {
		if strings.Contains(m, h) {
			return true
		}
	}
	for _, h := range strings.Split(getEnvStr("OLLAMA_THINKING_MODELS", ""), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" && strings.Contains(m, h) {
			return true
		}
	}
	return false
}

// --- Ollama API request/response types ---

type ollamaChatRequest struct {
	Model     string              `json:"model"`
	Messages  []ollamaChatMessage `json:"messages"`
	Stream    bool                `json:"stream"`
	Options   *ollamaOptions      `json:"options,omitempty"`
	KeepAlive string              `json:"keep_alive,omitempty"`
	// Think disables/enables the reasoning trace on thinking models. Pointer
	// so it's only sent when set; nil omits the field (model default).
	Think *bool `json:"think,omitempty"`
}

type ollamaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Images carries base64-encoded image data for vision models (llava,
	// llama3.2-vision). Omitted for text-only messages.
	Images []string `json:"images,omitempty"`
}

type ollamaOptions struct {
	Temperature float64 `json:"temperature,omitempty"`
	NumPredict  int     `json:"num_predict,omitempty"`
	TopP        float64 `json:"top_p,omitempty"`
	Seed        int     `json:"seed,omitempty"`
	NumThread   int     `json:"num_thread,omitempty"`
	NumCtx      int     `json:"num_ctx,omitempty"`
}

type ollamaChatResponse struct {
	Model           string            `json:"model"`
	Message         ollamaChatMessage `json:"message"`
	Done            bool              `json:"done"`
	PromptEvalCount int               `json:"prompt_eval_count"`
	EvalCount       int               `json:"eval_count"`
}

type ollamaEmbedRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"` // string or []string
}

type ollamaEmbedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float32 `json:"embeddings"`
}

// --- LLMProvider implementation ---

// Chat sends a synchronous (non-streaming) chat request to Ollama.
func (o *OllamaProvider) Chat(ctx context.Context, messages []ChatMessage, opts ChatOptions) (string, error) {
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

	ollamaMessages := make([]ollamaChatMessage, len(messages))
	for i, m := range messages {
		ollamaMessages[i] = ollamaChatMessage{Role: m.Role, Content: m.Content}
	}

	reqBody := ollamaChatRequest{
		Model:     model,
		Messages:  ollamaMessages,
		Stream:    false,
		KeepAlive: o.keepAlive,
		Think:     o.resolveThink(opts.Think),
	}

	reqBody.Options = &ollamaOptions{
		Temperature: opts.Temperature,
		NumPredict:  opts.MaxTokens,
		TopP:        opts.TopP,
		Seed:        opts.Seed,
		NumThread:   o.numThread,
		NumCtx:      o.numCtx,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("ollama: failed to marshal request: %w", err)
	}

	resp, err := doHTTPWithRetry(ctx, o.client, func() (*http.Request, error) {
		r, rerr := http.NewRequestWithContext(ctx, http.MethodPost, o.host+"/api/chat", bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		r.Header.Set("Content-Type", "application/json")
		return r, nil
	}, retryConfigForOpts(opts))
	if err != nil {
		return "", fmt.Errorf("ollama: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", providerStatusError("ollama: API returned", resp.StatusCode, respBody)
	}

	var chatResp ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", fmt.Errorf("ollama: failed to decode response: %w", err)
	}

	// Feed Ollama's real prompt token count (prompt_eval_count) into the
	// calibrator so EstimateTokens self-corrects per local model.
	if chatResp.PromptEvalCount > 0 {
		RecordTokenUsage(string(o.ProviderName()), model, charsInMessages(messages), chatResp.PromptEvalCount)
	}
	if chatResp.PromptEvalCount > 0 || chatResp.EvalCount > 0 {
		reportUsage(ctx, Usage{InputTokens: chatResp.PromptEvalCount, OutputTokens: chatResp.EvalCount})
		RecordTokenSpend(ctx, chatResp.PromptEvalCount+chatResp.EvalCount)
	}

	return chatResp.Message.Content, nil
}

// ChatStream sends a streaming chat request to Ollama and returns a channel
// that emits partial response chunks as they arrive.
func (o *OllamaProvider) ChatStream(ctx context.Context, messages []ChatMessage, opts ChatOptions) (<-chan StreamChunk, error) {
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

	ollamaMessages := make([]ollamaChatMessage, len(messages))
	for i, m := range messages {
		ollamaMessages[i] = ollamaChatMessage{Role: m.Role, Content: m.Content}
	}

	reqBody := ollamaChatRequest{
		Model:     model,
		Messages:  ollamaMessages,
		Stream:    true,
		KeepAlive: o.keepAlive,
		Think:     o.resolveThink(opts.Think),
	}

	reqBody.Options = &ollamaOptions{
		Temperature: opts.Temperature,
		NumPredict:  opts.MaxTokens,
		TopP:        opts.TopP,
		Seed:        opts.Seed,
		NumThread:   o.numThread,
		NumCtx:      o.numCtx,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("ollama: failed to marshal request: %w", err)
	}

	// Streaming requests need a longer timeout — managed by context cancellation instead
	streamClient := newProviderHTTPClient(httpClientConfig{timeout: 0})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: stream request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, providerStatusError("ollama: API returned", resp.StatusCode, respBody)
	}

	ch := make(chan StreamChunk, 64)

	go func() {
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
		// Increase buffer size for potentially large JSON lines
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				ch <- StreamChunk{Done: true, Error: ctx.Err()}
				return
			default:
			}

			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			var chunk ollamaChatResponse
			if err := json.Unmarshal(line, &chunk); err != nil {
				helpers.LogErrorWithContext(ctx, "ollama: failed to parse stream chunk: %v", err)
				continue
			}

			out.WriteString(chunk.Message.Content)
			ch <- StreamChunk{
				Content: chunk.Message.Content,
				Done:    chunk.Done,
			}

			if chunk.Done {
				// The final frame carries prompt_eval_count / eval_count —
				// calibrate and use for exact spend metering.
				if chunk.PromptEvalCount > 0 {
					RecordTokenUsage(string(o.ProviderName()), model, charsInMessages(messages), chunk.PromptEvalCount)
				}
				exactPrompt = chunk.PromptEvalCount
				exactCompletion = chunk.EvalCount
				meter()
				return
			}
		}

		if err := scanner.Err(); err != nil {
			ch <- StreamChunk{Done: true, Error: fmt.Errorf("ollama: stream read error: %w", err)}
		}
	}()

	return ch, nil
}

// --- EmbeddingProvider implementation ---

// Embed generates vector embeddings using Ollama's embedding API.
// Default model: nomic-embed-text (768 dimensions).
func (o *OllamaProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if o.redactor != nil {
		red, counts, err := o.redactor.ApplyTexts(texts)
		if err != nil {
			return nil, err
		}
		logRedaction(ctx, string(o.ProviderName()), counts)
		texts = red
	}
	reqBody := ollamaEmbedRequest{
		Model: o.embeddingModel,
		Input: texts,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("ollama: failed to marshal embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.host+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: failed to create embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: embed request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, providerStatusError("ollama: embed API returned", resp.StatusCode, respBody)
	}

	var embedResp ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("ollama: failed to decode embed response: %w", err)
	}

	return embedResp.Embeddings, nil
}

// Dimensions returns the vector dimension for the active embedding model.
func (o *OllamaProvider) Dimensions() int {
	if o.embeddingDims > 0 {
		return o.embeddingDims
	}
	// nomic-embed-text produces 768-dimensional vectors
	return 768
}

// thinkingCaps caches what each Ollama host says a model can do, keyed by
// host+model, so the engine is asked once per model per process.
var thinkingCaps sync.Map // map[string]bool

// canThink reports whether the configured model has a reasoning trace, asking
// the engine (POST /api/show, "capabilities") rather than guessing from the
// name. Names mislead both ways: qwen3:4b-instruct matches the "qwen3" hint
// but reports only [completion tools], and newer Ollama rejects `think` for it
// with a 400. Falls back to the name hints when the engine cannot be asked
// (old Ollama without capabilities, or unreachable), and that fallback is not
// cached so a later call can still learn the truth.
func (o *OllamaProvider) canThink() bool {
	key := o.host + "|" + o.model
	if v, ok := thinkingCaps.Load(key); ok {
		return v.(bool)
	}
	caps, ok := o.modelCapabilities()
	if !ok {
		return modelSupportsThinking(o.model)
	}
	has := false
	for _, c := range caps {
		if c == "thinking" {
			has = true
			break
		}
	}
	thinkingCaps.Store(key, has)
	return has
}

// modelCapabilities asks the engine what the model can do. ok is false when it
// did not say (request failed, or an engine too old to report capabilities).
func (o *OllamaProvider) modelCapabilities() ([]string, bool) {
	if o.host == "" || o.model == "" || o.client == nil {
		return nil, false
	}
	body, _ := json.Marshal(map[string]string{"model": o.model})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.host+"/api/show", bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var out struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || len(out.Capabilities) == 0 {
		return nil, false
	}
	return out.Capabilities, true
}
