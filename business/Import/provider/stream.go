package provider

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
)

// StreamTasks streams, on the channels an Iter* method returns, the items
// load gives back that match, each converted to a SourceTask. It stops when
// ctx is done and turns a panic into an error, as every iterator must. For
// providers that read their source once into a snapshot.
func StreamTasks[T any](ctx context.Context, name string, load func() ([]T, error), match func(T) bool, convert func(T) SourceTask) (<-chan SourceTask, <-chan error) {
	out := make(chan SourceTask, 32)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		defer helpers.RecoverToErr(name, errCh)
		items, err := load()
		if err != nil {
			errCh <- err
			return
		}
		for _, it := range items {
			if !match(it) {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- convert(it):
			}
		}
	}()
	return out, errCh
}
