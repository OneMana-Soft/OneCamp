package business

// A project's plan drafted from a sentence ("launch our app in six weeks"):
// the AI writes it as a project template (business/ProjectTemplate), which the
// person reads in New project before anything is made. Nothing is created
// here, and what the model says is checked the way a template file is, so a
// plan the AI got wrong is a message, never a broken project.
//
// A small local model on a server's CPU takes one to two minutes to write a
// plan, longer than a request may stay open behind a proxy (Cloudflare closes
// it at 100 s). So a draft runs on its own and the app asks for it until it's
// there; the person's latest draft is kept in Redis for a few minutes.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	templateBusiness "github.com/akashc777/OneCamp/business/ProjectTemplate"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// The prompt was tried on the demo's 3B model: an example status in the shape
// was copied into every plan (a "QA" column for an offsite), so the shape
// shows none, and the rules say when one is right.
const projectPlanPrompt = `You plan projects for a small team. Given what the person wants to do, write the plan as tasks.
Answer with one JSON object and nothing else, in this shape:
{"name": "a short project name", "description": "one sentence on what the project delivers",
 "statuses": [],
 "tasks": [{"name": "...", "details": "what done looks like, one or two sentences", "priority": "low|medium|high", "start_day": 0, "due_day": 3, "steps": []}]}
Rules:
- 6 to 14 tasks, in the order the work happens. Name each task with a verb ("Book the venue").
- Days count from 0, the day the project starts. due_day is never before start_day.
- Spread the tasks over the whole timeframe the person gives: "in six weeks" means the last task is due near day 42, "in two weeks" near day 14, "by the end of the month" near day 28. Without a timeframe, choose a realistic one for the work.
- statuses: leave it empty unless the work needs a review step the team will move tasks into, such as "QA" for software or "Client review" for client work. At most two. Category is "inReview" for a review step, otherwise "inProgress". Never use the names To do, In progress, In review, Done, Backlog or Cancelled.
- steps: leave it empty unless a task has distinct parts; then at most five short sub-steps.
- Never name people, companies or real dates. Never invent facts about the person's business.`

// The most a description may be, the fewest characters worth planning from,
// and what a plan may hold.
const (
	planMaxInput  = 600
	planMinInput  = 8
	planMaxTasks  = 25
	planMaxSteps  = 8
	planMaxTokens = 1600
	// planBudget is the longest a draft may run. Past it, one that hasn't
	// reported back is taken for lost.
	planBudget = 4 * time.Minute
)

// A draft's states.
const (
	PlanDrafting = "drafting"
	PlanDone     = "done"
	PlanFailed   = "failed"
)

var (
	// ErrPlanTooShort: the description doesn't say enough to plan from.
	ErrPlanTooShort = errors.New("describe the project in a sentence")
	// ErrPlanGone: no such draft for this person, or it has expired.
	ErrPlanGone = errors.New("plan draft not found")
)

// PlanDraft is a person's latest plan draft: drafting, done with the plan, or
// failed with what to tell them.
type PlanDraft struct {
	ID       string                     `json:"id"`
	State    string                     `json:"state"`
	Template *templateBusiness.Template `json:"template,omitempty"`
	Msg      string                     `json:"msg,omitempty"`
	Started  time.Time                  `json:"started"`
}

type rawPlan struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Statuses    []struct {
		Name     string `json:"name"`
		Category string `json:"category"`
	} `json:"statuses"`
	Tasks []struct {
		Name     string   `json:"name"`
		Details  string   `json:"details"`
		Priority string   `json:"priority"`
		StartDay *int     `json:"start_day"`
		DueDay   *int     `json:"due_day"`
		Steps    []string `json:"steps"`
	} `json:"tasks"`
}

func decodePlan(out string) (*rawPlan, bool) {
	js := extractJSONObject(StripReasoning(out))
	if js == "" {
		return nil, false
	}
	var p rawPlan
	if json.Unmarshal([]byte(js), &p) != nil || len(p.Tasks) == 0 {
		return nil, false
	}
	return &p, true
}

