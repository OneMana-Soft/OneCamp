package ai

import "context"

// ProviderType identifies the LLM provider in use.
type ProviderType string

const (
	ProviderOllama    ProviderType = "ollama"
	ProviderOpenAI    ProviderType = "openai"
	ProviderAnthropic ProviderType = "anthropic"
	// ProviderOpenAICompatible is any OpenAI /v1-compatible endpoint the
	// admin points at (vLLM, LM Studio, OpenRouter, llama.cpp server,
	// Together, Groq, a self-hosted gateway, ...). It reuses the OpenAI
	// client with a custom base URL.
	ProviderOpenAICompatible ProviderType = "openai_compatible"
)

// ModelInfo describes a single model exposed by a provider's catalog.
// It is provider-agnostic: the FE renders the same shape for Ollama,
// OpenAI, Anthropic, and custom endpoints.
type ModelInfo struct {
	ID string `json:"id"` // canonical model id used in API calls
	// Installed is meaningful only for Ollama (true = pulled locally).
	// For cloud providers it is always true (nothing to install).
	Installed bool `json:"installed"`
	// SizeBytes is the on-disk size for installed Ollama models (0 if
	// unknown / not applicable).
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Embedding hints whether this model is an embedding model. Best
	// effort; may be false for unknown custom models.
	Embedding bool `json:"embedding,omitempty"`
}

