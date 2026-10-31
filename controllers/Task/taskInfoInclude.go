package controller

import (
	"context"
	"net/http"
	"strings"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	business "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	estimateScaleController "github.com/akashc777/OneCamp/controllers/EstimateScale"
	taskFieldController "github.com/akashc777/OneCamp/controllers/TaskField"
	timeEntryController "github.com/akashc777/OneCamp/controllers/TimeEntry"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The task panel's other reads, answered with the task when GET
// /task/info/{task} asks for them by name (?include=cycle,recurrence,…):
// one request where the panel made eight, two levels deep (the BE audit's
// panel fan-out). Each part is exactly what its own endpoint answers, as
// {"data": …}, under the key the app keeps it by:
//
//	cycle          GET /task/cycle/{task}
//	recurrence     GET /task/recurrence/{task}
//	client_review  GET /task/clientReview/{task}
//	time_summary   GET /task/time/{task}
//	sync_status    GET /task/github-sync-status/{task}, for a linked task only
//	project_meta   statuses, fields and estimates: GET /project/{p}/statuses,
//	               /fields, /estimates
//
// The task is already read and visible to the reader, which is all the
// task's own parts ask; the project's parts ask what their endpoints ask of
// the reader's standing. A part the reader may not have is left out, and the
// app reads it on its own; a part that fails comes as {"error": sentence} in
// its place, so the panel shows that part's error and the rest stands. A name it doesn't know is
// ignored, so an older server answers a newer app with what it has.

// includeNames is the parts a request asks for.
func includeNames(r *http.Request) map[string]bool {
	raw := r.URL.Query().Get("include")
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out[name] = true
		}
	}
	return out
}

// panelParts reads the parts asked for, for a task the reader may see.
func panelParts(r *http.Request, names map[string]bool, taskID uuid.UUID, task *dgraphStruct.DgraphTask, user userModels.UserInfo, standing workAccess.Standing) map[string]helpers.Envolope {
	ctx := r.Context()
	out := map[string]helpers.Envolope{}
	put := func(name string, data any, err error) {
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/GetTaskInfo couldn't include %s for task %s err: %+v", name, taskID, err)
		}
		out[name] = partOf(name, data, err)
	}
	if names["cycle"] {
		c, err := cycleModel.CycleOf(taskID)
		put("cycle", c, err)
	}
	if names["recurrence"] {
		rec, err := business.GetTaskRecurrence(taskID)
		put("recurrence", rec, err)
	}
	if names["client_review"] {
		review, err := guestBusiness.LatestReview(taskID)
		put("client_review", review, err)
	}
	if names["time_summary"] {
		t, err := timeEntryController.TaskTime(r, taskID)
		put("time_summary", t, err)
	}
	if names["sync_status"] && (task.GitHubIssueURL != nil || task.GitHubPRURL != nil) {
		status, errMsg, attempts, err := business.GetGitHubSyncStatus(ctx, taskID)
		put("sync_status", map[string]interface{}{"status": status, "error": errMsg, "attempts": attempts}, err)
	}
	if names["project_meta"] && task.Project != nil {
		projectParts(ctx, put, task.Project, user, standing)
	}
	return out
}

// projectParts are the project's statuses, fields and estimate scale, as
// their endpoints answer them, for a reader those endpoints would answer.
func projectParts(ctx context.Context, put func(string, any, error), p *dgraphStruct.DgraphProject, user userModels.UserInfo, standing workAccess.Standing) {
	projectID, err := uuid.Parse(p.Uuid)
	if err != nil {
		return
	}
	// GET /project/{p}/statuses: the project's members, and a workspace admin
	// (who reads them to set GitHub automation for any linked project).
	if standing.Can(workAccess.NeedView) || user.UserPostgresInfo.IsAdmin {
		res, err := taskStatusBusiness.List(ctx, projectID)
		put("statuses", res, err)
	}
	if standing.Can(workAccess.NeedView) {
		fields, err := taskFieldController.Fields(ctx, projectID, standing)
		put("fields", fields, err)
		// GET /project/{p}/estimates: the scale the task's Points row sizes on.
		estimates, err := estimateScaleController.Answer(ctx, projectID, standing)
		put("estimates", estimates, err)
	}
}

// partWords name each part in a failed part's sentence.
var partWords = map[string]string{
	"cycle": "the cycle", "recurrence": "the repeat", "client_review": "the client review",
	"time_summary": "the time", "sync_status": "the GitHub link", "statuses": "the statuses", "fields": "the fields",
	"estimates": "the estimates",
}

// partOf is one part as the include answers it: {"data": …}, or for a part
// that failed, {"error": sentence}. Pure.
func partOf(name string, data any, err error) helpers.Envolope {
	if err == nil {
		return helpers.Envolope{"data": data}
	}
	what := partWords[name]
	if what == "" {
		what = "this part"
	}
	return helpers.Envolope{"error": "Couldn't read " + what + ". Try again."}
}
