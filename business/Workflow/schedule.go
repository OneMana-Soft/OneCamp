package business

// Rules that run on time (package 9 R3; design/projects-vs-asana-jira/
// PROGRAM.md, "R3 plan"): "every Monday at 09:00, make the review task",
// "every weekday at 17:00, for each open task that's overdue, tell its
// assignee".
//
// A schedule is a repeat rule (RFC 5545, the subset the builder offers: every
// day, on chosen days of the week, or on a day of the month), a time and a
// zone, in the rule's trigger_config:
//
//	{"repeat": "FREQ=WEEKLY;BYDAY=MO", "at": "09:00", "tz": "Europe/London",
//	 "from": "2026-10-20", "each_task": false}
//
// Its next times come from the calendar's recurrence engine (business/
// Recurrence) in the schedule's own zone, so 09:00 stays 09:00 when the clocks
// change. They don't come from business/Scheduler's own repeat rules, which
// count in UTC.
//
// It runs through the one queue for delayed work (business/Scheduler): each
// live schedule rule has one waiting job, which runs the rule and then plans
// the next (models/postgres/Workflow scheduleModel.go). Saving or turning on a
// rule plans its job, and pausing or deleting it cancels the job. An hourly
// claimed pass plans any live rule left without one.
//
// Each time is one run, keyed by the time it was due, so a job that runs
// twice runs the rule once. A run more than lateLimit late is skipped and
// recorded rather than run at the wrong time. Either way the next one is
// planned.
//
// A schedule acts once: it makes a task, and the steps after that act on the
// task it made. With each_task it acts for each open task in the project
// that its conditions hold for, at most maxEachTask of them in a run; the run
// records how many matched.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	recurrenceBusiness "github.com/akashc777/OneCamp/business/Recurrence"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// lateLimit is how late a scheduled run may start: later than this it is
// skipped and recorded, never run at a time nobody chose.
const lateLimit = 2 * time.Hour

// replanBatch bounds the hourly pass that plans schedule rules left without a
// waiting job.
const replanBatch = 200

// scheduleConfig is a schedule rule's trigger_config.
type scheduleConfig struct {
	Repeat   string `json:"repeat"`
	At       string `json:"at"`
	TZ       string `json:"tz,omitempty"`
	From     string `json:"from"`
	EachTask bool   `json:"each_task,omitempty"`
}

// schedule is a parsed, checked schedule.
type schedule struct {
	cfg      scheduleConfig
	rule     recurrenceBusiness.Rule
	loc      *time.Location
	atMinute int
	from     time.Time // the first day, at midnight in loc
}

// errScheduleRepeat is a repeat the builder doesn't offer.
var errScheduleRepeat = errors.New("a schedule runs every day, on days of the week you choose, or on a day of the month")

// Seams for tests: the queue a schedule's job waits in.
var (
	planScheduleJob   = workflowModel.PlanScheduleJob
	planScheduleJobs  = workflowModel.PlanScheduleJobs
	cancelScheduleJob = workflowModel.CancelScheduleJob
	unplanned         = workflowModel.UnplannedSchedules
)

// parseSchedule reads and checks a schedule. A missing first day is today in
// its zone, so the first run is the next one from when it is saved.
func parseSchedule(raw map[string]interface{}, now time.Time) (*schedule, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("choose when it runs")
	}
	var cfg scheduleConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("choose when it runs")
	}
	return checkSchedule(cfg, now)
}

// checkSchedule checks a schedule's parts and fills its first day.
func checkSchedule(cfg scheduleConfig, now time.Time) (*schedule, error) {
	cfg.Repeat = strings.TrimSpace(cfg.Repeat)
	if cfg.Repeat == "" {
		return nil, fmt.Errorf("choose how often it runs")
	}
	rule, err := recurrenceBusiness.Rules.Parse(cfg.Repeat)
	if err != nil {
		return nil, fmt.Errorf("choose how often it runs: %w", err)
	}
	if err := offered(rule); err != nil {
		return nil, err
	}
	loc, err := zoneNamed(cfg.TZ)
	if err != nil {
		return nil, err
	}
	at, err := time.Parse("15:04", strings.TrimSpace(cfg.At))
	if err != nil {
		return nil, fmt.Errorf("choose the time it runs, like 09:00")
	}
	cfg.At = at.Format("15:04")
	s := &schedule{cfg: cfg, rule: rule, loc: loc, atMinute: at.Hour()*60 + at.Minute()}
	if strings.TrimSpace(cfg.From) == "" {
		y, m, d := now.In(loc).Date()
		s.from = time.Date(y, m, d, 0, 0, 0, 0, loc)
	} else {
		f, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(cfg.From), loc)
		if err != nil {
			return nil, fmt.Errorf("choose the day it starts from")
		}
		s.from = f
	}
	s.cfg.From = s.from.Format("2006-01-02")
	return s, nil
}

