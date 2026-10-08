package Domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// dependencyFields is what the task panel shows of a task on the other end
// of a dependency.
const dependencyFields = `task_uuid task_name task_status task_custom_status task_custom_status_name task_start_date task_due_date`

// DependencyPair is what a dependency change has to check: both tasks (a
// task that doesn't exist comes back nil), the task's project and whether the
// person is its admin, whether the task already waits on the blocker and,
// when adding, every task the blocker already waits on, however indirectly:
// if the task is among them, the new edge would close a loop.
type DependencyPair struct {
	Task     *dgraphStruct.DgraphTask
	Blocker  *dgraphStruct.DgraphTask
	Linked   bool
	Upstream map[string]bool
	// How the task already waits on the blocker, when Linked: the edge's
	// facets (no kind is finish to start).
	Kind string
	Lag  int
}

type upstreamNode struct {
	UUID string         `json:"task_uuid"`
	Up   []upstreamNode `json:"task_blocked_by"`
}

func (n upstreamNode) collect(into map[string]bool) {
	if n.UUID != "" {
		into[n.UUID] = true
	}
	for _, u := range n.Up {
		u.collect(into)
	}
}

// ChangeTaskDependency adds "task waits on blocker" (the task_blocked_by
// edge, with kind and lag as its facets), changes how it waits when it's
// there already, or with remove takes it away, once check has passed the pair
// as it stands in the same transaction. Every change also stamps the project
// (project_dependencies_at), so two changes in one project at once conflict:
// Dgraph aborts one, and it runs again and sees the other. Two links made at
// the same moment can't close a loop together. It reports whether anything
// changed: adding a link that's there, or taking off one that isn't, writes
// nothing, and so does adding one the way it already is.
func ChangeTaskDependency(ctx context.Context, taskUUID, blockerUUID, userDgraphUID string, remove bool, kind string, lag int, check func(*DependencyPair) error) (bool, error) {
	changed := false
	err := dgraphInit.InTxn(ctx, func(txn *dgo.Txn) error {
		changed = false
		pair, err := readDependencyPair(ctx, txn, taskUUID, blockerUUID, userDgraphUID, !remove)
		if err != nil {
			return err
		}
		if err := check(pair); err != nil {
			return err
		}
		same := pair.Linked && sameKind(pair.Kind, kind) && pair.Lag == lag
		if (remove && !pair.Linked) || (!remove && same) {
			return nil
		}
		link := map[string]any{"uid": pair.Blocker.Uid}
		if !remove {
			// Setting the edge again replaces its facets.
			link["task_blocked_by|kind"] = kind
			if lag != 0 {
				link["task_blocked_by|lag"] = lag
			}
		}
		edge, err := json.Marshal(map[string]any{"uid": pair.Task.Uid, "task_blocked_by": []map[string]any{link}})
		if err != nil {
			return err
		}
		stamp, err := json.Marshal(map[string]any{"uid": pair.Task.Project.Uid, "project_dependencies_at": time.Now().UTC()})
		if err != nil {
			return err
		}
		change := &api.Mutation{SetJson: edge}
		if remove {
			change = &api.Mutation{DeleteJson: edge}
		}
		if _, err := txn.Do(ctx, &api.Request{Mutations: []*api.Mutation{change, {SetJson: stamp}}}); err != nil {
			helpers.LogErrorWithContext(ctx, "domain/ChangeTaskDependency (remove=%v) err: %+v", remove, err)
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// sameKind compares kinds, an edge with none being finish to start.
func sameKind(a, b string) bool {
	norm := func(k string) string {
		if k == "" {
			return "fs"
		}
		return k
	}
	return norm(a) == norm(b)
}

// readDependencyPair reads a DependencyPair in txn; upstream only when adding.
func readDependencyPair(ctx context.Context, txn *dgo.Txn, taskUUID, blockerUUID, userDgraphUID string, upstream bool) (*DependencyPair, error) {
	const fields = `uid task_uuid task_name task_deleted_at task_parent_task { task_uuid }`
	up := ""
	if upstream {
		// loop: false follows each edge once, so it ends on any graph and
		// reaches every task upstream, however far.
		up = `up(func: eq(task_uuid, $b)) @recurse(loop: false) { task_uuid task_blocked_by }`
	}
	query := `query q($t: string, $b: string, $u: string) {
		t(func: eq(task_uuid, $t)) {
			` + fields + `
			task_project { uid project_uuid project_is_admin: count(project_admins @filter(uid($u))) }
			link: task_blocked_by @facets(kind, lag) @filter(eq(task_uuid, $b)) { task_uuid }
		}
		b(func: eq(task_uuid, $b)) {
			` + fields + `
			task_project { uid project_uuid }
		}
		` + up + `
	}`
	resp, err := txn.QueryWithVars(ctx, query, map[string]string{"$t": taskUUID, "$b": blockerUUID, "$u": userDgraphUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/readDependencyPair err: %+v", err)
		return nil, err
	}
	var out struct {
		T []struct {
			dgraphStruct.DgraphTask
			// Facets of an aliased edge go by the alias.
			Link []struct {
				Kind string `json:"link|kind"`
				Lag  int    `json:"link|lag"`
			} `json:"link"`
		} `json:"t"`
		B  []*dgraphStruct.DgraphTask `json:"b"`
		Up []upstreamNode             `json:"up"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil {
		return nil, err
	}
	pair := &DependencyPair{Upstream: map[string]bool{}}
	if len(out.T) > 0 {
		pair.Task = &out.T[0].DgraphTask
		if link := out.T[0].Link; len(link) > 0 {
			pair.Linked, pair.Kind, pair.Lag = true, link[0].Kind, link[0].Lag
		}
	}
	if len(out.B) > 0 {
		pair.Blocker = out.B[0]
	}
	for _, n := range out.Up {
		n.collect(pair.Upstream)
	}
	return pair, nil
}

// DownstreamSchedule is the moved task and every live top-level task that
// waits on it, however indirectly, with their dates and the tasks each waits
// on: what moving the tasks waiting on it along reads. Nothing else of the
// project is read, so a task nothing waits on costs one small query.
func DownstreamSchedule(ctx context.Context, movedUUID string) ([]*dgraphStruct.DgraphTask, error) {
	const fields = `uid task_uuid task_status task_start_date task_due_date task_assignee { user_uuid } task_project { project_uuid }`
	query := `query q($m: string) {
		var(func: eq(task_uuid, $m)) @recurse(loop: false) { d as ~task_blocked_by }
		moved(func: eq(task_uuid, $m)) { ` + fields + ` }
		down(func: uid(d)) @filter(` + dgraphStruct.TASK_LIVE_FILTER + ` AND NOT has(task_parent_task)) {
			` + fields + `
			task_blocked_by @facets(kind, lag) @filter(` + dgraphStruct.TASK_LIVE_FILTER + `) { task_uuid }
		}
	}`
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, query, map[string]string{"$m": movedUUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DownstreamSchedule err: %+v", err)
		return nil, err
	}
	var out struct {
		Moved []*dgraphStruct.DgraphTask `json:"moved"`
		Down  []*dgraphStruct.DgraphTask `json:"down"`
	}
	if err := json.Unmarshal(resp.Json, &out); err != nil {
		return nil, err
	}
	return append(out.Moved, out.Down...), nil
}
