// Package taskrank orders the cards in a kanban column.
//
// Columns used to come back newest first and nothing else, so a card dropped
// between two others kept its place only until the next refetch, then jumped
// back to where its creation date put it. Reordering inside a column was never
// saved at all. People read that as the board rearranging their cards.
//
// A task now has an optional task_rank; a column is sorted by it, ascending. A
// task that was never moved has no rank and sorts by the negative of its
// creation time in milliseconds, which is exactly the newest-first order every
// board already had, so existing boards look the same and no backfill is needed.
// A new task has no rank either, and so still arrives at the top.
//
// A drop names the cards now above and below it; the moved card takes the
// midpoint of their ranks. When two neighbours are too close for a midpoint
// (repeated drops into one gap, or tasks created in the same millisecond) the
// column is renumbered.
package taskrank

import (
	"sort"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Step is the gap left between cards at the ends of a column and when a column
// is renumbered: room for about forty drops into one gap before renumbering.
const Step = 1024.0

// Effective is the rank a task sorts by.
func Effective(t *dgraphStruct.DgraphTask) float64 {
	if t == nil {
		return 0
	}
	if t.Rank != nil {
		return *t.Rank
	}
	if t.CreatedAt != nil {
		return -float64(t.CreatedAt.UnixMilli())
	}
	return 0
}

// Sort orders a column in place: by rank, then newest first, then by uuid, so
// the order is the same however the tasks came in. A board pages a column by
// this order (Window), and a page must be the start of the next, bigger one.
func Sort(tasks []*dgraphStruct.DgraphTask) {
	sort.SliceStable(tasks, func(i, j int) bool { return less(tasks[i], tasks[j]) })
}

func less(a, b *dgraphStruct.DgraphTask) bool {
	if ra, rb := Effective(a), Effective(b); ra != rb {
		return ra < rb
	}
	if ca, cb := createdMilli(a), createdMilli(b); ca != cb {
		return ca > cb
	}
	return a.Uuid < b.Uuid
}

func createdMilli(t *dgraphStruct.DgraphTask) int64 {
	if t == nil || t.CreatedAt == nil {
		return 0
	}
	return t.CreatedAt.UnixMilli()
}

// Window sorts a board column and keeps, of each part of it, the first n: a
// part is what partOf names, one of the columns the board draws for this one
// (its own status, or a project's custom status that counts as it), so a
// part's first n come whole whatever the other parts hold. n of 0 or less
// keeps every task.
func Window(column []*dgraphStruct.DgraphTask, n int, partOf func(*dgraphStruct.DgraphTask) string) []*dgraphStruct.DgraphTask {
	Sort(column)
	if n <= 0 {
		return column
	}
	kept := make([]*dgraphStruct.DgraphTask, 0, min(len(column), n))
	taken := map[string]int{}
	for _, t := range column {
		if part := partOf(t); taken[part] < n {
			taken[part]++
			kept = append(kept, t)
		}
	}
	return kept
}

// BetweenMany is n ranks, in order, for tasks dropped together between before
// (the task above them) and after (the task below them); either may be nil at
// an end. They are evenly spaced in the gap, or a Step apart going outward at
// an end. ok is false when the gap has no room for them all, and the column
// needs renumbering (RenumberMany).
func BetweenMany(before, after *dgraphStruct.DgraphTask, n int) (ranks []float64, ok bool) {
	ranks = make([]float64, n)
	switch {
	case before == nil && after == nil:
		for i := range ranks {
			ranks[i] = float64(i) * Step
		}
		return ranks, true
	case before == nil:
		for i := range ranks {
			ranks[i] = Effective(after) - float64(n-i)*Step
		}
		return ranks, true
	case after == nil:
		for i := range ranks {
			ranks[i] = Effective(before) + float64(i+1)*Step
		}
		return ranks, true
	}
	lo, hi := Effective(before), Effective(after)
	prev := lo
	for i := range ranks {
		r := lo + (hi-lo)*float64(i+1)/float64(n+1)
		// Also refuses a drop whose neighbours are out of order, which only
		// happens when the caller's view is stale: renumbering settles it.
		if !(prev < r && r < hi) {
			return nil, false
		}
		ranks[i], prev = r, r
	}
	return ranks, true
}

// RenumberMany places moved (one task, or a selection in its order) in column
// right below before, or else right above after, or else at the top, and
// returns every task in the column with a fresh, evenly spaced rank. A
// neighbour from outside the column (My Tasks mixes projects) places them by
// its rank instead. The first task that wasn't moved keeps its rank, so the
// column stays put relative to tasks that were not renumbered.
func RenumberMany(column, moved []*dgraphStruct.DgraphTask, before, after *dgraphStruct.DgraphTask) []*dgraphStruct.DgraphTask {
	isMoved := map[string]bool{}
	for _, m := range moved {
		isMoved[m.Uuid] = true
	}
	ordered := make([]*dgraphStruct.DgraphTask, 0, len(column)+len(moved))
	for _, t := range column {
		if t != nil && !isMoved[t.Uuid] {
			ordered = append(ordered, t)
		}
	}
	Sort(ordered)

	at := insertAt(ordered, before, after)
	// The first task that wasn't moved keeps its rank, so the column stays put
	// relative to tasks that aren't renumbered.
	start := 0.0
	if len(ordered) > 0 {
		start = Effective(ordered[0])
	}
	ordered = append(ordered[:at], append(append([]*dgraphStruct.DgraphTask{}, moved...), ordered[at:]...)...)
	for i, t := range ordered {
		r := start + float64(i)*Step
		t.Rank = &r
	}
	return ordered
}

func insertAt(ordered []*dgraphStruct.DgraphTask, before, after *dgraphStruct.DgraphTask) int {
	for i, t := range ordered {
		if before != nil && t.Uuid == before.Uuid {
			return i + 1
		}
	}
	for i, t := range ordered {
		if after != nil && t.Uuid == after.Uuid {
			return i
		}
	}
	switch {
	case before != nil:
		at := 0
		for i, t := range ordered {
			if Effective(t) <= Effective(before) {
				at = i + 1
			}
		}
		return at
	case after != nil:
		for i, t := range ordered {
			if Effective(t) >= Effective(after) {
				return i
			}
		}
		return len(ordered)
	}
	return 0
}