// offered says whether a repeat rule is one the builder offers: every N days;
// every N weeks on chosen days; every N months on a day of the month (the
// last with -1) or on one weekday of it ("the first Monday"). Open-ended: a
// schedule stops when its rule is turned off.
func offered(r recurrenceBusiness.Rule) error {
	if r.Count != 0 || r.Until != nil || len(r.ByMonth) > 0 || len(r.BySetPos) > 0 {
		return errScheduleRepeat
	}
	switch r.Freq {
	case recurrenceBusiness.Daily:
		if len(r.ByDay) > 0 || len(r.ByMonthDay) > 0 || r.Interval > 30 {
			return errScheduleRepeat
		}
	case recurrenceBusiness.Weekly:
		if len(r.ByDay) == 0 || len(r.ByMonthDay) > 0 || r.Interval > 12 {
			return errScheduleRepeat
		}
		for _, d := range r.ByDay {
			if d.N != 0 {
				return errScheduleRepeat
			}
		}
	case recurrenceBusiness.Monthly:
		if r.Interval > 12 || len(r.ByMonthDay)+len(r.ByDay) != 1 {
			return errScheduleRepeat
		}
		if len(r.ByMonthDay) == 1 && r.ByMonthDay[0] < -1 {
			return errScheduleRepeat
		}
		if len(r.ByDay) == 1 && (r.ByDay[0].N == 0 || r.ByDay[0].N < -1 || r.ByDay[0].N > 4) {
			return errScheduleRepeat
		}
	default:
		return errScheduleRepeat
	}
	return nil
}

// next is the schedule's first time strictly after after, and whether there
// is one.
//
// The recurrence engine counts every instance from a series' start, and
// takes the start itself as the first one whether or not the rule names it.
// So the series starts eight periods before the first day: in the past, so
// never a run, and a whole number of periods back, so "every other week"
// keeps the weeks it was saved with.
//
// "Monthly on the 29th, 30th or 31st" runs on a shorter month's last day, as
// a task's repeat does. The engine does that for a series repeating on its
// start's day (Options.Clamp), not for a day the rule names (where the RFC
// skips the month). So such a schedule starts on that day of a month that has
// it, and repeats on its start's day.
func (s *schedule) next(after time.Time) (time.Time, bool) {
	rule := s.rule
	start := s.from
	n := 8 * rule.Interval
	switch rule.Freq {
	case recurrenceBusiness.Daily:
		start = start.AddDate(0, 0, -n)
	case recurrenceBusiness.Weekly:
		start = start.AddDate(0, 0, -7*n)
	case recurrenceBusiness.Monthly:
		y, m := start.Year(), start.Month()-time.Month(n)
		day := 1
		if len(rule.ByMonthDay) == 1 && rule.ByMonthDay[0] >= 29 {
			day = rule.ByMonthDay[0]
			for i := 0; i < 12 && daysIn(y, m) < day; i++ {
				m -= time.Month(rule.Interval)
			}
			if daysIn(y, m) >= day {
				rule.ByMonthDay = nil
			} else {
				day = 1
			}
		}
		start = time.Date(y, m, day, 0, 0, 0, 0, s.loc)
	}
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, s.atMinute, 0, 0, s.loc)
	from := time.Date(s.from.Year(), s.from.Month(), s.from.Day(), 0, 0, 0, 0, s.loc)
	if after.Before(from) {
		after = from.Add(-time.Nanosecond)
	}
	return recurrenceBusiness.Rules.Next(rule, start, s.loc, after, recurrenceBusiness.Options{Clamp: true})
}

// daysIn is how many days month m of year y has (m may be out of 1-12).
func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// planNext plans a schedule rule's next run after a time, or cancels its job
// when it has none.
func planNext(ctx context.Context, wf *workflowModel.Workflow, after time.Time) error {
	s, err := scheduleOf(wf)
	if err != nil {
		return err
	}
	at, ok := s.next(after)
	if !ok {
		return cancelScheduleJob(ctx, wf.Id)
	}
	return planScheduleJob(ctx, wf.Id, wf.CreatedBy, at)
}

