package business

// The list's side orders, which the graph can't sort by: Manual (L-1), the
// project's one order (business/TaskRank), shared with the board, as a task
// never moved has no rank; and SLA (linear-24), breaches soonest, as Postgres
// keeps the timers (business/SLA). Each reads the list's places, orders them
// here, and reads the page it shows by uid.

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	slaBusiness "github.com/akashc777/OneCamp/business/SLA"
	taskrank "github.com/akashc777/OneCamp/business/TaskRank"
	domain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// sidePage is the tasks one page shows of places in order (a side order:
// dgraphquery.SideOrdered): Manual's, the project's one order; SLA's, by
// states (business/SLA StatesIn) breaches soonest, then the list's own order
// (ownOrder), which is all of it with no SLA state. Pure.
func sidePage(order string, places []*dgraphStruct.DgraphTask, states map[string]slaBusiness.Due, pageSize, pageIndex int, all bool) []*dgraphStruct.DgraphTask {
	if order == dgraphquery.SLAOrder {
		ownOrder(places)
		slaBusiness.ByBreach(places, states)
	} else {
		taskrank.Sort(places)
	}
	return pageOf(places, pageSize, pageIndex, all)
}

// ownOrder puts places in the list's own order as the graph reads it
// (dgraphquery's default order, made total by task_uuid): newest first,
// equals by uuid, and those with no creation time after them all, newest
// uid first, where Dgraph leaves what has no value to sort by. Pure.
func ownOrder(places []*dgraphStruct.DgraphTask) {
	uid := func(t *dgraphStruct.DgraphTask) uint64 {
		n, _ := strconv.ParseUint(strings.TrimPrefix(t.Uid, "0x"), 16, 64)
		return n
	}
	sort.SliceStable(places, func(i, j int) bool {
		a, b := places[i], places[j]
		switch {
		case (a.CreatedAt == nil) != (b.CreatedAt == nil):
			return b.CreatedAt == nil
		case a.CreatedAt == nil:
			return uid(a) > uid(b)
		case !a.CreatedAt.Equal(*b.CreatedAt):
			return a.CreatedAt.After(*b.CreatedAt)
		}
		return a.Uuid < b.Uuid
	})
}

// pageOf is the page of places a page shows. Pure.
func pageOf(places []*dgraphStruct.DgraphTask, pageSize, pageIndex int, all bool) []*dgraphStruct.DgraphTask {
	from, to := 0, len(places)
	if !all && pageSize > 0 {
		from = min(pageIndex*pageSize, len(places))
		to = min(from+pageSize, len(places))
	}
	return places[from:to]
}

// sidePlaces is the list's places in the side order, for its reader: the
// SLA order reads their SLA state (a guest is refused it: business/SLA
// Refused); where it can't be read, the list's own order (fails closed).
func sidePlaces(ctx context.Context, order, projectUUID, userDgraphUID, filterQuery string) ([]*dgraphStruct.DgraphTask, error) {
	places, err := domain.GetDgraphProjectTaskPlaces(ctx, projectUUID, userDgraphUID, filterQuery)
	if err != nil {
		return nil, err
	}
	var states map[string]slaBusiness.Due
	if order == dgraphquery.SLAOrder {
		project, perr := uuid.Parse(projectUUID)
		if perr == nil {
			states, _, err = slaBusiness.StatesIn(ctx, project)
		}
		if refused := (*slaBusiness.Refused)(nil); errors.As(err, &refused) {
			return nil, refused
		}
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Project the SLA sort of %s reads no SLA state: %+v", projectUUID, err)
			states = nil
		}
	}
	return sidePage(order, places, states, 0, 0, true), nil
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

// sideTaskList is GetDgraphProjectTaskList in a side order.
func sideTaskList(ctx context.Context, order, projectUUID, userDgraphUID, filterQuery string, pageSize, pageIndex int, getAll bool) (*dgraphStruct.DgraphProject, error) {
	places, err := sidePlaces(ctx, order, projectUUID, userDgraphUID, filterQuery)
	if err != nil {
		return nil, err
	}
	project, err := sideRows(ctx, projectUUID, userDgraphUID, filterQuery, pageOf(places, pageSize, pageIndex, getAll), getAll)
	if err != nil || project == nil {
		return project, err
	}
	project.TaskCount = uint64(len(places))
	return project, nil
}

// sideRows reads the list's tasks at page's places, in their order; all
// reads them however many there are.
func sideRows(ctx context.Context, projectUUID, userDgraphUID, filterQuery string, page []*dgraphStruct.DgraphTask, all bool) (*dgraphStruct.DgraphProject, error) {
	uids := make([]string, 0, len(page))
	for _, p := range page {
		uids = append(uids, p.Uid)
	}
	project, err := domain.GetDgraphProjectTaskList(ctx, projectUUID, userDgraphUID, uidFilter(filterQuery, uids), "", len(uids), 0, all || len(uids) == 0)
	if err != nil || project == nil {
		return project, err
	}
	inOrder(project.Tasks, page)
	return project, nil
}
