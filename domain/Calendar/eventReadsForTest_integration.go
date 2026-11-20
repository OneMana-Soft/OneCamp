//go:build integration

package Calendar

// WhileEventReadForTest calls f at every GetDgraphEventInfoByUUID until the
// restore it returns is called: a guest's move reads its meeting once.
// Integration build only.
func WhileEventReadForTest(f func()) (restore func()) {
	old := eventRead
	eventRead = f
	return func() { eventRead = old }
}
