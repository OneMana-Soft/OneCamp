package Domain

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
)

// planFields is what the planner reads of a task: its days, whether it is
// finished and since when, its points, its links with their kind and lag, and
// how the reader (the query's $u, a user uuid) stands to it and its project,
// for Work Access. The forecast's pace counts the done ones (linear-20),
// over no more weeks than the project has had (project_created_at).
const planFields = `uid task_uuid task_name task_status task_status_since task_estimate_points task_start_date task_due_date
	task_assignee { uid user_uuid }
	task_created_by { uid user_uuid }
	task_project {
		uid project_uuid project_name project_deleted_at project_created_at
		project_is_member: count(project_members @filter(eq(user_uuid, $u)))
		project_is_admin: count(project_admins @filter(eq(user_uuid, $u)))
	}`

// planLinked is a live top-level task's filter, for every hop.
const planLinked = dgraphStruct.TASK_LIVE_FILTER + ` AND NOT has(task_parent_task)`

// PlanRead is what a plan read keeps of one project: its tasks, and what its
// limits left out.
type PlanRead struct {
	Tasks []*dgraphStruct.DgraphTask
	Cut   PlanCut
}

// PlanTasksOf is what each project's critical path reads (DG10), for any
// number of projects in one query (linear-20's overview forecasts every
// project it lists), keyed by lower-case project id; a project that isn't
// there reads as no tasks. Per project: its live top-level tasks and every
// live top-level task linked to them, upstream and downstream however far, in
// any project, each with its days, status, links (kind and lag) and its
// project as readerUUID stands to it, of the tasks the reader of ctx may see
// (models/taskread; whether they may see another project's tasks at all is
// Work Access's to decide). It keeps limits.Open of the project's own open
// tasks: the soonest due first, those due the same day in the graph's order,
// then those with no due day, the earliest made first. Dgraph can't order by
// two keys when a task lacks one, so the dated and the undated are two reads,
// each naming its first. When limits.Open is zero it keeps every one, in one
// read with no order, since a sorted block that names no first stops at 1,000
// and says nothing. It keeps every open task of other projects linked to
// them, and limits.Finished finished ones, the latest finished first, so a
// cut leaves out the oldest finished work, which moves nothing. Each read's
// Cut says what it left out. A project's blocks are named by its place in
// the list (planBlocks).
func PlanTasksOf(ctx context.Context, projectUUIDs []string, readerUUID string, limits PlanLimits) (map[string]PlanRead, error) {
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	ids := distinctIDs(projectUUIDs)
	out := make(map[string]PlanRead, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	params := []string{"$u: string"}
	vars := map[string]string{"$u": readerUUID}
	var blocks strings.Builder
	for i, id := range ids {
		n := strconv.Itoa(i)
		params = append(params, "$p"+n+": string")
		vars["$p"+n] = id
		blocks.WriteString(planBlocks(n, limits))
	}
	read, err := dgexec.Read[map[string][]*dgraphStruct.DgraphTask](ctx, tr.Fill(`query q(`+strings.Join(params, ", ")+`) {`+blocks.String()+`
	}`), vars)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/PlanTasksOf err: %+v", err)
		return nil, err
	}
	for i, id := range ids {
		n := strconv.Itoa(i)
		var cut PlanCut
		own, closed := append(append(read["own"+n], read["dated"+n]...), read["undated"+n]...), read["closed"+n]
		if limits.Open > 0 && len(own) > limits.Open {
			own, cut.Open = own[:limits.Open], true
		}
		if len(closed) > limits.Finished {
			closed, cut.Finished = closed[:limits.Finished], true
		}
		out[id] = PlanRead{Tasks: tr.KeepTasks(append(append(own, read["linked"+n]...), closed...)), Cut: cut}
	}
	return out, nil
}

