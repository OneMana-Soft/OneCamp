//go:build integration

package business

// CapPlansForTest makes a plan that sums a project up stop at n of its open
// tasks and n finished ones, so a test can
// cut one with a few tasks rather than 1,001; restore puts planLimit back. It
// exists only in the integration build.
func CapPlansForTest(n int) (restore func()) {
	old := planLimit
	planLimit = n
	return func() { planLimit = old }
}
