package ai

// Vision (multimodal) support. The OpenAI, Anthropic, and Ollama providers
// implement DescribeImages so an admin can point the optional vision model at
// any of them (gpt-4o, claude vision, local llava / llama3.2-vision, or any
// OpenAI-compatible multimodal endpoint). This lives apart from the text Chat
// path so adding vision can never regress text generation.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// defaultVisionMaxTokens caps the description length when a caller doesn't set
// one, so a single image can't burn an unbounded number of output tokens.
const defaultVisionMaxTokens = 1024

func visionMaxTokens(opts ChatOptions) int {
	if opts.MaxTokens > 0 {
		return opts.MaxTokens
	}
	return defaultVisionMaxTokens
}

// --- OpenAI (and OpenAI-compatible) vision ---

type openaiVisionImageURL struct {
	URL string `json:"url"`
}

type openaiVisionContent struct {
	Type     string                `json:"type"`
	Text     string                `json:"text,omitempty"`
	ImageURL *openaiVisionImageURL `json:"image_url,omitempty"`
}

type openaiVisionMessage struct {
	Role    string                `json:"role"`
	Content []openaiVisionContent `json:"content"`
}

type openaiVisionRequest struct {
	Model       string                `json:"model"`
	Messages    []openaiVisionMessage `json:"messages"`
	MaxTokens   *int                  `json:"max_completion_tokens,omitempty"`
	Temperature *float64              `json:"temperature,omitempty"`
}

