package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	business "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// DocAIRequest represents an inline AI doc completion request.
type DocAIRequest struct {
	Action  string `json:"action"`  // "write", "expand", "summarize", "fix_grammar", "shorten", "rewrite"
	Text    string `json:"text"`    // selected text or context
	Prompt  string `json:"prompt"`  // optional custom instruction
	DocID   string `json:"doc_id"`  // document UUID for context
	Context string `json:"context"` // surrounding document text for tone/style matching
}

// buildUserContent constructs the user message incorporating optional context and prompt.
func buildUserContent(req DocAIRequest) string {
	var parts []string

	if req.Prompt != "" {
		parts = append(parts, fmt.Sprintf("Instruction: %s", req.Prompt))
	}

	if req.Context != "" {
		parts = append(parts, fmt.Sprintf("Surrounding document context (for tone and style reference):\n%s", req.Context))
	}

	parts = append(parts, fmt.Sprintf("Text:\n%s", req.Text))

	return strings.Join(parts, "\n\n")
}

// DocAIResponse represents the AI doc assistant response.
type DocAIResponse struct {
	Result   string `json:"result"`
	Action   string `json:"action"`
	Provider string `json:"provider"`
	// Notice is set when the input was shortened to fit the model's context window,
	// so the editor can say the result was produced from part of the selection rather
	// than all of it. Omitted when everything fitted — a note on every action is one
	// nobody reads.
	Notice string `json:"notice,omitempty"`
}

// Action-specific system prompts
var docActionPrompts = map[string]string{
	"write": `You are an AI writing assistant inside a document editor.
Generate content based on the user's instruction. Write in a professional, clear style.
Output ONLY the generated text — no explanations, no markdown code fences.`,

	"expand": `You are an AI writing assistant. Expand the given text with more detail, examples, and depth.
Keep the same tone and style. Output ONLY the expanded text.`,

	"summarize": `You are an AI writing assistant. Summarize the given text concisely.
Keep key points and maintain clarity. Output ONLY the summary.`,

	"fix_grammar": `You are an AI writing assistant. Fix grammar, spelling, and punctuation errors in the text.
Do NOT change the meaning or style. Output ONLY the corrected text.`,

	"shorten": `You are an AI writing assistant. Make the text more concise while preserving meaning.
Remove redundancy and wordiness. Output ONLY the shortened text.`,

	"rewrite": `You are an AI writing assistant. Rewrite the text to improve clarity and readability.
Maintain the original meaning but improve the flow. Output ONLY the rewritten text.`,
}

// DocAIComplete handles POST /ai/doc/complete
// Synchronous AI doc completion with 6 action types.
func DocAIComplete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	svc := ai.GetService()
	if !svc.IsEnabled() {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{
			"msg": "AI service is not enabled",
		})
		return
	}

	var req DocAIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err.Error(),
		})
		return
	}

	req.Action = strings.TrimSpace(req.Action)
	if len(req.Action) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "action is required",
		})
		return
	}

	systemPrompt, exists := docActionPrompts[req.Action]
	if !exists {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": fmt.Sprintf("unknown action: %s. Valid: write, expand, summarize, fix_grammar, shorten, rewrite", req.Action),
		})
		return
	}

	// Resolve the member's admin-authorized model pick (per-model breaker),
	// then gate on that breaker + the per-user rate limit — same governance as
	// AskAI, so doc/composer AI honors each member's chosen model and residency
	// instead of always using the workspace default.
	userUUID := userInfo.UserDgraphInfo.Uuid
	llm, cb, limits := svc.ResolveUserModelWithLimits(ctx, userUUID)
	ctx = ai.WithModelLimits(ctx, limits)
	ctx = ai.WithContextNoticeSink(ctx)
	// The instruction and the text it acts on, so an unreachable connector is
	// named where it would have mattered. A rewrite of somebody's paragraph
	// rarely needed one.
	ai.NoteQuestion(ctx, strings.TrimSpace(req.Prompt+" "+req.Text))
	if err := cb.Allow(); err != nil {
		statusCode := http.StatusServiceUnavailable
		if err != ai.ErrCircuitOpen {
			statusCode = http.StatusTooManyRequests
		}
		helpers.WriteJSON(w, statusCode, helpers.Envolope{"msg": err.Error()})
		return
	}
	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": err.Error()})
		return
	}

	userContent := buildUserContent(req)
	// Bound the input so a very long thread/selection can't overflow the
	// model's context window (silent front-truncation drops the system
	// prompt). Reserve room for the action prompt + the 2048-token output.
	userContent = limits.TruncateForPrompt(ctx, userContent, limits.DocInputBudget(2048))

	messages := []ai.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userContent},
	}

	opts := ai.ChatOptions{
		Temperature: 0.4,
		MaxTokens:   2048,
	}

	result, err := ai.ChatWithRescue(ctx, llm, messages, opts)
	if err != nil {
		cb.RecordResult(err)
		helpers.LogErrorWithContext(ctx, "controllers/DocAIComplete AI completion failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "AI doc completion failed",
			"err": err.Error(),
		})
		return
	}

	cb.RecordSuccess()
	// Drop any reasoning-model chain-of-thought so it never lands in the doc.
	result = business.StripReasoning(result)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "AI doc completion successful!",
		"data": DocAIResponse{
			Result:   result,
			Action:   req.Action,
			Provider: string(svc.Config.Provider()),
			Notice:   ai.TakeContextNotice(ctx).Message(),
		},
	})
}

