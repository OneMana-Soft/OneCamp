package Domain

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// dependencyFields is what the task panel shows of a task on the other end
// of a dependency, with standing (how the reader stands to its project, as
// the task read counts it): an end in another project they may not see is
// dropped (DG10, workAccess.KeepSeenEnds).
func dependencyFields(standing string) string {
	return `task_uuid task_name task_status task_custom_status task_custom_status_name task_start_date task_due_date
	task_project { uid project_uuid project_name ` + standing + ` }`
}

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
	// UpstreamProjects are the graph uids of the projects of the tasks in
	// Upstream (DG10): a change stamps them all, so two links in different
	// projects that would close a loop together still conflict.
	UpstreamProjects map[string]bool
	// How the task already waits on the blocker, when Linked: the edge's
	// facets (no kind is finish to start).
	Kind string
	Lag  int
}

type upstreamNode struct {
	UUID    string         `json:"task_uuid"`
	Project uidRef         `json:"task_project"`
	Up      []upstreamNode `json:"task_blocked_by"`
}

// uidRef is a node's uid, read from an object or a list of one.
type uidRef string

func (u *uidRef) UnmarshalJSON(b []byte) error {
	var one struct {
		Uid string `json:"uid"`
	}
	if err := json.Unmarshal(b, &one); err == nil {
		*u = uidRef(one.Uid)
		return nil
	}
	var many []struct {
		Uid string `json:"uid"`
	}
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	if len(many) > 0 {
		*u = uidRef(many[0].Uid)
	}
	return nil
}

func (n upstreamNode) collect(into, projects map[string]bool) {
	if n.UUID != "" {
		into[n.UUID] = true
	}
	if n.Project != "" {
		projects[string(n.Project)] = true
	}
	for _, u := range n.Up {
		u.collect(into, projects)
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
		// Every project the link touches: both ends', and, when adding, those
		// of every task upstream of the blocker.
		now := time.Now().UTC()
		touched := map[string]bool{pair.Task.Project.Uid: true}
		if pair.Blocker != nil && pair.Blocker.Project != nil && pair.Blocker.Project.Uid != "" {
			touched[pair.Blocker.Project.Uid] = true
		}
		for uid := range pair.UpstreamProjects {
			touched[uid] = true
		}
		var stamps []map[string]any
		for uid := range touched {
			if strings.HasPrefix(uid, "0x") {
				stamps = append(stamps, map[string]any{"uid": uid, "project_dependencies_at": now})
			}
		}
		stamp, err := json.Marshal(stamps)
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
		up = `up(func: eq(task_uuid, $b)) @recurse(loop: false) { uid task_uuid task_blocked_by task_project }`
	}
	query := `query q($t: string, $b: string, $u: string) {
		t(func: eq(task_uuid, $t)) {
			` + fields + `
			task_assignee { uid }
			task_created_by { uid }
			task_project {
				uid
				project_uuid
				project_is_member: count(project_members @filter(uid($u)))
				project_is_admin: count(project_admins @filter(uid($u)))
			}
			link: task_blocked_by @facets(kind, lag) @filter(eq(task_uuid, $b)) { task_uuid }
		}
		b(func: eq(task_uuid, $b)) {
			` + fields + `
			task_assignee { uid }
			task_created_by { uid }
			task_project {
				uid
				project_uuid
				project_is_member: count(project_members @filter(uid($u)))
				project_is_admin: count(project_admins @filter(uid($u)))
			}
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
	// A task the person may not see isn't there to link, from either end
	// (models/taskread); upstream ids only find a loop, and show nothing.
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	pair := &DependencyPair{Upstream: map[string]bool{}, UpstreamProjects: map[string]bool{}}
	if len(out.T) > 0 && !tr.Hides(out.T[0].Uuid) {
		pair.Task = &out.T[0].DgraphTask
		if link := out.T[0].Link; len(link) > 0 {
			pair.Linked, pair.Kind, pair.Lag = true, link[0].Kind, link[0].Lag
		}
		tr.Keep(pair.Task)
	}
	if len(out.B) > 0 && !tr.Hides(out.B[0].Uuid) {
		pair.Blocker = out.B[0]
		tr.Keep(pair.Blocker)
	}
	for _, n := range out.Up {
		n.collect(pair.Upstream, pair.UpstreamProjects)
	}
	return pair, nil
}

// DownstreamSchedule is the moved task and every live top-level task that
// waits on it, however indirectly and in any project, with their dates and
// the tasks each waits on: what moving the tasks waiting on it along reads.
// Each carries how the reader of ctx (taskread.ReaderOf) stands to it and its
// project, so Work Access can say which they may move (DG10). Nothing else is
// read, so a task nothing waits on costs one small query.
func DownstreamSchedule(ctx context.Context, movedUUID string) ([]*dgraphStruct.DgraphTask, error) {
	const fields = planFields
	query := `query q($m: string, $u: string) {
		var(func: eq(task_uuid, $m)) @recurse(loop: false) { d as ~task_blocked_by }
		moved(func: eq(task_uuid, $m)) { ` + fields + ` }
		down(func: uid(d)) @filter(` + dgraphStruct.TASK_LIVE_FILTER + ` AND NOT has(task_parent_task)) {
			` + fields + `
			task_blocked_by @facets(kind, lag) @filter(` + dgraphStruct.TASK_LIVE_FILTER + `) { task_uuid }
		}
	}`
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, query, map[string]string{"$m": movedUUID, "$u": taskread.ReaderOf(ctx)})
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
	// Moving tasks along changes only those the person may see
	// (models/taskread).
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	return tr.KeepTasks(append(out.Moved, out.Down...)), nil
}
