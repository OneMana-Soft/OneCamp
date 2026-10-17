package business

import (
	"context"
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// A deleted task is gone to a refresh, answered as not found; a read that
// failed is that failure, never "gone"; a live task may be refreshed.
func TestADeletedTaskIsGoneToARefresh(t *testing.T) {
	deleted0 := taskDeleted
	t.Cleanup(func() { taskDeleted = deleted0 })
	down := errors.New("postgres is down")
	for _, c := range []struct {
		name    string
		deleted bool
		readErr error
		want    error
	}{
		{"a live task", false, nil, nil},
		{"a deleted task", true, nil, helpers.ErrNotFound},
		{"a read that failed", false, down, down},
	} {
		t.Run(c.name, func(t *testing.T) {
			taskDeleted = func(context.Context, uuid.UUID) (bool, error) { return c.deleted, c.readErr }
			err := RefreshableTask(context.Background(), uuid.New())
			if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("RefreshableTask: %v, want %v", err, c.want)
			}
			if c.readErr != nil && errors.Is(err, helpers.ErrNotFound) {
				t.Error("a failed read answered as gone")
			}
		})
	}
}