// DocAICompleteStream handles POST /ai/doc/complete/stream
// Streams AI doc completion via Server-Sent Events (SSE).
func DocAICompleteStream(w http.ResponseWriter, r *http.Request) {
	// Enforce a streaming deadline to prevent zombie SSE connections (tunable).
	ctx, cancel := context.WithTimeout(r.Context(), ai.StreamTimeout())
	defer cancel()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	svc := ai.GetService()
	if !svc.IsEnabled() {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{
			"msg": "AI service is not enabled",
		})
		return
	}

	var req DocAIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err.Error(),
		})
		return
	}

	req.Action = strings.TrimSpace(req.Action)
	if len(req.Action) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "action is required",
		})
		return
	}

	systemPrompt, exists := docActionPrompts[req.Action]
	if !exists {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": fmt.Sprintf("unknown action: %s", req.Action),
		})
		return
	}

	// Resolve the member's model pick (per-model breaker) + gate on it and the
	// per-user rate limit — same governance as AskAI.
	userUUID := userInfo.UserDgraphInfo.Uuid
	llm, cb, limits := svc.ResolveUserModelWithLimits(ctx, userUUID)
	ctx = ai.WithModelLimits(ctx, limits)
	ctx = ai.WithContextNoticeSink(ctx)
	// The instruction and the text it acts on, so an unreachable connector is
	// named where it would have mattered. A rewrite of somebody's paragraph
	// rarely needed one.
	ai.NoteQuestion(ctx, strings.TrimSpace(req.Prompt+" "+req.Text))
	if err := cb.Allow(); err != nil {
		statusCode := http.StatusServiceUnavailable
		if err != ai.ErrCircuitOpen {
			statusCode = http.StatusTooManyRequests
		}
		helpers.WriteJSON(w, statusCode, helpers.Envolope{"msg": err.Error()})
		return
	}
	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": err.Error()})
		return
	}

	userContent := buildUserContent(req)
	// Bound the input so a very long thread/selection can't overflow the
	// model's context window (silent front-truncation drops the system
	// prompt). Reserve room for the action prompt + the 2048-token output.
	userContent = limits.TruncateForPrompt(ctx, userContent, limits.DocInputBudget(2048))

	messages := []ai.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userContent},
	}

	opts := ai.ChatOptions{
		Temperature: 0.4,
		MaxTokens:   2048,
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Streaming not supported",
		})
		return
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	chunks, err := ai.ChatStreamWithRescue(streamCtx, llm, messages, opts)
	if err != nil {
		cb.RecordResult(err)
		fmt.Fprintf(w, "data: {\"error\": %q}\n\n", ai.FriendlyProviderError(err))
		flusher.Flush()
		return
	}

	thinkFilter := &business.StreamThinkFilter{}
	for chunk := range chunks {
		if chunk.Error != nil {
			cb.RecordResult(chunk.Error)
			fmt.Fprintf(w, "data: {\"error\": %q}\n\n", ai.FriendlyProviderError(chunk.Error))
			flusher.Flush()
			return
		}

		if chunk.Content != "" {
			visible := thinkFilter.Feed(chunk.Content)
			if visible != "" {
				escaped := strings.ReplaceAll(visible, "\n", "\\n")
				escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
				fmt.Fprintf(w, "data: {\"content\": \"%s\"}\n\n", escaped)
				flusher.Flush()
			}
		}

		if chunk.Done {
			cb.RecordSuccess()
			// Emitted before "done" so the client has it when it finalises, and only
			// when something was actually shortened. streamCtx descends from ctx, so
			// this picks up both the pre-emptive trim and any rescue.
			if notice := ai.TakeContextNotice(streamCtx).Message(); notice != "" {
				if noticeJSON, merr := json.Marshal(map[string]string{"notice": notice}); merr == nil {
					fmt.Fprintf(w, "data: %s\n\n", string(noticeJSON))
					flusher.Flush()
				}
			}
			fmt.Fprintf(w, "data: {\"done\": true}\n\n")
			flusher.Flush()
			return
		}
	}

	// Stream ended
	fmt.Fprintf(w, "data: {\"done\": true}\n\n")
	flusher.Flush()
	cb.RecordSuccess()
}
