package business

// Rules that run when a task's due date is near or has passed (package 9
// R3): "the day before a task is due, tell its assignee", "when a task is
// overdue, raise it to high".
//
// A due date is a day: the web writes the midnight it starts in the zone of
// whoever set it, and the task list calls a task overdue once that day has
// passed. So a due trigger fires at a time of day in the rule's zone:
//
//   - task_due_soon on the day `days` before the due day (0 is the day
//     itself);
//   - task_overdue on the day after it.
//
// trigger_config holds both: {"days": 1, "at": "09:00", "tz": "Asia/Kolkata"}.
//
// A claimed sweep (workflows.due, every dueEvery) finds them: one read of each
// project that has such rules, for its open tasks due near now. Each due date
// fires once per rule, keyed by the task and the day it is due, so a moved
// date fires again. A fire time more than dueLookback old is missed, not run:
// a new rule doesn't wake every task that went overdue last month. A rule
// takes at most duePerRule fires a pass and the rest the next, which keeps a
// project's busy Friday under the per-rule limits that stop runaway rules.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/akashc777/OneCamp/models/taskread"
	authService "github.com/akashc777/OneCamp/services/Auth"
)

const (
	// dueEvery is how often the sweep looks.
	dueEvery = 15 * time.Minute
	// dueLookback is how late a due fire may still run.
	dueLookback = 24 * time.Hour
	// duePerRule bounds one rule's fires in one pass.
	duePerRule = 50
	// maxDueDays is the furthest ahead "due soon" looks.
	maxDueDays = 30
)

// dueConfig is a due trigger's trigger_config.
type dueConfig struct {
	Days int    `json:"days,omitempty"`
	At   string `json:"at"`
	TZ   string `json:"tz,omitempty"`
}

// dueTrigger is a parsed due trigger.
type dueTrigger struct {
	cfg      dueConfig
	atMinute int
	loc      *time.Location
}

// Seams for tests: the tasks a sweep reads, and the runs it finds made.
var (
	openTasks   = taskDomain.OpenTasks
	claimedKeys = workflowModel.ClaimedKeys
)

// isDueTrigger says whether a trigger is a due trigger.
func isDueTrigger(t string) bool {
	return t == workflowModel.TriggerTaskDueSoon || t == workflowModel.TriggerTaskOverdue
}

// parseDue reads and checks a due trigger's config.
func parseDue(trigger string, raw map[string]interface{}) (*dueTrigger, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("choose when it runs")
	}
	var cfg dueConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("choose how many days before the due date it runs")
	}
	return checkDue(trigger, cfg)
}

// checkDue checks a due trigger's parts.
func checkDue(trigger string, cfg dueConfig) (*dueTrigger, error) {
	if trigger == workflowModel.TriggerTaskOverdue {
		cfg.Days = 0
	}
	if cfg.Days < 0 || cfg.Days > maxDueDays {
		return nil, fmt.Errorf("a rule runs 0 to %d days before a task is due", maxDueDays)
	}
	if strings.TrimSpace(cfg.At) == "" {
		cfg.At = "09:00"
	}
	at, err := time.Parse("15:04", strings.TrimSpace(cfg.At))
	if err != nil {
		return nil, fmt.Errorf("choose the time it runs, like 09:00")
	}
	cfg.At = at.Format("15:04")
	loc, err := zoneNamed(cfg.TZ)
	if err != nil {
		return nil, err
	}
	return &dueTrigger{cfg: cfg, atMinute: at.Hour()*60 + at.Minute(), loc: loc}, nil
}

// fireAt is when a due date fires a rule: at its time of day, in its zone,
// on the day `days` before the due day (due soon) or the day after it
// (overdue).
func (d *dueTrigger) fireAt(trigger string, due time.Time) time.Time {
	y, m, day := due.In(d.loc).Date()
	shift := -d.cfg.Days
	if trigger == workflowModel.TriggerTaskOverdue {
		shift = 1
	}
	return time.Date(y, m, day+shift, 0, d.atMinute, 0, 0, d.loc)
}

