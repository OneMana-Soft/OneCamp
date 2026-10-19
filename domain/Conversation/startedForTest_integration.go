//go:build integration

package domain

import "time"

// StartedForTest makes this process count as started at at, as a server
// started after a restore does, until restore is called; either way the next
// decision reads the index's state again. It exists only in the integration
// build: the tag keeps it out of the shipped binary.
//
// A process trusts no comparison of the conversations index that began before
// it started (source.go). The integration tests run in one process, started
// before any comparison they make, so a test about a restore can only start
// the server again through here.
func StartedForTest(at time.Time) (restore func()) {
	old := started.Load()
	set := func(unix int64) {
		started.Store(unix)
		forgetSource()
		askedMu.Lock()
		askedFor = ""
		askedMu.Unlock()
	}
	set(at.Unix())
	return func() { set(old) }
}