// DescribeImages implements VisionProvider for OpenAI / OpenAI-compatible
// endpoints using the chat/completions image_url (data URL) content form.
func (o *OpenAIProvider) DescribeImages(ctx context.Context, images []ImageInput, prompt string, opts ChatOptions) (string, error) {
	if len(images) == 0 {
		return "", fmt.Errorf("no images provided")
	}
	if err := guardTokenBudget(ctx); err != nil {
		return "", err
	}
	model := o.model
	if opts.Model != "" {
		model = opts.Model
	}

	// Redact PII from the prompt text before a cloud vision call (no-op for
	// local endpoints / redaction off). Images are not regex-redactable; v1
	// scope is text egress only.
	redMsgs, rerr := o.redact(ctx, []ChatMessage{{Role: "user", Content: prompt}})
	if rerr != nil {
		return "", rerr
	}
	prompt = redMsgs[0].Content

	content := make([]openaiVisionContent, 0, len(images)+1)
	content = append(content, openaiVisionContent{Type: "text", Text: prompt})
	for _, img := range images {
		content = append(content, openaiVisionContent{
			Type:     "image_url",
			ImageURL: &openaiVisionImageURL{URL: dataURL(img.MIME, img.Data)},
		})
	}

	maxTok := visionMaxTokens(opts)
	reqBody := openaiVisionRequest{
		Model:     model,
		Messages:  []openaiVisionMessage{{Role: "user", Content: content}},
		MaxTokens: &maxTok,
	}
	if opts.Temperature > 0 && !o.isReasoningModel(model) {
		t := opts.Temperature
		reqBody.Temperature = &t
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("openai vision: marshal: %w", err)
	}

	resp, err := doHTTPWithRetry(ctx, o.client, func() (*http.Request, error) {
		r, rerr := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		r.Header.Set("Content-Type", "application/json")
		if o.apiKey != "" {
			r.Header.Set("Authorization", "Bearer "+o.apiKey)
		}
		return r, nil
	}, defaultRetryConfig())
	if err != nil {
		return "", fmt.Errorf("openai vision: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", providerStatusError("openai vision:", resp.StatusCode, b)
	}

	var cr openaiChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("openai vision: decode: %w", err)
	}
	if cr.Error != nil {
		return "", fmt.Errorf("openai vision: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("openai vision: no choices returned")
	}
	out := cr.Choices[0].Message.Content
	if cr.Usage != nil && (cr.Usage.PromptTokens > 0 || cr.Usage.CompletionTokens > 0) {
		RecordTokenSpend(ctx, cr.Usage.PromptTokens+cr.Usage.CompletionTokens)
	} else {
		// Compatible endpoint omitted usage; estimate so vision spend still
		// counts. Images dominate cost, so add a flat per-image allowance.
		RecordTokenSpend(ctx, EstimateTokens(prompt)+EstimateTokens(out)+1000*len(images))
	}
	return out, nil
}

// --- Anthropic vision ---

type anthropicVisionSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicVisionBlock struct {
	Type   string                 `json:"type"`
	Text   string                 `json:"text,omitempty"`
	Source *anthropicVisionSource `json:"source,omitempty"`
}

type anthropicVisionMessage struct {
	Role    string                 `json:"role"`
	Content []anthropicVisionBlock `json:"content"`
}

type anthropicVisionRequest struct {
	Model       string                   `json:"model"`
	MaxTokens   int                      `json:"max_tokens"`
	Messages    []anthropicVisionMessage `json:"messages"`
	Temperature *float64                 `json:"temperature,omitempty"`
}

// DescribeImages implements VisionProvider for Anthropic using image content
// blocks (base64 source).
func (a *AnthropicProvider) DescribeImages(ctx context.Context, images []ImageInput, prompt string, opts ChatOptions) (string, error) {
	if len(images) == 0 {
		return "", fmt.Errorf("no images provided")
	}
	if err := guardTokenBudget(ctx); err != nil {
		return "", err
	}
	model := a.model
	if opts.Model != "" {
		model = opts.Model
	}

	// Redact PII from the prompt before a cloud vision call (no-op otherwise).
	redMsgs, rerr := a.redact(ctx, []ChatMessage{{Role: "user", Content: prompt}})
	if rerr != nil {
		return "", rerr
	}
	prompt = redMsgs[0].Content

	blocks := make([]anthropicVisionBlock, 0, len(images)+1)
	for _, img := range images {
		blocks = append(blocks, anthropicVisionBlock{
			Type: "image",
			Source: &anthropicVisionSource{
				Type:      "base64",
				MediaType: img.MIME,
				Data:      base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}
	blocks = append(blocks, anthropicVisionBlock{Type: "text", Text: prompt})

	reqBody := anthropicVisionRequest{
		Model:     model,
		MaxTokens: visionMaxTokens(opts),
		Messages:  []anthropicVisionMessage{{Role: "user", Content: blocks}},
	}
	if opts.Temperature > 0 {
		t := opts.Temperature
		reqBody.Temperature = &t
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("anthropic vision: marshal: %w", err)
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
	}, defaultRetryConfig())
	if err != nil {
		return "", fmt.Errorf("anthropic vision: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", providerStatusError("anthropic vision:", resp.StatusCode, b)
	}

	var cr anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("anthropic vision: decode: %w", err)
	}
	if cr.Error != nil {
		return "", fmt.Errorf("anthropic vision: %s", cr.Error.Message)
	}
	if len(cr.Content) == 0 {
		return "", fmt.Errorf("anthropic vision: no content returned")
	}
	if cr.Usage != nil {
		RecordTokenSpend(ctx, cr.Usage.InputTokens+cr.Usage.OutputTokens)
	}
	return cr.Content[0].Text, nil
}

// --- Ollama vision (native /api/chat images) ---

// DescribeImages implements VisionProvider for local Ollama vision models
// (llava, llama3.2-vision) using the native images field (base64, no data:
// prefix).
func (o *OllamaProvider) DescribeImages(ctx context.Context, images []ImageInput, prompt string, opts ChatOptions) (string, error) {
	if len(images) == 0 {
		return "", fmt.Errorf("no images provided")
	}
	if err := guardTokenBudget(ctx); err != nil {
		return "", err
	}
	model := o.model
	if opts.Model != "" {
		model = opts.Model
	}

	// Redact PII from the prompt before a cloud vision call (no-op for the
	// usual local Ollama case).
	redMsgs, rerr := o.redact(ctx, []ChatMessage{{Role: "user", Content: prompt}})
	if rerr != nil {
		return "", rerr
	}
	prompt = redMsgs[0].Content

	b64 := make([]string, 0, len(images))
	for _, img := range images {
		b64 = append(b64, base64.StdEncoding.EncodeToString(img.Data))
	}

	reqBody := ollamaChatRequest{
		Model:     model,
		Stream:    false,
		KeepAlive: o.keepAlive,
		Messages: []ollamaChatMessage{{
			Role:    "user",
			Content: prompt,
			Images:  b64,
		}},
		Options: &ollamaOptions{
			Temperature: opts.Temperature,
			NumPredict:  visionMaxTokens(opts),
			NumThread:   o.numThread,
			NumCtx:      o.numCtx,
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("ollama vision: marshal: %w", err)
	}

	resp, err := doHTTPWithRetry(ctx, o.client, func() (*http.Request, error) {
		r, rerr := http.NewRequestWithContext(ctx, http.MethodPost, o.host+"/api/chat", bytes.NewReader(body))
		if rerr != nil {
			return nil, rerr
		}
		r.Header.Set("Content-Type", "application/json")
		return r, nil
	}, defaultRetryConfig())
	if err != nil {
		return "", fmt.Errorf("ollama vision: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", providerStatusError("ollama vision:", resp.StatusCode, b)
	}

	var cr ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("ollama vision: decode: %w", err)
	}
	if cr.PromptEvalCount > 0 || cr.EvalCount > 0 {
		RecordTokenSpend(ctx, cr.PromptEvalCount+cr.EvalCount)
	}
	return cr.Message.Content, nil
}

// dataURL builds a data: URL for an image, defaulting the MIME type when the
// caller couldn't determine it.
func dataURL(mime string, data []byte) string {
	if strings.TrimSpace(mime) == "" {
		mime = "image/png"
	}
	return fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(data))
}