// planBlocks are one project's blocks of a plan read ($p<n> names the project),
// their variables and names ending in n: its own open tasks as limits keeps
// them, the open tasks of other projects linked to them, and its finished
// ones, the latest finished first, limits.Finished and one more, so a cut
// shows.
func planBlocks(n string, limits PlanLimits) string {
	fields := planFields + `
			task_blocked_by @facets(kind, lag) @filter(` + planLinked + `) { task_uuid }`
	own := `
		own` + n + `(func: uid(pt` + n + `)) @filter(` + dgraphStruct.TASK_OPEN_FILTER + ` ` + taskread.Mark + `) {
			` + fields + `
		}`
	if limits.Open > 0 {
		first := strconv.Itoa(limits.Open + 1)
		own = `
		dated` + n + `(func: uid(pt` + n + `), orderasc: task_due_date, first: ` + first + `) @filter(` + dgraphStruct.TASK_OPEN_FILTER + ` AND has(task_due_date) ` + taskread.Mark + `) {
			` + fields + `
		}
		undated` + n + `(func: uid(pt` + n + `), orderasc: task_created_at, first: ` + first + `) @filter(` + dgraphStruct.TASK_OPEN_FILTER + ` AND NOT has(task_due_date) ` + taskread.Mark + `) {
			` + fields + `
		}`
	}
	return `
		var(func: eq(project_uuid, $p` + n + `)) { pt` + n + ` as project_tasks @filter(` + planLinked + `) }
		var(func: uid(pt` + n + `)) @recurse(loop: false) { up` + n + ` as task_blocked_by @filter(` + planLinked + `) }
		var(func: uid(pt` + n + `)) @recurse(loop: false) { down` + n + ` as ~task_blocked_by @filter(` + planLinked + `) }` + own + `
		linked` + n + `(func: uid(up` + n + `, down` + n + `)) @filter(` + dgraphStruct.TASK_OPEN_FILTER + ` AND NOT uid(pt` + n + `) ` + taskread.Mark + `) {
			` + fields + `
		}
		closed` + n + `(func: uid(pt` + n + `, up` + n + `, down` + n + `), orderdesc: task_status_since, first: ` + strconv.Itoa(limits.Finished+1) + `) @filter(` + dgraphStruct.TASK_CLOSED_FILTER + ` ` + taskread.Mark + `) {
			` + fields + `
		}`
}

