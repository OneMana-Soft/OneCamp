package business

// The Work Change Log (business/WorkFeed, WCL-1) from this package's side:
// each write that changes a task reports it once, after it is stored
// (logChange), and the log asks this package who may see which change
// (changeSight). Until this package loads, the feed shows nothing.

import (
	"context"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
)

func init() {
	workFeed.RegisterSight(changeSight)
}

// Seams for tests: which projects someone may view, the WA2 filter for one
// person, and the log.
var (
	viewableProjects = workAccess.ProjectsViewable
	readerFilter     = taskread.For
	recordTasks      = workFeed.RecordTasks
)

// logChange reports changes to tasks to the Work Change Log: once per
// write, after it is stored. by nil takes the request's reader.
func logChange(ctx context.Context, by *dgraphStruct.DgraphUser, changes ...workFeed.TaskChange) {
	recordTasks(ctx, userUUIDOf(by), changes...)
}

// changeSight is which rows who may see, by number: a task's change when
// they may view its project (Work Access, the check the project's own routes
// make) and the WA2 filter doesn't hide the task from them now. A change
// made while its task was restricted shows only to those the filter lets
// into it now, so a task opened, or gone, since never shows what only a few
// saw. Anything but a task shows to nobody yet. An error is the read's.
func changeSight(ctx context.Context, who accessBusiness.Subject, rows []workFeed.Row) (map[int64]bool, error) {
	out := make(map[int64]bool, len(rows))
	if who.UserUUID == "" || len(rows) == 0 {
		return out, nil
	}
	var projects []string
	asked := map[string]bool{}
	for _, r := range rows {
		if p := strings.ToLower(r.ProjectUUID); r.ResourceKind == "task" && !asked[p] {
			asked[p] = true
			projects = append(projects, p)
		}
	}
	viewable, err := viewableProjects(ctx, who.UserUUID, who.UserUID, projects)
	if err != nil {
		return nil, err
	}
	filter, err := readerFilter(ctx, who.UserUUID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.ResourceKind != "task" || !viewable[strings.ToLower(r.ProjectUUID)] || filter.Hides(r.ResourceUUID) {
			continue
		}
		if r.Restricted && !filter.Restricted(r.ResourceUUID) {
			continue
		}
		out[r.Sequence] = true
	}
	return out, nil
}

func userUUIDOf(u *dgraphStruct.DgraphUser) string {
	if u == nil {
		return ""
	}
	return u.Uuid
}

func projectOf(t *dgraphStruct.DgraphTask) string {
	if t == nil || t.Project == nil {
		return ""
	}
	return t.Project.Uuid
}