// NextRun is when a schedule rule that is on runs next, in its zone; nil for
// any other rule. What the Rules list shows under a schedule.
func NextRun(wf *workflowModel.Workflow) *time.Time {
	if wf == nil || !wf.IsActive || wf.TriggerType != workflowModel.TriggerSchedule {
		return nil
	}
	s, err := scheduleOf(wf)
	if err != nil {
		return nil
	}
	at, ok := s.next(clock())
	if !ok {
		return nil
	}
	return &at
}

// scheduleOf is a saved rule's schedule.
func scheduleOf(wf *workflowModel.Workflow) (*schedule, error) {
	var cfg scheduleConfig
	if err := json.Unmarshal([]byte(wf.TriggerConfig), &cfg); err != nil {
		return nil, fmt.Errorf("bad schedule: %w", err)
	}
	return checkSchedule(cfg, clock())
}

// syncSchedule makes a rule's waiting job match the rule as saved: planned
// when it is a live schedule that is on, cancelled otherwise. Rules of other
// kinds have none, so a rule changed from a schedule loses its job.
func syncSchedule(ctx context.Context, id uuid.UUID) error {
	wf, err := getRule(ctx, id)
	if err != nil {
		return err
	}
	if wf == nil || wf.DeletedAt != nil || !wf.IsActive || wf.TriggerType != workflowModel.TriggerSchedule {
		return cancelScheduleJob(ctx, id)
	}
	return planNext(ctx, wf, clock())
}

// runScheduleJob is the queue's handler for a schedule rule's job: run the
// rule for the time it was due, then plan the next. It answers nil whatever
// the run did, so the queue never retries a run (the run's own record says
// how it went) and a failing rule keeps its schedule.
func runScheduleJob(ctx context.Context, job *jobModel.ScheduledJob) error {
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(ctx, "Workflow schedule job %s panic: %v", job.Id, r)
		}
	}()
	var p workflowModel.SchedulePayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		return &schedulerBusiness.Terminal{Reason: "bad schedule payload"}
	}
	id, err := uuid.Parse(p.WorkflowID)
	if err != nil {
		return &schedulerBusiness.Terminal{Reason: "bad schedule payload"}
	}
	wf, err := getRule(ctx, id)
	if err != nil || wf == nil || wf.DeletedAt != nil || !wf.IsActive || wf.TriggerType != workflowModel.TriggerSchedule {
		return nil // gone, off or changed: its job ends here
	}
	cw, err := compile(wf)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Workflow schedule %s compile: %v", id, err)
		return nil
	}
	now := clock()
	due := job.RunAt
	ev := triggerEvent{projectID: cw.projectID, eventKey: scheduleKey(due), text: cw.name}
	if late := now.Sub(due); late > lateLimit {
		cw.recordSkip(ctx, ev, now, fmt.Sprintf("Skipped: it was due at %s and ran %s late", due.In(cw.loc).Format("15:04 on 2 Jan"), late.Round(time.Minute)))
	} else if cw.schedule != nil && cw.schedule.cfg.EachTask {
		cw.runEachTask(ctx, ev, now)
	} else {
		runWorkflow(ctx, cw, ev)
	}
	if err := planNext(ctx, wf, now); err != nil {
		helpers.LogErrorWithContext(ctx, "Workflow schedule %s plan next: %v", id, err)
	}
	return nil
}

// scheduleKey is a scheduled run's event key: the time it was due.
func scheduleKey(due time.Time) string {
	return workflowModel.TriggerSchedule + ":" + due.UTC().Format(time.RFC3339)
}

// replanSchedules plans the live schedule rules that have no waiting job:
// one whose job ran but whose next couldn't be planned, or whose job failed.
func replanSchedules(ctx context.Context) error {
	rules, err := unplanned(ctx, replanBatch)
	if err != nil {
		return err
	}
	now := clock()
	var plans []workflowModel.SchedulePlan
	for _, wf := range rules {
		s, err := scheduleOf(wf)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "Workflow schedule %s replan: %v", wf.Id, err)
			continue
		}
		if at, ok := s.next(now); ok {
			plans = append(plans, workflowModel.SchedulePlan{WorkflowID: wf.Id, Owner: wf.CreatedBy, RunAt: at})
		}
	}
	// One statement for all of them.
	return planScheduleJobs(ctx, plans)
}

func init() {
	schedulerBusiness.RegisterJobHandler(workflowModel.ScheduleJobType, runScheduleJob)
	// Claimed, so one instance runs each pass. Never renamed.
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "workflows.schedules.replan", Every: time.Hour, Run: replanSchedules})
}