// dueKey is a due fire's event key: the task and the day it is due, so one
// due date fires a rule once and a moved one fires it again.
func dueKey(trigger, taskID string, due time.Time, loc *time.Location) string {
	return trigger + ":" + taskID + ":" + due.In(loc).Format("2006-01-02")
}

// taskVars are the words a rule's text may use about a task: {task} and
// {link}.
func taskVars(t *dgraphStruct.DgraphTask) map[string]string {
	return map[string]string{"task": t.Name, "link": authService.FrontendBaseURL() + "/app/task/" + t.Uuid}
}

// sweepDue runs the due triggers whose time has come.
func sweepDue(ctx context.Context) error {
	now := clock()
	byProject := map[string][]*compiledWorkflow{}
	ahead := 0
	for _, trig := range []string{workflowModel.TriggerTaskDueSoon, workflowModel.TriggerTaskOverdue} {
		for _, cw := range candidatesFor(trig) {
			if cw.projectID == "" || cw.due == nil {
				continue
			}
			byProject[cw.projectID] = append(byProject[cw.projectID], cw)
			if cw.due.cfg.Days > ahead {
				ahead = cw.due.cfg.Days
			}
		}
	}
	// The due dates any rule fires for now: an overdue fire is the day after
	// the due day, kept dueLookback; a due-soon fire is up to `ahead` days
	// before it. A day either side covers every zone.
	from := now.Add(-dueLookback).AddDate(0, 0, -3)
	to := now.AddDate(0, 0, ahead+2)
	projects := make([]string, 0, len(byProject))
	for pid := range byProject {
		projects = append(projects, pid)
	}
	if len(projects) == 0 {
		return nil
	}
	// Every project with due rules, in one read.
	// Read as nobody: a rule never acts on a restricted task, as it is never
	// told of one's events (webhook RegisterEventListener), so its runs never
	// name one to the project (models/taskread).
	tasks, err := openTasks(taskread.AsNobody(ctx), projects, &from, &to, taskDomain.MaxOpenTasks)
	if err != nil {
		return err
	}
	for pid, rules := range byProject {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, cw := range rules {
			cw.fireDue(ctx, pid, tasks[pid], now)
		}
	}
	return nil
}

// fireDue runs a due rule for the tasks whose time has come and which it
// hasn't run for, at most duePerRule of them.
func (cw *compiledWorkflow) fireDue(ctx context.Context, projectID string, tasks []*dgraphStruct.DgraphTask, now time.Time) {
	var due []*dgraphStruct.DgraphTask
	var keys []string
	for _, t := range tasks {
		if t.DueDate == nil || t.DueDate.Year() <= 1970 {
			continue
		}
		at := cw.due.fireAt(cw.triggerType, *t.DueDate)
		if now.Before(at) || now.Sub(at) > dueLookback {
			continue
		}
		// What the conditions rule out needs no run: the run reads the task
		// again, as its owner, before it acts.
		if len(cw.elseActions) == 0 && !cw.conditions.hold(factsOf(t), now, cw.loc) {
			continue
		}
		due = append(due, t)
		keys = append(keys, dueKey(cw.triggerType, t.Uuid, *t.DueDate, cw.due.loc))
	}
	if len(due) == 0 {
		return
	}
	ran, err := claimedKeys(ctx, cw.id, keys)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Workflow due sweep: rule %s runs: %v", cw.id, err)
		return
	}
	fired := 0
	for i, t := range due {
		if ran[keys[i]] {
			continue
		}
		if fired == duePerRule {
			return
		}
		runWorkflow(ctx, cw, triggerEvent{projectID: projectID, taskID: t.Uuid, text: t.Name, eventKey: keys[i], vars: taskVars(t)})
		fired++
	}
}

func init() {
	// Claimed, so one instance runs each pass. Never renamed.
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "workflows.due", Every: dueEvery, Run: sweepDue})
}
