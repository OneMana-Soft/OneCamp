package models

import (
	"context"
	"strings"

	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// SetDgraphTasksIf writes tasks already in the graph (each by its uid) in one
// upsert: q's blocks run first, and the write happens only when cond (an
// @if over q's variables) holds, all of it or none (DG10's atomic move). The
// answer carries q's blocks, so the caller can tell a write the condition
// stopped, and the uids of any named blank nodes. A task without a uid isn't
// written.
func SetDgraphTasksIf(ctx context.Context, q string, vars map[string]string, cond string, tasks []*dgraphStruct.DgraphTask) (*api.Response, error) {
	var nodes []*dgraphStruct.DgraphTask
	for _, t := range tasks {
		if t != nil && strings.HasPrefix(t.Uid, "0x") {
			nodes = append(nodes, forDgraph(t))
		}
	}
	if len(nodes) == 0 {
		return &api.Response{}, nil
	}
	return dgexec.Mutate(ctx, q, vars, dgexec.Mutation{Set: nodes, Cond: cond})
}

// UndoDgraphTaskDates puts tasks' dates back (restore, each by uid) and
// deletes the history lines a move added (lines, by task uid), in one
// mutation: what undoes a move whose Postgres half failed.
func UndoDgraphTaskDates(ctx context.Context, restore []*dgraphStruct.DgraphTask, lines map[string][]string) error {
	var del []map[string]any
	for task, ls := range lines {
		if !strings.HasPrefix(task, "0x") {
			continue
		}
		var edges []map[string]any
		for _, l := range ls {
			if strings.HasPrefix(l, "0x") {
				edges = append(edges, map[string]any{"uid": l})
				del = append(del, map[string]any{"uid": l})
			}
		}
		if len(edges) > 0 {
			del = append(del, map[string]any{"uid": task, "task_activities": edges})
		}
	}
	muts := []dgexec.Mutation{{Set: restore}}
	if len(del) > 0 {
		muts = append(muts, dgexec.Mutation{Delete: del})
	}
	_, err := dgexec.Mutate(ctx, "", nil, muts...)
	return err
}
