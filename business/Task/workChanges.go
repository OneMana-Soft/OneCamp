package business

// The Work Change Log (business/WorkFeed, WCL-1) from this package's side:
// each write that changes a task reports it once, after it is stored
// (logChange), and the log asks this package who may see which change
// (changeSight). Until this package loads, the feed shows nothing.

import (
	"context"
	"strings"
	"sync"

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
// write, after it is stored. by nil takes the request's reader. Inside a
// heldChanges (MoveTask) they wait to be logged with the rest of it, once.
func logChange(ctx context.Context, by *dgraphStruct.DgraphUser, changes ...workFeed.TaskChange) {
	if h, ok := ctx.Value(heldChangesKey{}).(*heldChanges); ok && h.hold(userUUIDOf(by), changes) {
		return
	}
	recordTasks(ctx, userUUIDOf(by), changes...)
}

type heldChangesKey struct{}

// heldChanges is the changes one act makes in several writes (a board drag:
// its status, then its place), logged together in one Record, in the order
// they were made, each by whoever made it: the same entries as one log each.
type heldChanges struct {
	mu      sync.Mutex
	changes []workFeed.TaskChange
	done    bool
}

// holdChanges is ctx holding the changes logged through it until log, which
// logs them once; a change logged after (what the act set going) is logged
// at once, as it always was.
func holdChanges(ctx context.Context) (held context.Context, log func()) {
	h := &heldChanges{}
	return context.WithValue(ctx, heldChangesKey{}, h), func() {
		h.mu.Lock()
		changes := h.changes
		h.changes, h.done = nil, true
		h.mu.Unlock()
		if len(changes) > 0 {
			recordTasks(ctx, "", changes...)
		}
	}
}

// hold keeps changes by by, unless the changes were already logged.
func (h *heldChanges) hold(by string, changes []workFeed.TaskChange) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done {
		return false
	}
	for _, c := range changes {
		if c.By == "" {
			c.By = by
		}
		h.changes = append(h.changes, c)
	}
	return true
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
