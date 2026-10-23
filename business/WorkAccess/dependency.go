package business

// What a person may do with the tasks a dependency joins (beta's dependency
// rule, DG10): see each one, and move it. A move that pushes tasks along
// moves only the tasks the person may change (VerdictOf's Move), so in a
// project where members change only their own tasks, someone else's stays.
// Through an API token, a member at most (OfMany's asToken), like every other
// decision here.

import (
	"context"
	"strings"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Verdict is what one person may do with one task, for its dependencies: see
// it, and move it (change its dates, its links).
type Verdict struct {
	See, Move bool
	Standing  Standing
	Facts     TaskFacts
}

// VerdictOf is the verdict for standing s and facts f, by the task rule. Pure.
func VerdictOf(s Standing, f TaskFacts) Verdict {
	return Verdict{See: s.CanOnTask(NeedView, f), Move: s.CanOnTask(NeedEditTask, f), Standing: s, Facts: f}
}

// JudgeTasks is the person's verdict on each task (by lower-case uuid), read
// for them (by uuid userUUID and graph uid userUID): each task's project must
// carry the person's member and admin counts, as every project read does. One
// read of their role rows for all the projects, one of the restrictions for
// all the tasks.
func JudgeTasks(ctx context.Context, userUUID, userUID string, ts ...*dgraphStruct.DgraphTask) (map[string]Verdict, error) {
	seen := map[string]bool{}
	var projects []*dgraphStruct.DgraphProject
	for _, t := range ts {
		if t == nil || t.Project == nil || t.Project.Uuid == "" {
			continue
		}
		if key := strings.ToLower(t.Project.Uuid); !seen[key] {
			seen[key] = true
			projects = append(projects, t.Project)
		}
	}
	standings, err := OfMany(ctx, userUUID, projects)
	if err != nil {
		return nil, err
	}
	facts, err := FactsForMany(ctx, userUUID, userUID, ts...)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Verdict, len(ts))
	for _, t := range ts {
		if t == nil {
			continue
		}
		var s Standing
		if t.Project != nil && t.Project.Uuid != "" {
			s = standings[strings.ToLower(t.Project.Uuid)]
		}
		key := strings.ToLower(t.Uuid)
		out[key] = VerdictOf(s, facts[key])
	}
	return out, nil
}
