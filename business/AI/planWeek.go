package business

// "Plan my week" with the workspace's own model (calendar B13, CAL52-T3):
// the model orders the viewer's open tasks and says why for each, in one
// line; business/Availability places them in free time, so the model never
// gives a time. Through the task extractor's path (its rate limit, model,
// breaker and JSON retry), with an object at the top ({"plan": [...]}) as
// JSON mode needs; only the tasks it was given are kept. Every call is in
// the AI activity record: the model, where it runs and how many tasks, never
// their names.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	availabilityBusiness "github.com/akashc777/OneCamp/business/Availability"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// ActionCalendarWeekPlanned is the record's action for a plan the model
// ordered (or was asked to and didn't).
const ActionCalendarWeekPlanned = "ai.calendar.week_planned"

const planReasonMaxLen = 120

// A plan's token budget holds the model's whole answer: a row is a task id
// (a uuid alone is ~25 tokens) and a reason of at most 12 words, with its
// JSON (V-811: 40 a task was below one row). A model that thinks gets its
// room to think from the provider (services/AI outputBudget), as every
// caller does.
const (
	weekPlanAnswerTokens  = 60
	weekPlanTokensPerTask = 80
)

// weekPlanMaxTokens is the budget for a week's plan of n tasks.
func weekPlanMaxTokens(n int) int {
	return weekPlanAnswerTokens + weekPlanTokensPerTask*n
}

// The AI edition orders plans (business/Availability's slot).
func init() { availabilityBusiness.RegisterPlanOrderer(orderPlan) }

// planModelInfo says which model llm is and where it runs: a seam for tests.
var planModelInfo = func(ctx context.Context, userUUID string, llm ai.LLMProvider) ai.DocModel {
	svc := ai.GetService()
	if svc == nil {
		return ai.DocModel{}
	}
	return svc.DescribeModel(ctx, userUUID, llm)
}

const planWeekPrompt = `You plan a person's working week. You are given their open tasks, each with an id, a name, its project, its due date, how long its block is, and what it waits on.

Order the tasks in the order they should be worked on this week: what is due soonest or unblocks other work first, then the rest. For each, write one short reason (at most 12 words) a person would agree with.

Answer with a JSON object only, exactly in this shape:
{"plan":[{"id":"<the task's id, copied exactly>","reason":"<one short reason>"}]}
Use only the ids given. Do not invent tasks, times or dates.`

// rawPlanItem is one entry of the model's plan.
type rawPlanItem struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// orderPlan is the model's order for a plan's tasks: nil, nil when AI is off
// (no model is asked); an error when the call fails or its answer isn't a
// plan, so the plan goes by due date.
func orderPlan(ctx context.Context, who *userModels.UserInfo, tasks []availabilityBusiness.PlanTask, from, to time.Time) (*availabilityBusiness.PlanOrder, error) {
	if !extractionOn() {
		return nil, nil
	}
	asker := who.UserDgraphInfo.Uuid
	llm, cb, err := extractionModel(ctx, asker)
	if err != nil {
		return nil, err
	}
	if llm == nil || cb == nil {
		return nil, nil
	}
	if err := cb.Allow(); err != nil {
		return nil, err
	}
	model := planModelInfo(ctx, asker, llm)
	out, err := chatExtraction(ctx, llm, cb, planWeekPrompt, planWeekMessage(tasks, from, to), ai.ChatOptions{Temperature: 0.2, MaxTokens: weekPlanMaxTokens(len(tasks))}, isValidPlanJSON)
	var order []availabilityBusiness.Ordered
	if err == nil {
		items, ok := decodePlanItems(out)
		if !ok {
			err = fmt.Errorf("the model's answer isn't a plan")
		}
		order = keptPlanOrder(tasks, items)
	}
	recordPlan(ctx, who, model, len(tasks), len(order), err == nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI/orderPlan model call failed: %v", err)
		return nil, err
	}
	return &availabilityBusiness.PlanOrder{Order: order, Model: model.Label, Local: model.Local}, nil
}

// planWeekMessage is the week and the tasks, one a line. Pure.
func planWeekMessage(tasks []availabilityBusiness.PlanTask, from, to time.Time) string {
	in := map[string]bool{}
	for _, t := range tasks {
		in[t.UUID] = true
	}
	var b strings.Builder
	b.WriteString("Today is " + from.UTC().Format("Monday 2006-01-02") + " (UTC). The week runs to " + to.UTC().Format("Monday 2006-01-02") + ". " + UpcomingDays(from.UTC(), 7) + "\n\nTasks:\n")
	for _, t := range tasks {
		fmt.Fprintf(&b, "- id %s | %s", t.UUID, cleanLabel(t.Name, extractMaxTitleLen))
		if t.ProjectName != "" {
			b.WriteString(" | project " + cleanLabel(t.ProjectName, extractMaxTitleLen))
		}
		if t.Due != nil {
			b.WriteString(" | due " + t.Due.UTC().Format("Mon 2006-01-02"))
		}
		fmt.Fprintf(&b, " | %d min", t.Minutes)
		if t.Blocker != nil && in[t.Blocker.UUID] {
			b.WriteString(" | waits on " + t.Blocker.UUID)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// decodePlanItems reads the model's plan: an object with a "plan" list (in
// a code fence or not). A bare list is not a plan: JSON mode answers an
// object, and a list is what loses its single item elsewhere. Pure.
func decodePlanItems(out string) ([]rawPlanItem, bool) {
	s := strings.TrimSpace(out)
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```"), "```")
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return nil, false
	}
	var wrapped struct {
		Plan *[]rawPlanItem `json:"plan"`
	}
	if json.Unmarshal([]byte(s), &wrapped) != nil || wrapped.Plan == nil {
		return nil, false
	}
	return *wrapped.Plan, true
}

// isValidPlanJSON is the retry's check. Pure.
func isValidPlanJSON(out string) bool {
	_, ok := decodePlanItems(out)
	return ok
}

// keptPlanOrder is the model's order of the tasks it was given, each once,
// with its reason cleaned: an id it made up is dropped. Pure.
func keptPlanOrder(tasks []availabilityBusiness.PlanTask, items []rawPlanItem) []availabilityBusiness.Ordered {
	known := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		known[t.UUID] = true
	}
	out := make([]availabilityBusiness.Ordered, 0, len(items))
	for _, it := range items {
		id := strings.ToLower(strings.TrimSpace(it.ID))
		if !known[id] {
			continue
		}
		known[id] = false
		out = append(out, availabilityBusiness.Ordered{UUID: id, Reason: cleanLabel(it.Reason, planReasonMaxLen)})
	}
	return out
}

// recordPlan writes the model call to the AI activity record: the model,
// where it runs, how many tasks it was given and ordered, and whether it
// answered; never the tasks. A record that fails is logged: the plan writes
// nothing, so there is nothing to refuse.
func recordPlan(ctx context.Context, who *userModels.UserInfo, model ai.DocModel, tasks, ordered int, answered bool) {
	summary := "Ordered a week's plan"
	if !answered {
		summary = "Asked for a week's plan; no answer"
	}
	if model.Label != "" {
		where := "on your server"
		if !model.Local {
			where = "outside your server"
		}
		summary += " with " + model.Label + " " + where
	}
	summary += fmt.Sprintf(" (%d tasks)", tasks)
	meta := map[string]interface{}{"model": model.Label, "local": model.Local, "tasks": tasks, "ordered": ordered, "answered": answered}
	id, email := actorOf(who)
	if err := recordEntry(ctx, id, email, auditBusiness.ActorHuman, ActionCalendarWeekPlanned, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI/recordPlan not recorded: %v", err)
	}
}
