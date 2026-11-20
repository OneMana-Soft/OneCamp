package business

// The SLA views (linear-24 H3): a project's task list and board filtered by
// where their tasks' timers stand, its list sorted by when they breach, and
// Insights sliced by SLA. Each is SLA state, so each reads it as the lay does
// (lay.go): only a project's members do (its viewers to its admins), never a
// guest, even with a member's place, who is refused (a 403) rather than
// shown none; and none of it where the project's SLAs are off, or where its
// switch can't be read (fails closed).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/SLA"
	"github.com/google/uuid"
)

// FilterID is the list's and the board's filter by SLA: its values are
// States, and NoSLA, the tasks whose timers stand nowhere (none, or ended).
const (
	FilterID = "task_sla"
	NoSLA    = "none"
)

// projectTimers is a seam for tests: a project's timers.
var projectTimers = model.TimersIn

// stateOrder is the states in urgency order, the SLA filter's, most urgent
// first, with their names as a slice names them.
var stateOrder = []struct {
	state State
	name  string
}{{Breached, "Breached"}, {AtRisk, "At risk"}, {OnTrack, "On track"}, {Met, "Met"}}

// Name is the state as people read it, "" for None.
func (s State) Name() string {
	for _, st := range stateOrder {
		if st.state == s {
			return st.name
		}
	}
	return ""
}

// Rank is the state's place in urgency order, 1 first: the states' own,
// then any other state, then None (no timer) last.
func (s State) Rank() int {
	for i, st := range stateOrder {
		if st.state == s {
			return i + 1
		}
	}
	if s != None {
		return len(stateOrder) + 1
	}
	return len(stateOrder) + 2
}

// Due is where a task's timer stands now and when it is due.
type Due struct {
	State State
	At    *time.Time
}

// readsIn is which of projects (lower-case uuids) reader reads SLA state in:
// those they are a member of whose SLAs are on. A guest with a member's
// place in any of them is refused (errGuest); a failed read is an error, and
// the caller shows no SLA state.
func readsIn(ctx context.Context, reader string, projects []string) (map[string]bool, error) {
	out := map[string]bool{}
	if _, err := uuid.Parse(reader); err != nil || len(projects) == 0 {
		return out, nil
	}
	asked := make(map[string][]string, len(projects))
	for _, p := range projects {
		asked[p] = []string{reader}
	}
	in, err := membersAmong(ctx, asked)
	if err != nil {
		return nil, fmt.Errorf("the reader's projects: %w", err)
	}
	var mine []string
	for _, p := range projects {
		if in[p][reader] {
			mine = append(mine, p)
		}
	}
	if len(mine) == 0 {
		return out, nil
	}
	if isGuest(ctx, reader) {
		return nil, errGuest
	}
	on, err := onIn(ctx, mine)
	if err != nil {
		return nil, fmt.Errorf("the projects' SLA switches: %w", err)
	}
	for _, p := range mine {
		out[p] = on[p]
	}
	return out, nil
}

// ReadsIn is which of projects (by lower-case uuid) the request's reader
// reads SLA state in (readsIn): what Insights slices by SLA.
func ReadsIn(ctx context.Context, projects []uuid.UUID) (map[string]bool, error) {
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		ids = append(ids, p.String())
	}
	return readsIn(ctx, readerOf(ctx), ids)
}

// StatesIn is where each of the project's tasks' timers stands now for the
// request's reader, by lower-case task uuid (a task whose timer stands
// nowhere is left out), and whether they read the project's SLA state at
// all (readsIn): one read of the project's timers, only when they do.
func StatesIn(ctx context.Context, project uuid.UUID) (map[string]Due, bool, error) {
	reads, err := readsIn(ctx, readerOf(ctx), []string{project.String()})
	if err != nil || !reads[project.String()] {
		return nil, false, err
	}
	timers, err := projectTimers(ctx, project)
	if err != nil {
		return nil, false, fmt.Errorf("the project's timers: %w", err)
	}
	now := clock()
	out := make(map[string]Due, len(timers))
	for _, t := range timers {
		if s := StateAt(t, now); s != None {
			out[t.TaskID.String()] = Due{State: s, At: t.DueAt}
		}
	}
	return out, true, nil
}

// FilterClause narrows a project's task list to the tasks whose timers
// stand in one of the states given, and, with NoSLA among them, those whose
// timers stand nowhere. It matches nothing where the reader reads no SLA
// state (SLAs off; outside the project); a guest is refused, and a failed
// read is an error (the list narrows to nothing).
func FilterClause(ctx context.Context, project uuid.UUID, values []string) (string, error) {
	states, reads, err := StatesIn(ctx, project)
	if err != nil {
		return "", err
	}
	if !reads {
		return taskDomain.NoTask, nil
	}
	tasks, but := picked(states, values)
	if len(tasks) == 0 && !but {
		return taskDomain.NoTask, nil
	}
	return taskDomain.TasksClause(tasks, but), nil
}

// picked is the tasks the filter names, or, with NoSLA among values (but),
// the tasks it leaves out: those whose timers stand somewhere it doesn't
// ask for. A value that is no state names nothing. Pure.
func picked(states map[string]Due, values []string) (tasks []string, but bool) {
	want := map[State]bool{}
	for _, v := range values {
		switch s := State(v); {
		case v == NoSLA:
			but = true
		case s.Name() != "":
			want[s] = true
		}
	}
	for id, d := range states {
		if want[d.State] != but {
			tasks = append(tasks, id)
		}
	}
	sort.Strings(tasks)
	return tasks, but
}

// ByBreach orders tasks by when their timers breach, soonest first (one
// already breached first of all), then those that never will (met, or no
// timer) in the order they came in: the list's own (stable). states is
// StatesIn's. Pure.
func ByBreach(tasks []*dgraphStruct.DgraphTask, states map[string]Due) {
	at := func(t *dgraphStruct.DgraphTask) (time.Time, bool) {
		d, ok := states[strings.ToLower(t.Uuid)]
		if !ok || d.At == nil || d.State == Met {
			return time.Time{}, false
		}
		return *d.At, true
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		a, aok := at(tasks[i])
		b, bok := at(tasks[j])
		if aok != bok {
			return aok
		}
		return aok && a.Before(b)
	})
}