// planFromModel is the model's answer as a usable template, or false when it
// isn't one. Everything the model could get wrong is mended rather than
// refused: an unknown priority is dropped, a day is kept in range, a status
// that would clash is left out. Pure.
func planFromModel(out, fallbackName string) (templateBusiness.Template, bool) {
	p, ok := decodePlan(out)
	if !ok {
		return templateBusiness.Template{}, false
	}
	t := templateBusiness.Template{
		Name:        helpers.OneLine(helpers.FirstNonBlank(p.Name, fallbackName), templateBusiness.MaxNameLength),
		Description: helpers.OneLine(p.Description, templateBusiness.MaxAboutLength),
	}
	seen := map[string]bool{}
	for _, st := range p.Statuses {
		in := taskStatusBusiness.Input{Name: st.Name, Category: st.Category, Color: "violet"}
		if len(t.Statuses) == 2 || taskStatusBusiness.IsClosed(in.Category) || in.Validate() != nil || seen[strings.ToLower(in.Name)] {
			continue
		}
		seen[strings.ToLower(in.Name)] = true
		t.Statuses = append(t.Statuses, in)
	}
	day := func(d *int) *int {
		if d == nil {
			return nil
		}
		v := min(max(*d, 0), templateBusiness.MaxDay)
		return &v
	}
	for _, rt := range p.Tasks {
		name := helpers.OneLine(rt.Name, templateBusiness.MaxTaskName)
		if name == "" || len(t.Tasks) == planMaxTasks {
			continue
		}
		task := templateBusiness.Task{Name: name, Description: helpers.PlainTextToHTML(rt.Details), StartDay: day(rt.StartDay), DueDay: day(rt.DueDay)}
		if pr := strings.ToLower(strings.TrimSpace(rt.Priority)); templateBusiness.ValidPriority(pr) {
			task.Priority = pr
		}
		if task.StartDay != nil && task.DueDay != nil && *task.StartDay > *task.DueDay {
			task.StartDay = nil
		}
		for _, step := range rt.Steps {
			if step = helpers.OneLine(step, templateBusiness.MaxTaskName); step != "" && len(task.Subtasks) < planMaxSteps {
				task.Subtasks = append(task.Subtasks, templateBusiness.Subtask{Name: step})
			}
		}
		t.Tasks = append(t.Tasks, task)
	}
	checked, err := templateBusiness.Check(t)
	return checked, err == nil
}

// A timeframe as people write it: "in six weeks", "a 3-month project", "over
// 10 days", "a fortnight".
var timeframeRe = regexp.MustCompile(`(?i)\b(\d{1,3}|an?|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)[\s-]+(days?|weeks?|fortnights?|months?)\b`)

var numberWords = map[string]int{"a": 1, "an": 1, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12}

// timeframeDays is the longest timeframe the person wrote, in days, or 0 when
// they gave none. Pure.
func timeframeDays(description string) int {
	longest := 0
	for _, m := range timeframeRe.FindAllStringSubmatch(description, -1) {
		n, ok := numberWords[strings.ToLower(m[1])]
		if !ok {
			n, _ = strconv.Atoi(m[1])
		}
		unit := strings.TrimSuffix(strings.ToLower(m[2]), "s")
		perUnit := map[string]int{"day": 1, "week": 7, "fortnight": 14, "month": 30}[unit]
		longest = max(longest, n*perUnit)
	}
	return min(longest, templateBusiness.MaxDay)
}

// fitTimeframe stretches or squeezes a plan's days so it ends near the
// timeframe the person gave: small models write "in six weeks" and stop at
// day 16. Days keep their order and their spacing in proportion. A plan
// already within a third of the timeframe is left alone. Pure.
func fitTimeframe(t *templateBusiness.Template, days int) {
	if days < 2 {
		return
	}
	last := 0
	for _, task := range t.Tasks {
		for _, d := range []*int{task.StartDay, task.DueDay} {
			if d != nil {
				last = max(last, *d)
			}
		}
		for _, s := range task.Subtasks {
			if s.DueDay != nil {
				last = max(last, *s.DueDay)
			}
		}
	}
	if last == 0 || (float64(last) >= 0.67*float64(days) && float64(last) <= 1.5*float64(days)) {
		return
	}
	factor := float64(days) / float64(last)
	scale := func(d *int) *int {
		if d == nil {
			return nil
		}
		v := min(int(math.Round(float64(*d)*factor)), templateBusiness.MaxDay)
		return &v
	}
	for i := range t.Tasks {
		task := &t.Tasks[i]
		task.StartDay, task.DueDay = scale(task.StartDay), scale(task.DueDay)
		for j := range task.Subtasks {
			task.Subtasks[j].DueDay = scale(task.Subtasks[j].DueDay)
		}
	}
}

// draftPlan asks the model for the plan and makes it a template. It can take
// minutes on a small model; ctx bounds it.
func draftPlan(ctx context.Context, svc *ai.AIService, userUUID, description string) (*templateBusiness.Template, error) {
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if llm == nil {
		return nil, ai.ErrAIDisabled
	}
	if err := cb.Allow(); err != nil {
		return nil, err
	}
	fallback := helpers.OneLine(description, 40)
	out, err := ai.ChatJSONWithRetry(ctx, llm, cb, projectPlanPrompt, "What the person wants to do:\n"+description,
		ai.ChatOptions{Temperature: 0.3, MaxTokens: planMaxTokens},
		func(s string) bool { _, ok := planFromModel(s, fallback); return ok })
	if err != nil {
		return nil, err
	}
	t, ok := planFromModel(out, fallback)
	if !ok {
		return nil, errors.New("the model's plan couldn't be read")
	}
	fitTimeframe(&t, timeframeDays(description))
	return &t, nil
}

