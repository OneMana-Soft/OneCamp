package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// coding_llm.go — the sidecar's client for the main server's internal coding-LLM
// proxy. The runner has NO model of its own (the main server owns the
// model-agnostic LLM + provider secrets, and meters every call against the
// workspace/user/agent/channel token budgets), so the edit loop asks the main
// server for each completion over the internal network. The proxy token is a
// shared internal secret; it is never logged.

// llmMessage mirrors the main server's ai.ChatMessage on the wire.
type llmMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// llmProxyRequest is the POST body to the coding-LLM proxy.
type llmProxyRequest struct {
	JobID       string       `json:"job_id"`
	Messages    []llmMessage `json:"messages"`
	MaxTokens   int          `json:"max_tokens"`
	Temperature float64      `json:"temperature"`
}

// llmProxyResponse is the proxy's reply (the raw model completion).
type llmProxyResponse struct {
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

// llmClient calls the main server's coding-LLM proxy.
type llmClient struct {
	url    string
	token  string
	jobID  string
	client *http.Client
}

func newLLMClient(url, token, jobID string) *llmClient {
	return &llmClient{
		url:    strings.TrimRight(strings.TrimSpace(url), "/"),
		token:  strings.TrimSpace(token),
		jobID:  jobID,
		client: &http.Client{Timeout: 3 * time.Minute},
	}
}

// configured reports whether the proxy coordinates are present.
func (c *llmClient) configured() bool { return c.url != "" }

// complete sends messages to the proxy and returns the model's completion text.
// Bounded body read; a proxy error / non-200 is surfaced as an error so the
// edit loop can stop cleanly.
func (c *llmClient) complete(ctx context.Context, messages []llmMessage, maxTokens int, temperature float64) (string, error) {
	if !c.configured() {
		return "", fmt.Errorf("no LLM proxy configured")
	}
	body, err := json.Marshal(llmProxyRequest{
		JobID:       c.jobID,
		Messages:    messages,
		MaxTokens:   maxTokens,
		Temperature: temperature,
	})
	if err != nil {
		return "", fmt.Errorf("marshal llm request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("X-Runner-Token", c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm proxy unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm proxy returned %d", resp.StatusCode)
	}
	var out llmProxyResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("unreadable llm proxy response")
	}
	if strings.TrimSpace(out.Error) != "" {
		return "", fmt.Errorf("llm proxy error: %s", out.Error)
	}
	return out.Content, nil
}
