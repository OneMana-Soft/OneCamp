package business

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// The count as a conversation opened is the read state's; a read that fails
// leaves it out rather than answering none (which would hide the line).
func TestUnreadAsOpenedIsTheReadStatesCountOrNothing(t *testing.T) {
	old := unreadOf
	t.Cleanup(func() { unreadOf = old })
	unreadOf = func(context.Context, uuid.UUID, string) (int, error) { return 3, nil }
	if n := unreadAsOpened(context.Background(), uuid.New(), "g"); n == nil || *n != 3 {
		t.Errorf("the read state's count: %v", n)
	}
	unreadOf = func(context.Context, uuid.UUID, string) (int, error) { return 0, errors.New("down") }
	if n := unreadAsOpened(context.Background(), uuid.New(), "g"); n != nil {
		t.Errorf("a failed read answered %d", *n)
	}
}
