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

// Sort orders a column in place. Ties keep the order they came in, which is
// newest first from the queries that feed it.
func Sort(tasks []*dgraphStruct.DgraphTask) {
	sort.SliceStable(tasks, func(i, j int) bool { return Effective(tasks[i]) < Effective(tasks[j]) })
}

// Between is the rank for a card dropped between before (the card above it)
// and after (the card below it); either may be nil at the ends of a column.
// ok is false when there is no room between them and the column needs
// renumbering.
func Between(before, after *dgraphStruct.DgraphTask) (rank float64, ok bool) {
	switch {
	case before == nil && after == nil:
		return 0, true
	case before == nil:
		return Effective(after) - Step, true
	case after == nil:
		return Effective(before) + Step, true
	}
	lo, hi := Effective(before), Effective(after)
	mid := lo + (hi-lo)/2
	// Also refuses a drop whose neighbours are out of order, which only happens
	// when the caller's view of the column is stale: renumbering settles it.
	if !(lo < mid && mid < hi) {
		return 0, false
	}
	return mid, true
}

// Renumber places moved in column right below before, or else right above
// after, or else at the top, and returns every card in the column with a
// fresh, evenly spaced rank. A neighbour from outside the column (My Tasks
// mixes projects) places the card by its rank instead. The first card keeps
// its current rank, so the column stays put relative to cards that were not
// renumbered.
func Renumber(column []*dgraphStruct.DgraphTask, moved, before, after *dgraphStruct.DgraphTask) []*dgraphStruct.DgraphTask {
	ordered := make([]*dgraphStruct.DgraphTask, 0, len(column)+1)
	for _, t := range column {
		if t != nil && t.Uuid != moved.Uuid {
			ordered = append(ordered, t)
		}
	}
	Sort(ordered)

	at := insertAt(ordered, before, after)
	ordered = append(ordered, nil)
	copy(ordered[at+1:], ordered[at:])
	ordered[at] = moved

	start := 0.0
	if len(ordered) > 1 {
		// The card that was first before the move.
		first := ordered[0]
		if at == 0 {
			first = ordered[1]
		}
		start = Effective(first)
	}
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