// distinctIDs is ids written as the graph keeps project ids, lower-case, each
// once, in their order, with blanks left out.
func distinctIDs(ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// PlanLimits are how much of a project a plan read keeps (linear-20).
type PlanLimits struct {
	// Open is how many of the project's own open tasks it keeps, of those the
	// reader may see, the soonest due first (PlanTasksOf has the order). Zero
	// keeps every one, as a move's impact must, to see all the work it pushes.
	Open int
	// Finished is how many finished tasks it keeps, the latest finished first.
	Finished int
}

// PlanCut is what a plan read left out: Open, some of the project's own open
// tasks, the latest due; Finished, its oldest finished work.
type PlanCut struct {
	Open, Finished bool
}

// Tally is how many tasks there are, how many of them have points, and the
// points those add up to.
type Tally struct {
	N, Pointed int
	Points     float64
}

// PlanTotals is what a project's forecast counts of its own work (linear-20):
// its live top-level tasks, every one the reader may see, however many.
type PlanTotals struct {
	// Tasks is how many there are.
	Tasks int
	// Open are those still to do.
	Open Tally
	// Done are those done in each span of ReadPlanTotals' bounds.
	Done []Tally
	// FirstDone is the earliest a task of them got done; nil when none says.
	FirstDone *time.Time
	// CreatedAt is when the project was made; nil when not known.
	CreatedAt *time.Time
}

// ReadPlanTotalsOf is each project's PlanTotals for the reader of ctx (a
// restricted task they may not see counts nowhere: models/taskread), from the
// graph's counts and sums, in one query however many projects there are,
// keyed by lower-case project id; a project that isn't there reads as none of
// anything. bounds run latest first, and Done[i] is what got done from
// bounds[i+1] up to, not including, bounds[i]. A project's blocks are named by
// its place in the list (totalsBlocks), the spans' bounds shared.
func ReadPlanTotalsOf(ctx context.Context, projectUUIDs []string, bounds []time.Time) (map[string]PlanTotals, error) {
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	ids := distinctIDs(projectUUIDs)
	out := make(map[string]PlanTotals, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	spans := len(bounds) - 1
	var params []string
	vars := map[string]string{}
	for i := 0; i < spans; i++ {
		s := strconv.Itoa(i)
		vars["$to"+s], vars["$from"+s] = bounds[i].UTC().Format(time.RFC3339), bounds[i+1].UTC().Format(time.RFC3339)
		params = append(params, "$to"+s+": string", "$from"+s+": string")
	}
	var blocks strings.Builder
	for i, id := range ids {
		p := "p" + strconv.Itoa(i)
		vars["$"+p] = id
		params = append(params, "$"+p+": string")
		blocks.WriteString(totalsBlocks(p, spans))
	}
	raw, err := dgexec.Read[map[string][]map[string]any](ctx, tr.Fill(`query q(`+strings.Join(params, ", ")+`) {`+blocks.String()+`
	}`), vars)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ReadPlanTotalsOf err: %+v", err)
		return nil, err
	}
	for i, id := range ids {
		out[id] = planTotalsOf(raw, spans, "p"+strconv.Itoa(i))
	}
	return out, nil
}

// totalsBlocks are one project's blocks of a totals read ($<p> names the
// project), their variables and names ending in p: its tasks, its open ones
// and their points, what got done in each of spans spans ($from<i> to $to<i>),
// its first done, and when it was made.
func totalsBlocks(p string, spans int) string {
	var ranges, counts, sums strings.Builder
	for i := 0; i < spans; i++ {
		w, s := "w"+strconv.Itoa(i)+p, strconv.Itoa(i)
		ranges.WriteString(`
		var(func: uid(fin` + p + `)) @filter(ge(task_status_since, $from` + s + `) AND lt(task_status_since, $to` + s + `)) { ` + w + ` as uid p` + w + ` as task_estimate_points }`)
		counts.WriteString(`
		done` + s + p + `(func: uid(` + w + `)) { n: count(uid) }
		donePointed` + s + p + `(func: uid(` + w + `)) @filter(has(task_estimate_points)) { n: count(uid) }`)
		sums.WriteString(` done` + s + `: sum(val(p` + w + `))`)
	}
	return `
		var(func: eq(project_uuid, $` + p + `)) { own` + p + ` as project_tasks @filter(` + planLinked + ` ` + taskread.Mark + `) }
		var(func: uid(own` + p + `)) @filter(` + dgraphStruct.TASK_OPEN_FILTER + `) { o` + p + ` as uid op` + p + ` as task_estimate_points }
		var(func: uid(own` + p + `)) @filter(eq(task_status, "` + dgraphStruct.TASK_STATUS_DONE + `")) { fin` + p + ` as uid }` + ranges.String() + `
		project` + p + `(func: eq(project_uuid, $` + p + `)) { project_created_at }
		tasks` + p + `(func: uid(own` + p + `)) { n: count(uid) }
		open` + p + `(func: uid(o` + p + `)) { n: count(uid) }
		openPointed` + p + `(func: uid(o` + p + `)) @filter(has(task_estimate_points)) { n: count(uid) }` + counts.String() + `
		firstDone` + p + `(func: uid(fin` + p + `), orderasc: task_status_since, first: 1) @filter(has(task_status_since)) { task_status_since }
		sums` + p + `() { open: sum(val(op` + p + `))` + sums.String() + ` }`
}

// planTotalsOf is one project's PlanTotals in a totals read's answer, its
// blocks named with p: spans spans of Done. A count or a sum the graph leaves
// out is none. Pure.
func planTotalsOf(raw map[string][]map[string]any, spans int, p string) PlanTotals {
	num := func(block, field string) float64 {
		for _, row := range raw[block+p] {
			if v, ok := row[field].(float64); ok {
				return v
			}
		}
		return 0
	}
	count := func(block string) int { return int(num(block, "n")) }
	when := func(block, field string) *time.Time {
		for _, row := range raw[block+p] {
			if s, ok := row[field].(string); ok {
				if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
					return &t
				}
			}
		}
		return nil
	}
	out := PlanTotals{
		Tasks:     count("tasks"),
		Open:      Tally{N: count("open"), Pointed: count("openPointed"), Points: num("sums", "open")},
		Done:      make([]Tally, spans),
		FirstDone: when("firstDone", "task_status_since"),
		CreatedAt: when("project", "project_created_at"),
	}
	for i := range out.Done {
		n := strconv.Itoa(i)
		out.Done[i] = Tally{N: count("done" + n), Pointed: count("donePointed" + n), Points: num("sums", "done"+n)}
	}
	return out
}
