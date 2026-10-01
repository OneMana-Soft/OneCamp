package business

// codePRLLMProxy.go — the internal, token-gated LLM endpoint the code-runner
// sidecar calls back to during a coding run. The sidecar is network-restricted
// and has NO model of its own; the main server owns the model-agnostic LLM +
// provider secrets and meters every call, so the runner asks here for each
// completion in its edit/verify loop.
//
// Trust: authenticated by the SAME shared secret as the runner (the decrypted
// code-PR runner token), compared in constant time. It refuses unless code-PR is
// enabled and a runner token is configured, so it's inert until the feature is
// deliberately turned on. It routes through the shared AI service, so the
// workspace token budget + circuit breaker apply exactly as everywhere else.

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"

	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// codePRProxyMaxTokens / defaults bound one proxied completion.
const (
	codePRProxyMaxTokens = 8192
	codePRProxyMinTokens = 256
)

// CodePRLLMProxy authenticates the runner and returns a model completion for its
// edit loop. token is the shared runner secret (X-Runner-Token). Returns a
// user-safe error on auth failure / disabled feature / provider error.
func CodePRLLMProxy(ctx context.Context, token string, messages []ai.ChatMessage, maxTokens int, temperature float64) (string, error) {
	settings, err := aiModels.GetSettings(ctx)
	if err != nil {
		return "", fmt.Errorf("configuration unavailable")
	}
	if !settings.CodePREnabled {
		return "", fmt.Errorf("code PRs are not enabled")
	}
	if len(settings.CodePRRunnerTokenEnc) == 0 {
		return "", fmt.Errorf("no runner token configured")
	}
	want, derr := aiModels.DecryptAPIKey(settings.CodePRRunnerTokenEnc)
	if derr != nil || strings.TrimSpace(want) == "" {
		return "", fmt.Errorf("runner token unavailable")
	}
	// Constant-time auth: the caller must present the exact runner secret.
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(strings.TrimSpace(want))) != 1 {
		return "", fmt.Errorf("unauthorized")
	}

	if len(messages) == 0 {
		return "", fmt.Errorf("no messages")
	}
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return "", fmt.Errorf("AI service is not enabled")
	}
	if err := svc.Resiliency.CB.Allow(); err != nil {
		return "", err
	}
	// Use the OPTIONAL dedicated code-run model when the admin configured one,
	// so coding runs don't compete with the chat model's provider quota; else
	// fall back to the chat model.
	llm := svc.LLM
	if svc.CodeRun != nil {
		llm = svc.CodeRun
	}

	if maxTokens <= 0 || maxTokens > codePRProxyMaxTokens {
		maxTokens = codePRProxyMaxTokens
	}
	if maxTokens < codePRProxyMinTokens {
		maxTokens = codePRProxyMinTokens
	}
	if temperature < 0 {
		temperature = 0
	}

	answer, cerr := ai.ChatWithRescue(ctx, llm, messages, ai.ChatOptions{Temperature: temperature, MaxTokens: maxTokens})
	if cerr != nil {
		svc.Resiliency.CB.RecordResult(cerr)
		return "", fmt.Errorf("completion failed")
	}
	svc.Resiliency.CB.RecordSuccess()
	return answer, nil
}
