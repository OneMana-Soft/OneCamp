package business

// Board Sidekick — Wave 3, Requirement 3: clarify -> plan -> approve -> build.
//
// Before drawing, the Sidekick reasons about the goal. If it's underspecified
// it returns a few clarifying questions; if it's clear it returns an ordered
// plan of what it will build plus a suggested diagram type. Nothing is drawn
// here — the existing GenerateBoardDiagram(Stream) path does the building only
// after the user approves the plan. This is the Miro-Sidekick "tell me what
// you'll do, then do it on my OK" loop, layered on top of the mature board
// generator without changing it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	sidekickMaxQuestions = 4
	sidekickMaxSteps     = 10
	sidekickMaxStepLen   = 160
)

// BoardSidekickPlan is the Sidekick's pre-build proposal. Exactly one of
// Questions (clarify) or Steps (plan) is the primary payload: when Ready is
// false the goal needs clarification; when true the plan is approved-ready.
type BoardSidekickPlan struct {
	Title         string   `json:"title"`
	Ready         bool     `json:"ready"`
	Questions     []string `json:"questions"`
	Steps         []string `json:"steps"`
	SuggestedType string   `json:"suggested_type"`
}

// PlanBoardDiagram asks the model to either clarify or plan the user's goal.
// Resilient (rate limit + per-user model + circuit breaker) and never draws.
func PlanBoardDiagram(ctx context.Context, userUUID, goal, diagramType string) (*BoardSidekickPlan, error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, fmt.Errorf("describe what you want to build")
	}
	if len(goal) > boardMaxPromptLen {
		goal = goal[:boardMaxPromptLen]
	}

	diagramType = strings.ToLower(strings.TrimSpace(diagramType))

	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return nil, err
	}
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if llm == nil {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	if err := cb.Allow(); err != nil {
		return nil, err
	}

	opts := ai.ChatOptions{Temperature: 0.2, MaxTokens: 700, LowLatency: true, JSONMode: true}
	messages := []ai.ChatMessage{
		{Role: "system", Content: boardSidekickSystemPrompt()},
		{Role: "user", Content: goal},
	}
	out, err := ai.ChatWithRescue(ctx, llm, messages, opts)
	if err != nil {
		cb.RecordResult(err)
		helpers.LogErrorWithContext(ctx, "business/PlanBoardDiagram LLM failed: %+v", err)
		return nil, fmt.Errorf("the Sidekick could not plan that right now, please try again")
	}
	cb.RecordSuccess()

	plan, perr := parseSidekickPlan(out)
	if perr != nil {
		helpers.LogErrorWithContext(ctx, "business/PlanBoardDiagram parse failed: %+v raw=%q", perr, truncateForLog(out))
		// Graceful fallback: let the user build directly rather than dead-end.
		return &BoardSidekickPlan{Ready: true, Steps: []string{"Draft the diagram from your description"}, SuggestedType: normalizeSuggestedType(diagramType)}, nil
	}

	// If the caller forced a specific type, honor it over the model's guess.
	if boardDiagramTypes[diagramType] {
		plan.SuggestedType = diagramType
	} else {
		plan.SuggestedType = normalizeSuggestedType(plan.SuggestedType)
	}
	return plan, nil
}

// normalizeSuggestedType clamps a model-suggested type to a supported one,
// defaulting to a flowchart.
func normalizeSuggestedType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if boardDiagramTypes[t] {
		return t
	}
	return BoardDiagramFlow
}

// parseSidekickPlan extracts and sanitizes the Sidekick JSON. Pure +
// unit-tested. Enforces the clarify/plan invariant: when there are no steps it
// is treated as "needs clarification" (Ready=false); when there are steps it is
// approved-ready and questions are dropped.
func parseSidekickPlan(raw string) (*BoardSidekickPlan, error) {
	js := extractJSONObject(raw)
	if js == "" {
		return nil, fmt.Errorf("no JSON object found")
	}
	var p BoardSidekickPlan
	if err := json.Unmarshal([]byte(js), &p); err != nil {
		return nil, err
	}

	p.Title = sanitizeLabel(p.Title)

	cleanQ := make([]string, 0, len(p.Questions))
	for _, q := range p.Questions {
		q = sanitizeSidekickLine(q)
		if q != "" {
			cleanQ = append(cleanQ, q)
		}
		if len(cleanQ) >= sidekickMaxQuestions {
			break
		}
	}
	cleanS := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		s = sanitizeSidekickLine(s)
		if s != "" {
			cleanS = append(cleanS, s)
		}
		if len(cleanS) >= sidekickMaxSteps {
			break
		}
	}

	// Enforce the invariant deterministically rather than trusting the model's
	// own "ready" flag: a plan with steps is ready; without steps it needs
	// clarification (and must carry at least one question to be actionable).
	if len(cleanS) > 0 {
		p.Ready = true
		p.Steps = cleanS
		p.Questions = nil
	} else {
		p.Ready = false
		p.Steps = nil
		p.Questions = cleanQ
		if len(p.Questions) == 0 {
			return nil, fmt.Errorf("neither steps nor questions produced")
		}
	}
	return &p, nil
}

// sanitizeSidekickLine trims a question/step, strips control chars + leading
// list markers/numbering, and caps the length.
func sanitizeSidekickLine(s string) string {
	s = sanitizeLabel(s)
	s = strings.TrimLeft(s, "-*•0123456789. )")
	s = strings.TrimSpace(s)
	if len(s) > sidekickMaxStepLen {
		s = s[:sidekickMaxStepLen]
	}
	return s
}

// boardSidekickSystemPrompt instructs the model to clarify or plan, in strict
// JSON, never drawing.
func boardSidekickSystemPrompt() string {
	return strings.Join([]string{
		"You are the Sidekick for a collaborative whiteboard. The user gives a goal for something to draw (a flowchart, roadmap, mind map, org chart, user journey, or UI mockup).",
		"Decide whether the goal is specific enough to draw a useful diagram:",
		"- If it is TOO VAGUE to draw something genuinely useful, ask 1-3 short clarifying questions and return an empty steps list.",
		"- If it is clear enough, return an ORDERED PLAN: 3-8 short steps naming the sections/areas/branches you will draw (not how to draw them). Also pick the best diagram type.",
		"Never draw or output the diagram itself — only the plan or the questions.",
		"",
		"Output rules (MUST follow exactly):",
		"- Output ONLY a single JSON object. No markdown, no code fences, no prose.",
		"- Schema: {\"title\": string, \"ready\": boolean, \"questions\": [string], \"steps\": [string], \"suggested_type\": \"flow\"|\"roadmap\"|\"journey\"|\"mindmap\"|\"orgchart\"|\"wireframe\"|\"ui-mobile\"|\"ui-desktop\"}.",
		"- When asking questions: ready=false, steps=[], 1-3 entries in questions.",
		"- When planning: ready=true, questions=[], 3-8 short entries in steps, and a suggested_type.",
		"- Keep every question and step to a single concise line.",
	}, "\n")
}
