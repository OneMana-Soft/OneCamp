package helpers

import (
	"context"
	"maps"
)

// IndexBy maps each item by its key; an item whose key comes again replaces
// the earlier one. For the hand-written "for _, x := range xs { m[x.ID] = x }".
func IndexBy[K comparable, T any](items []T, key func(T) K) map[K]T {
	out := make(map[K]T, len(items))
	for _, it := range items {
		out[key(it)] = it
	}
	return out
}

// LoadMap reads the items for ids in as few calls as chunk allows (each id
// once, in chunks of at most chunk, or all at once when chunk < 1) and maps
// them by key. It is how a loop that read one item per id becomes a read by
// ids: load takes a list (an = ANY($1), an eq(x, [...])). An id load doesn't
// return is simply missing from the map. The first error stops it.
func LoadMap[K comparable, T any](ctx context.Context, ids []K, chunk int, load func(context.Context, []K) ([]T, error), key func(T) K) (map[K]T, error) {
	seen := make(map[K]bool, len(ids))
	uniq := make([]K, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	out := make(map[K]T, len(uniq))
	if len(uniq) == 0 {
		return out, nil
	}
	if chunk < 1 {
		chunk = len(uniq)
	}
	for start := 0; start < len(uniq); start += chunk {
		end := min(start+chunk, len(uniq))
		items, err := load(ctx, uniq[start:end])
		if err != nil {
			return nil, err
		}
		maps.Copy(out, IndexBy(items, key))
	}
	return out, nil
}
