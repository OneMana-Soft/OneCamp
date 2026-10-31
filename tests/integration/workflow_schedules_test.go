//go:build integration
// +build integration

package integration_test

// Rules that run on time, end to end (package 9 R3; design/projects-vs-asana-
// jira/PROGRAM.md, "R3 plan"), against real Postgres 12 and Dgraph. Saving a
// schedule rule plans one waiting job in the queue every delayed piece of
// work uses (scheduled_jobs), and changing, pausing or deleting it moves or
// cancels that job. When the job comes due the queue runs the rule once, as
// its owner, through the Task Writer: it makes the task and the steps after
// act on it. Then the next run is planned. The same job run again makes
// nothing.
//
// Run: go test -tags=integration ./tests/integration/ -run TestASchedule -v

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
)

// waitingJobs is a rule's waiting jobs' times.
func waitingJobs(t *testing.T, w *workWorld, rule uuid.UUID) []time.Time {
	t.Helper()
	rows, err := w.env.PG.Query(`SELECT run_at FROM scheduled_jobs
		WHERE job_type = 'workflow_schedule' AND status = 'pending' AND deleted_at IS NULL AND payload->>'workflow_id' = $1`, rule.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		out = append(out, at)
	}
	return out
}

func TestAScheduleRulesJobFollowsTheRule(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana := w.account("ana")
	project := uuid.MustParse(w.project)
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skip("no zone data")
	}
	rule, err := workflowBusiness.CreateProjectRule(ctx, &ana, project, workflowBusiness.WorkflowInput{
		Name: "Monday review", IsActive: true, TriggerType: workflowModel.TriggerSchedule,
		TriggerConfig: map[string]interface{}{"repeat": "FREQ=WEEKLY;BYDAY=MO", "at": "09:00", "tz": "Europe/London"},
		Actions:       []workflowBusiness.WorkflowAction{{Type: workflowBusiness.ActionCreateTask, TaskName: "Review the week"}},
	})
	if err != nil {
		t.Fatalf("an admin makes a schedule: %v", err)
	}
	jobs := waitingJobs(t, w, rule.Id)
	if len(jobs) != 1 {
		t.Fatalf("%d waiting jobs, want 1", len(jobs))
	}
	at := jobs[0].In(london)
	if at.Weekday() != time.Monday || at.Hour() != 9 || at.Minute() != 0 || !at.After(time.Now()) {
		t.Errorf("planned for %s, want the next Monday at 09:00 in London", at)
	}

	// Changed to every day at 07:30: the same one job, moved.
	if _, err := workflowBusiness.UpdateProjectRule(ctx, &ana, project, rule.Id, workflowBusiness.WorkflowInput{
		Name: "Morning review", IsActive: true, TriggerType: workflowModel.TriggerSchedule,
		TriggerConfig: map[string]interface{}{"repeat": "FREQ=DAILY", "at": "07:30", "tz": "Europe/London"},
		Actions:       []workflowBusiness.WorkflowAction{{Type: workflowBusiness.ActionCreateTask, TaskName: "Review the day"}},
	}); err != nil {
		t.Fatal(err)
	}
	jobs = waitingJobs(t, w, rule.Id)
	if len(jobs) != 1 || jobs[0].In(london).Hour() != 7 || jobs[0].In(london).Minute() != 30 {
		t.Fatalf("after the change: %v, want one job at 07:30", jobs)
	}

	// Off: no job. On: one again.
	if err := workflowBusiness.SetProjectRuleActive(ctx, &ana, project, rule.Id, false); err != nil {
		t.Fatal(err)
	}
	if jobs = waitingJobs(t, w, rule.Id); len(jobs) != 0 {
		t.Fatalf("a rule turned off still waits: %v", jobs)
	}
	if err := workflowBusiness.SetProjectRuleActive(ctx, &ana, project, rule.Id, true); err != nil {
		t.Fatal(err)
	}
	if jobs = waitingJobs(t, w, rule.Id); len(jobs) != 1 {
		t.Fatalf("turned on, %d jobs wait", len(jobs))
	}

	// Due: the queue runs it once, as ana, and plans the next.
	w.exec(`UPDATE scheduled_jobs SET run_at = NOW() - interval '1 minute'
		WHERE job_type = 'workflow_schedule' AND status = 'pending' AND payload->>'workflow_id' = $1`, rule.Id.String())
	var job *jobModel.ScheduledJob
	{
		rows, err := jobModel.ExecJobs(ctx, `SELECT `+jobModel.SelectColumns+` FROM scheduled_jobs
			WHERE job_type = 'workflow_schedule' AND status = 'pending' AND payload->>'workflow_id' = $1`, []any{rule.Id.String()})
		if err != nil || len(rows) != 1 {
			t.Fatalf("the due job: %v %d", err, len(rows))
		}
		job = rows[0]
	}
	if ran := schedulerBusiness.DrainDueJobs(ctx, "integration"); ran < 1 {
		t.Fatalf("the queue ran %d jobs", ran)
	}
	runs, err := workflowModel.ListRuns(ctx, &project, &rule.Id, nil, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %v: %+v", err, runs)
	}
	run := runs[0]
	if run.Status != workflowModel.RunOK || run.SubjectID == "" || run.Subject != "Review the day" {
		t.Fatalf("the run: %+v", run)
	}
	made, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, run.SubjectID, ana.UserDgraphInfo.Uid)
	if err != nil || made == nil || made.Name != "Review the day" {
		t.Fatalf("the task it made: %+v %v", made, err)
	}
	if jobs = waitingJobs(t, w, rule.Id); len(jobs) != 1 || !jobs[0].After(time.Now()) {
		t.Errorf("after running, waiting %v, want the next one", jobs)
	}

	// The same job again (a retry) makes nothing more.
	schedulerBusiness.RunClaimed(ctx, job)
	if runs, _ = workflowModel.ListRuns(ctx, &project, &rule.Id, nil, 10); len(runs) != 1 {
		t.Errorf("the job run twice made %d runs", len(runs))
	}

	// Deleted: no job.
	if err := workflowBusiness.DeleteProjectRule(ctx, project, rule.Id); err != nil {
		t.Fatal(err)
	}
	if jobs = waitingJobs(t, w, rule.Id); len(jobs) != 0 {
		t.Errorf("a deleted rule still waits: %v", jobs)
	}
}
