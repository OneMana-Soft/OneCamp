package provider

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
)

// Stream streams, on the channels an Iter* method returns, what load gives
// back. It stops when ctx is done and turns a panic into an error, as every
// iterator must.
func Stream[S any](ctx context.Context, name string, load func() ([]S, error)) (<-chan S, <-chan error) {
	out := make(chan S, 32)
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
			select {
			case <-ctx.Done():
				return
			case out <- it:
			}
		}
	}()
	return out, errCh
}

// StreamTasks streams the items load gives back that match, each converted
// to a SourceTask. For providers that read their source once into a snapshot.
func StreamTasks[T any](ctx context.Context, name string, load func() ([]T, error), match func(T) bool, convert func(T) SourceTask) (<-chan SourceTask, <-chan error) {
	return Stream(ctx, name, func() ([]SourceTask, error) {
		items, err := load()
		if err != nil {
			return nil, err
		}
		var out []SourceTask
		for _, it := range items {
			if match(it) {
				out = append(out, convert(it))
			}
		}
		return out, nil
	})
}
