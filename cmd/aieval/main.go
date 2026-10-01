// Command aieval is the model-behavior evaluation harness for the workspace
// AI agent. The unit tests in services/AI prove the deterministic plumbing
// (tool classification, parsing, the read/write split); this harness proves
// the part that depends on the configured model: does it reliably emit the
// RIGHT <tool_call> for a given request, and does it chain a read into a
// follow-up write?
//
// It is model-agnostic by construction — it drives whatever provider the admin
// configured (Ollama / OpenAI / Anthropic / custom OpenAI-compatible endpoint)
// through the exact same system prompt the chat path uses. Run it whenever you
// change the prompt, swap models, or upgrade a model, to catch regressions
// before they reach customers.
//
// Usage (with the same AI env the server uses, e.g. OLLAMA_HOST set):
//
//	go run ./cmd/aieval            # human-readable report
//	go run ./cmd/aieval -strict    # exit non-zero if any scenario fails (CI)
//
// It needs only the LLM endpoint — no Redis, no database — because it checks
// tool SELECTION, not execution.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	business "github.com/akashc777/OneCamp/business/AI"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func main() {
	strict := flag.Bool("strict", false, "exit non-zero if any scenario fails (for CI)")
	flag.Parse()

	if err := ai.InitAIService(); err != nil {
		fmt.Fprintf(os.Stderr, "could not initialise AI service: %v\n", err)
		os.Exit(2)
	}
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		fmt.Fprintln(os.Stderr, "AI service is not enabled/configured. Set the AI env (e.g. OLLAMA_HOST + model) and retry.")
		os.Exit(2)
	}

	cfg := ai.GetConfig()
	fmt.Printf("AI eval harness — provider=%s model=%s\n\n", cfg.Provider(), cfg.ActiveModel())

	// Same tool-enabled system prompt the chat path builds, and the same
	// scenario set the admin-dashboard self-test runs (one source of truth).
	systemPrompt := business.AskAISystemPromptWithConnectors(true, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	checks := business.RunSelfTestScenarios(ctx, svc.LLM, systemPrompt)

	passed, failed := 0, 0
	for _, c := range checks {
		status := "PASS"
		if !c.Passed {
			status = "FAIL"
			failed++
		} else {
			passed++
		}
		fmt.Printf("[%s] %s\n", status, c.Name)
		if c.Detail != "" {
			fmt.Printf("       %s\n", c.Detail)
		}
	}

	fmt.Printf("\n%d passed, %d failed (%d total)\n", passed, failed, len(checks))
	if failed > 0 {
		fmt.Println("\nNote: small local models are less reliable at tool selection. A few failures")
		fmt.Println("here usually mean the prompt needs tuning for this model, or a larger model is")
		fmt.Println("needed before enabling agent actions for customers.")
		if *strict {
			os.Exit(1)
		}
	}
}
