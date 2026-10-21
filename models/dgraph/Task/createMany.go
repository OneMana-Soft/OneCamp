package models

import (
	"context"

	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// CreateDgraphTasks writes new tasks, each a blank node ("_:t0", ...), and
// the other nodes they hang from (their project, a parent's sub-task edge),
// in one mutation; an aborted write is tried again. It answers the uids
// Dgraph gave the blank nodes, by name.
func CreateDgraphTasks(ctx context.Context, tasks []*dgraphStruct.DgraphTask, others ...any) (map[string]string, error) {
	nodes := make([]any, 0, len(tasks)+len(others))
	for _, t := range tasks {
		nodes = append(nodes, forDgraph(t))
	}
	nodes = append(nodes, others...)
	return dgexec.Set(ctx, nodes)
}
