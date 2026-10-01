package business

// resetRegistryForTest clears the tool registry between tests.
//
// It lives in a _test.go file rather than in spec.go on purpose. It is a test fixture: the
// only callers are tests, and keeping it in production code both shipped it in the binary
// and made it indistinguishable from an unwired production helper. Being in test code says
// exactly what it is, and the compiler enforces that nothing in production can reach it.
//
// A _test.go file in the same package can touch registryMu and registry, so nothing needs
// exporting to make this work.
func resetRegistryForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]*ToolSpec{}
}
