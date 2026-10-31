package entityLinkBusiness

// HasReader is whether kind has a reader, for the external guard
// (kinds_guard_test.go).
func HasReader(kind string) bool {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	_, ok := kinds[kind]
	return ok
}
