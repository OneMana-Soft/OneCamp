package business

import (
	"context"
	"fmt"
	"strings"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// ai.go — the built-in "OneCamp AI" app. /ask answers a quick question using
// the workspace's own admin-configured AI provider/model (Anthropic, OpenAI,
// Ollama, …) via the in-process AI service. No third-party key required: it
// reuses whatever the workspace already runs for AI, so it works out of the box
// the moment AI is enabled.
//
// This is the OneCamp-native answer to the "OpenAI /ask" marketplace entry —
// strictly better as a default because it doesn't depend on a separate OpenAI
// account and respects the workspace's chosen model.

const oneCampAIAppSlug = "onecamp-ai"

func init() {
	Register("ask", handleAsk)
	RegisterAppTest(oneCampAIAppSlug, testOneCampAI)
}

// handleAsk runs a single-shot completion against the workspace AI service and
// returns the answer as an ephemeral message to the invoker.
func handleAsk(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	prompt := strings.TrimSpace(cc.Text)
	if prompt == "" {
		return errorResponse("Usage: `/ask <your question>`"), nil
	}

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return errorResponse("OneCamp AI isn't enabled yet. An admin can turn it on under Admin → AI Models."), nil
	}

	// Bound the call so a slow model can't hang the request.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Records any shortening so the answer can say it happened. The reply is a rendered
	// block rather than plain text, so the footnote goes in the answer section below.
	cctx = ai.WithContextNoticeSink(cctx)
	ai.NoteQuestion(cctx, prompt)

	messages := []ai.ChatMessage{
		{Role: "system", Content: "You are OneCamp's helpful assistant. Answer concisely and accurately. If you are unsure, say so."},
		{Role: "user", Content: prompt},
	}

	answer, err := ai.ChatWithRescue(cctx, svc.LLM, messages, ai.ChatOptions{Temperature: 0.4, MaxTokens: 1024})
	if err != nil {
		logErr(ctx, "handleAsk/Chat", err)
		return errorResponse("OneCamp AI couldn't answer that right now. Try again in a moment."), nil
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return ephemeral("OneCamp AI didn't return an answer. Try rephrasing your question."), nil
	}

	return &commandAdapter.CommandResponse{
		ResponseType: "ephemeral",
		Ephemeral:    true,
		Blocks: []commandAdapter.Block{
			{
				Type: "context",
				Text: &commandAdapter.BlockText{Type: "mrkdwn", Text: fmt.Sprintf("*You asked:* %s", htmlEscape(prompt))},
			},
			{
				Type: "section",
				Text: &commandAdapter.BlockText{Type: "mrkdwn", Text: ai.AppendNoticeFootnote(answer, ai.TakeContextNotice(cctx))},
			},
		},
		TriggerID: cc.TriggerID,
	}, nil
}

// testOneCampAI reports whether the workspace AI service is enabled and ready,
// so an admin can confirm /ask will work before relying on it. Registered as
// the OneCamp AI app's Test probe; appID is unused (the service is workspace-
// global, not per-app).
func testOneCampAI(_ context.Context, _ uuid.UUID) *commandAdapter.AppTestResult {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return &commandAdapter.AppTestResult{Success: false, Message: "OneCamp AI is not enabled. Turn it on under Admin → AI Models."}
	}
	if cfg := ai.GetConfig(); cfg != nil {
		return &commandAdapter.AppTestResult{Success: true, Message: fmt.Sprintf("Ready — using %s / %s.", cfg.Provider(), cfg.ActiveModel())}
	}
	return &commandAdapter.AppTestResult{Success: true, Message: "Ready — workspace AI is enabled."}
}
