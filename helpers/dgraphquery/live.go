package dgraphquery

// zeroTime is the boundary every "is it deleted" filter compares against: a
// live node's deletion time is 0001-01-01 (or unset), a deleted one's is after
// the epoch. helpers.IsSoftDeleted is the same rule in Go.
const zeroTime = `"1970-01-01T00:00:00Z"`

// Live is the filter that keeps nodes whose deletion predicate (as
// "doc_deleted_at") says they are live: not gt(pred, epoch). A node whose
// deletion time is 0001-01-01, or that has none, is live; never write
// has(pred) or "has a value" for this.
func Live(pred string) string { return "not gt(" + pred + ", " + zeroTime + ")" }

// Deleted is Live's opposite: gt(pred, epoch).
func Deleted(pred string) string { return "gt(" + pred + ", " + zeroTime + ")" }