// planFailure is what to tell the person when a draft fails.
func planFailure(err error) string {
	switch {
	case errors.Is(err, ai.ErrRateLimited):
		return "That's a lot of AI requests in a short time. Try again in a minute."
	case errors.Is(err, ai.ErrCircuitOpen):
		return "The AI isn't answering just now. Try again in a few minutes, or pick a template."
	case errors.Is(err, context.DeadlineExceeded):
		return "The AI took too long to draft the plan. Try a shorter description, or pick a template."
	default:
		return "The plan couldn't be drafted just now. Try again, or pick a template."
	}
}

// StartProjectPlan begins drafting a plan for what the person wrote and
// returns at once with the draft, read back with ProjectPlan. A person has
// one draft at a time: asking again while one runs returns that one. Without
// Redis to keep the draft, it is made within the request instead.
func StartProjectPlan(ctx context.Context, u *userModels.UserInfo, description string) (*PlanDraft, error) {
	description = helpers.OneLine(description, planMaxInput)
	if utf8.RuneCountInString(description) < planMinInput {
		return nil, ErrPlanTooShort
	}
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, ai.ErrAIDisabled
	}
	userUUID := u.UserDgraphInfo.Uuid
	if !redisStore.IsAvailable() {
		if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		t, err := draftPlan(ctx, svc, userUUID, description)
		if err != nil {
			return nil, err
		}
		return &PlanDraft{ID: uuid.NewString(), State: PlanDone, Template: t, Started: time.Now()}, nil
	}

	key := []string{userUUID}
	fresh := &PlanDraft{ID: uuid.NewString(), State: PlanDrafting, Started: time.Now()}
	running := fresh
	err := redisStore.UpdateJSONAtomic(ctx, registry.AIPlanDraft, key, registry.AIPlanDraft.TTL, func(cur PlanDraft, found bool) PlanDraft {
		if found && cur.State == PlanDrafting && time.Since(cur.Started) < planBudget {
			running = &cur
			return cur
		}
		running = fresh
		return *fresh
	})
	if err != nil {
		return nil, err
	}
	if running != fresh {
		return running, nil
	}
	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		finishPlan(ctx, userUUID, fresh.ID, nil, err)
		return nil, err
	}
	bg := context.WithoutCancel(ctx)
	helpers.GoSafeNamed("ai plan draft", func() {
		ctx, cancel := context.WithTimeout(bg, planBudget)
		defer cancel()
		t, err := draftPlan(ctx, svc, userUUID, description)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/AI/StartProjectPlan draft err: %+v", err)
		}
		finishPlan(bg, userUUID, fresh.ID, t, err)
	})
	return fresh, nil
}

// finishPlan records how draft id ended, if it is still the person's latest.
func finishPlan(ctx context.Context, userUUID, id string, t *templateBusiness.Template, err error) {
	werr := redisStore.UpdateJSONAtomic(ctx, registry.AIPlanDraft, []string{userUUID}, registry.AIPlanDraft.TTL, func(cur PlanDraft, found bool) PlanDraft {
		if !found || cur.ID != id {
			return cur
		}
		if err != nil {
			cur.State, cur.Msg = PlanFailed, planFailure(err)
		} else {
			cur.State, cur.Template = PlanDone, t
		}
		return cur
	})
	if werr != nil {
		helpers.LogErrorWithContext(ctx, "business/AI/finishPlan err: %+v", werr)
	}
}

// ProjectPlan is the person's draft with that id: still drafting, done, or
// failed. ErrPlanGone when there is no such draft. A draft that outlived its
// budget without reporting back (the server restarted) reads as failed.
func ProjectPlan(ctx context.Context, userUUID, id string) (*PlanDraft, error) {
	var d PlanDraft
	found, err := redisStore.GetJSON(ctx, registry.AIPlanDraft, []string{userUUID}, &d)
	if err != nil {
		return nil, err
	}
	if !found || d.ID != id {
		return nil, ErrPlanGone
	}
	if d.State == PlanDrafting && time.Since(d.Started) > planBudget+30*time.Second {
		d.State, d.Msg = PlanFailed, planFailure(context.DeadlineExceeded)
	}
	return &d, nil
}
