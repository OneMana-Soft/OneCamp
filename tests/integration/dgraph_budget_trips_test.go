//go:build integration

package integration_test

import "fmt"

// tripsAtMost are the lines whose round trips are a ceiling, not an exact
// count: the work graph's reads (DG5) make one graph read, then one reader
// call per kind of thing the capped hops happened to reach. Which kinds a
// capped hop reaches follows the uid order Dgraph gives the fixture's nodes,
// and that order changes from one load of the fixture to the next, so the
// same read made 5, 7 and 9 trips in three runs (DG6, DG7 and DG9's gates,
// 19 Oct). The line's number is the most kinds the read can reach plus its
// graph read: a reader per node (an N+1) is far past it and still fails.
var tripsAtMost = map[string]bool{
	"graph.doc.1":     true,
	"graph.doc.2":     true,
	"graph.person.2":  true,
	"graph.channel.2": true,
	"graph.project.2": true,
	"graph.path.near": true,
	"graph.path.far":  true,
}

// tripsOff says whether a read's round trips break its line, and what the
// line asks for.
func tripsOff(query string, got, line int) (bool, string) {
	if tripsAtMost[query] {
		return got < 1 || got > line, fmt.Sprintf("at most %d", line)
	}
	return got != line, fmt.Sprintf("%d", line)
}
