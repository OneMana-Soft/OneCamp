package helpers

// Deref is what p points at, or T's zero value for nil. Pure.
func Deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
