package helpers

import "time"

// CompareTimes orders two times, oldest first or newest first, with a
// missing time after every other in either order, as Dgraph places a node
// that lacks the sort predicate. For sorting in Go a list a sorted Dgraph
// block would cut at 1,000 results.
func CompareTimes(a, b *time.Time, newestFirst bool) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	case newestFirst:
		return b.Compare(*a)
	}
	return a.Compare(*b)
}
