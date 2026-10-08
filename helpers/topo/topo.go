// Package topo orders things that depend on each other, such as a table's
// formulas that read other formulas.
package topo

// Order puts items in an order where each comes after the items it depends
// on (Kahn's algorithm), and otherwise keeps the order given. Items caught in
// a loop, and the items that depend on them, can't be placed: they come back
// in looped, in the order given. Items must be distinct; deps may name things
// that aren't among them, which are ignored.
func Order[T comparable](items []T, deps func(T) []T) (ordered, looped []T) {
	in := make(map[T]bool, len(items))
	for _, it := range items {
		in[it] = true
	}
	waiting := make(map[T]int, len(items))
	dependents := map[T][]T{}
	for _, it := range items {
		seen := map[T]bool{}
		for _, d := range deps(it) {
			if !in[d] || seen[d] {
				continue
			}
			seen[d] = true
			waiting[it]++
			dependents[d] = append(dependents[d], it)
		}
	}
	ordered = make([]T, 0, len(items))
	for _, it := range items {
		if waiting[it] == 0 {
			ordered = append(ordered, it)
		}
	}
	for i := 0; i < len(ordered); i++ {
		for _, it := range dependents[ordered[i]] {
			if waiting[it]--; waiting[it] == 0 {
				ordered = append(ordered, it)
			}
		}
	}
	if len(ordered) == len(items) {
		return ordered, nil
	}
	for _, it := range items {
		if waiting[it] > 0 {
			looped = append(looped, it)
		}
	}
	return ordered, looped
}
