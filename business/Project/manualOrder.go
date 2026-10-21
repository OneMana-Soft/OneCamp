package business

// The list's Manual sort (L-1): the project's one order (business/TaskRank),
// shared with the board. The graph can't sort by it (a task never moved has
// no rank), so the list reads its tasks' places, orders them here, and reads
// the page it shows by uid.

import (
	"context"
	"sort"
	"strings"

	taskrank "github.com/akashc777/OneCamp/business/TaskRank"
	domain "github.com/akashc777/OneCamp/domain/Project"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// manualPage is the tasks one page shows, in the project's order.
func manualPage(places []*dgraphStruct.DgraphTask, pageSize, pageIndex int, all bool) []*dgraphStruct.DgraphTask {
	taskrank.Sort(places)
	from, to := 0, len(places)
	if !all && pageSize > 0 {
		from = min(pageIndex*pageSize, len(places))
		to = min(from+pageSize, len(places))
	}
	return places[from:to]
}

// uidFilter is the list's filter, keeping only uids.
func uidFilter(filter string, uids []string) string {
	only := "uid(0x0)"
	if len(uids) > 0 {
		only = "uid(" + strings.Join(uids, ", ") + ")"
	}
	if strings.TrimSpace(filter) == "" {
		return only
	}
	return filter + " AND " + only
}

// inOrder puts tasks in the order of page, matched by task uuid: the list's
// read carries no uid.
func inOrder(tasks, page []*dgraphStruct.DgraphTask) {
	at := make(map[string]int, len(page))
	for i, p := range page {
		at[strings.ToLower(p.Uuid)] = i
	}
	sort.SliceStable(tasks, func(i, j int) bool { return at[strings.ToLower(tasks[i].Uuid)] < at[strings.ToLower(tasks[j].Uuid)] })
}

// manualTaskList is GetDgraphProjectTaskList in the project's one order.
func manualTaskList(ctx context.Context, projectUUID, userDgraphUID, filterQuery string, pageSize, pageIndex int, getAll bool) (*dgraphStruct.DgraphProject, error) {
	places, err := domain.GetDgraphProjectTaskPlaces(ctx, projectUUID, userDgraphUID, filterQuery)
	if err != nil {
		return nil, err
	}
	page := manualPage(places, pageSize, pageIndex, getAll)
	uids := make([]string, 0, len(page))
	for _, p := range page {
		uids = append(uids, p.Uid)
	}
	project, err := domain.GetDgraphProjectTaskList(ctx, projectUUID, userDgraphUID, uidFilter(filterQuery, uids), "", len(uids), 0, getAll || len(uids) == 0)
	if err != nil || project == nil {
		return project, err
	}
	inOrder(project.Tasks, page)
	project.TaskCount = uint64(len(places))
	return project, nil
}
