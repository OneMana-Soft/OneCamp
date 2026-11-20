//go:build integration

package models

import "sync/atomic"

// CountPageReadsForTest counts every PageById until the restore it returns is
// called: a guest's move reads its page once. Integration build only.
func CountPageReadsForTest() (reads func() int64, restore func()) {
	var n atomic.Int64
	old := pageRead
	pageRead = func() { n.Add(1) }
	return n.Load, func() { pageRead = old }
}