// ModelLister is implemented by providers that can enumerate the models
// available to them. Ollama lists locally-installed models; OpenAI and
// OpenAI-compatible endpoints hit GET /v1/models; Anthropic hits
// GET /v1/models.
type ModelLister interface {
	// ListModels returns the catalog of models this provider can serve.
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// ModelManager is implemented by providers that can install/remove
// models on demand. Only Ollama supports this today (it downloads model
// weights to local disk). Cloud providers expose a fixed catalog and do
// not implement this interface.
type ModelManager interface {
	// PullModel downloads a model, emitting progress updates on the
	// returned channel until the pull completes or fails. The channel is
	// closed when done.
	PullModel(ctx context.Context, model string) (<-chan PullProgress, error)
	// DeleteModel removes a locally-installed model.
	DeleteModel(ctx context.Context, model string) error
	// Version returns the provider runtime version (e.g. the Ollama
	// server version), used to flag when an update is required for newer
	// model architectures.
	Version(ctx context.Context) (string, error)
}

// PullProgress is a single progress event during a model download.
type PullProgress struct {
	Status    string `json:"status"`              // human-readable status line
	Total     int64  `json:"total,omitempty"`     // total bytes for current layer
	Completed int64  `json:"completed,omitempty"` // bytes downloaded so far
	Done      bool   `json:"done"`                // true on the terminal event
	Error     string `json:"error,omitempty"`     // non-empty if the pull failed
	// UpdateRequired is true when the pull failed specifically because the
	// Ollama server is too old for the model's architecture. The admin UI
	// uses this to show an "update Ollama" call-to-action rather than a
	// generic failure.
	UpdateRequired bool `json:"update_required,omitempty"`
}

// ChatMessage represents a single message in a conversation.
type ChatMessage struct {
	Role    string `json:"role"`    // "system", "user", "assistant", "tool"
	Content string `json:"content"` // message content

	// Native function-calling fields (optional; ignored by text-only paths and
	// providers). On an ASSISTANT turn, ToolCalls carries the structured tool
	// calls the model made. On a TOOL turn, ToolCallID identifies which call
	// this result answers and Name is the tool's name (some providers require
	// it). Leaving these empty yields an ordinary text message, so every
	// existing caller is unaffected.
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolSpec describes one tool to a provider's NATIVE function-calling API as a
// JSON-Schema function definition. Providers that support structured tool use
// (OpenAI / Groq / vLLM, and others) render it into their `tools` payload. This
// is the reliable alternative to describing tools in the prompt text and
// parsing `<tool_call>` blocks back out.
type ToolSpec struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"` // JSON Schema (object)
}

// ToolCall is a single structured tool invocation returned by a provider's
// native function-calling API. Arguments is the raw JSON arguments object the
// model produced for the call.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCallingProvider is the OPTIONAL native function-calling capability. A
// provider that implements it lets the agent loop use structured tool calls —
// reliable, no text parsing, and impossible to fabricate a result — instead of
// the text `<tool_call>` protocol. Providers that do not implement it (or whose
// SupportsToolCalling reports false) transparently fall back to the text path,
// so the behavior is unchanged for them.
type ToolCallingProvider interface {
	// ChatWithTools sends a chat completion advertising the given tools and
	// returns the model's text content plus any structured tool calls it made.
	// When toolCalls is empty the content is the final answer.
	ChatWithTools(ctx context.Context, messages []ChatMessage, tools []ToolSpec, opts ChatOptions) (content string, toolCalls []ToolCall, err error)
	// SupportsToolCalling reports whether native tool calling is usable now
	// (endpoint/model dependent). False routes the caller to the text protocol.
	SupportsToolCalling() bool
}

// ImageInput is a single image handed to a vision model. Data is the raw
// bytes (already size/type validated by the caller) and MIME is the content
// type (e.g. "image/png", "image/jpeg", "image/gif", "image/webp").
type ImageInput struct {
	Data []byte
	MIME string
}

// VisionProvider is the optional multimodal capability. Providers that can
// accept images implement it; the vision client is built only when the admin
// configures a vision model, so callers gate on (VisionProvider, ok). Kept
// separate from LLMProvider.Chat so the text path is never touched.
type VisionProvider interface {
	// DescribeImages sends the images plus a text prompt to the model and
	// returns its textual analysis. opts.MaxTokens/Temperature apply as usual.
	DescribeImages(ctx context.Context, images []ImageInput, prompt string, opts ChatOptions) (string, error)
}

// ChatOptions configures a single LLM request.
type ChatOptions struct {
	Model       string  `json:"model,omitempty"`
	Temperature float64 `json:"temperature,omitempty"` // 0.0 - 1.0
	MaxTokens   int     `json:"max_tokens,omitempty"`
	TopP        float64 `json:"top_p,omitempty"`
	Seed        int     `json:"seed,omitempty"`

	// Think controls "thinking"/reasoning models (Ollama gemma4, deepseek-r1,
	// qwen3, …). nil = use OneCamp's default (thinking OFF), *false = force off,
	// *true = allow the model's chain-of-thought. OneCamp's tasks (summaries,
	// memory extraction, Q&A, briefings) want the final answer, not a long
	// internal reasoning trace that is slow to generate on CPU and discarded
	// anyway — so we default thinking OFF for a large latency win on these
	// models. Non-thinking models ignore the flag.
	Think *bool `json:"think,omitempty"`

	// LowLatency marks a request where a human is actively waiting (live chat,
	// the agent read-then-act loop). On a provider rate limit (429) such a
	// request fails fast with a friendly message instead of backing off for
	// many seconds; the patient retry is reserved for background jobs
	// (summaries, briefings, memory, self-test) where the wait is invisible.
	LowLatency bool `json:"low_latency,omitempty"`

	// JSONMode asks the provider to constrain the response to a single valid
	// JSON object (OpenAI / Groq / vLLM "response_format": {"type":"json_object"}).
	// Use for structured-output tasks (e.g. the board diagram agent) so small
	// models cannot wrap the JSON in prose or markdown. The prompt must still
	// instruct the model to produce JSON. Providers that do not support it
	// ignore the field.
	JSONMode bool `json:"json_mode,omitempty"`
}

// StreamChunk is a single piece of a streamed LLM response.
type StreamChunk struct {
	Content string `json:"content"`         // partial text
	Done    bool   `json:"done"`            // true when stream is complete
	Error   error  `json:"error,omitempty"` // non-nil if an error occurred
}

// LLMProvider is the core interface for text generation.
// Every LLM backend (Ollama, OpenAI, Anthropic) must implement this.
type LLMProvider interface {
	// Chat sends a synchronous chat completion request and returns the full response.
	Chat(ctx context.Context, messages []ChatMessage, opts ChatOptions) (string, error)

	// ChatStream sends a chat completion request and returns a channel that emits
	// partial response chunks as they arrive. The channel is closed when the response
	// is complete or an error occurs.
	ChatStream(ctx context.Context, messages []ChatMessage, opts ChatOptions) (<-chan StreamChunk, error)

	// ProviderName returns the identifier for this provider.
	ProviderName() ProviderType
}

// EmbeddingProvider generates vector embeddings for text.
// Some providers (Ollama, OpenAI) support both LLM and embeddings.
// Anthropic does not have an embedding API, so it will use a fallback.
type EmbeddingProvider interface {
	// Embed generates vector embeddings for one or more text inputs.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions returns the dimensionality of the embedding vectors
	// produced by this provider (e.g., 768 for nomic-embed-text, 1536 for OpenAI).
	Dimensions() int
}

// SourceRef links an AI-generated answer back to the original content.
type SourceRef struct {
	ContentType string  `json:"content_type"` // "post", "chat", "doc", "task", "comment"
	ContentUUID string  `json:"content_uuid"` // UUID of the source content
	ChannelUUID string  `json:"channel_uuid,omitempty"`
	ChannelName string  `json:"channel_name,omitempty"`
	Snippet     string  `json:"snippet,omitempty"` // text excerpt from the source
	Score       float64 `json:"score,omitempty"`   // relevance score
}
