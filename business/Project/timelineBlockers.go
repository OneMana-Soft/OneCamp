package business

import (
	"strings"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// keepOwnBlockers leaves in each task's BlockedBy only the tasks among
// tasks: a timeline draws arrows between its own rows, and a blocker in
// another project (DG10) is named only by the critical path's read, which
// asks Work Access whether the reader may see it. Pure.
func keepOwnBlockers(tasks []*dgraphStruct.DgraphTask) {
	own := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		if t != nil {
			own[strings.ToLower(t.Uuid)] = true
		}
	}
	for _, t := range tasks {
		if t == nil || t.BlockedBy == nil {
			continue
		}
		kept := t.BlockedBy[:0]
		for _, b := range t.BlockedBy {
			if b != nil && own[strings.ToLower(b.Uuid)] {
				kept = append(kept, b)
			}
		}
		t.BlockedBy = kept
	}
}
