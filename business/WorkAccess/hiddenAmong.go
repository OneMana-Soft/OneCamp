package business

import (
	"context"
	"strings"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	roleModel "github.com/akashc777/OneCamp/models/postgres/ProjectRole"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// Seams for tests: the decision's three reads, made once for many readers.
var (
	candidatesAmong = restrictionModel.CandidatesAmong
	standingAmong   = taskDomain.GetTasksStandingAmong
	rolesAmong      = roleModel.RolesAmong
)

// HiddenUnassignedAmong is, for each of readers (user uuids; anything else
// is left out), by lower-case reader, hiddenFor as the assign rule judges
// it: the restricted tasks their rows, their place in the project and the
// rule keep them out of, their assignments not counted. It is the one
// decision (leftOut, then letInBy, as hidden makes it with assignments not
// counted), from its three reads made once for them all rather than once
// each: their rows, the tasks any of them is left out of with each one's
// place in those tasks' projects, and their role rows there. An upgrade's
// pass over old assignments judges a page of assignees at a time by it,
// against the rule a task is assigned by (business/AssigneeRepair).
func HiddenUnassignedAmong(ctx context.Context, readers []string) (map[string]map[string]bool, error) {
	var ids []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, r := range readers {
		if id, err := uuid.Parse(strings.TrimSpace(r)); err == nil && id != uuid.Nil && !seen[id] {
			seen[id], ids = true, append(ids, id)
		}
	}
	out := make(map[string]map[string]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := candidatesAmong(ctx, ids)
	if err != nil {
		return nil, err
	}
	type start struct {
		hidden     map[string]bool
		candidates []restrictionModel.Candidate
		read       []string
	}
	starts := make(map[uuid.UUID]start, len(ids))
	var tasks, people []string
	var them []uuid.UUID
	asked := map[string]bool{}
	for _, me := range ids {
		if len(rows[me]) == 0 {
			out[me.String()] = nil
			continue
		}
		hidden, candidates, read := leftOut(me, rows[me])
		starts[me] = start{hidden, candidates, read}
		if len(candidates) == 0 {
			continue
		}
		people, them = append(people, me.String()), append(them, me)
		for _, id := range read {
			if !asked[id] {
				asked[id], tasks = true, append(tasks, id)
			}
		}
	}
	var views map[string][]*dgraphStruct.DgraphTask
	var roles map[uuid.UUID]map[uuid.UUID]string
	if len(tasks) > 0 {
		// As hidden reads them: with no audience, or the read would ask the
		// decision itself.
		if views, err = standingAmong(taskread.WithSystem(ctx), tasks, people); err != nil {
			return nil, err
		}
		var projects []uuid.UUID
		inAny := map[uuid.UUID]bool{}
		for _, view := range views {
			_, withRole := graphStandings(projectsOf(view))
			for _, p := range withRole {
				if !inAny[p] {
					inAny[p], projects = true, append(projects, p)
				}
			}
		}
		if roles, err = rolesAmong(ctx, them, projects); err != nil {
			return nil, err
		}
	}
	token := viaToken(ctx)
	for me, s := range starts {
		if len(s.candidates) > 0 {
			view := only(views[me.String()], s.read)
			standings, _ := graphStandings(projectsOf(view))
			lowerBy(standings, roles[me])
			if token {
				for key, st := range standings {
					standings[key] = st.asToken()
				}
			}
			letInBy(me, s.hidden, s.candidates, view, standings, false)
		}
		out[me.String()] = s.hidden
	}
	return out, nil
}

// only is the tasks among ids, as the one reader's decision reads them.
func only(tasks []*dgraphStruct.DgraphTask, ids []string) []*dgraphStruct.DgraphTask {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[strings.ToLower(id)] = true
	}
	var out []*dgraphStruct.DgraphTask
	for _, t := range tasks {
		if t != nil && want[strings.ToLower(t.Uuid)] {
			out = append(out, t)
		}
	}
	return out
}
