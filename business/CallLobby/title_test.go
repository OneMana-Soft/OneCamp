package business

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// A conversation's call, opened by its invite (HU13), is named for whoever
// invited them, never for the conversation: its name isn't the guest's to see.
func TestACallsGuestIsToldWhoseCallItIsAndNotWhere(t *testing.T) {
	old := ownerName
	t.Cleanup(func() { ownerName = old })
	name := "Maya Chen"
	ownerName = func(context.Context, uuid.UUID) string { return name }
	channel := "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"
	dm := uuid.NewString() + " " + uuid.NewString()
	for _, room := range []string{channel, dm} {
		if got := Title(context.Background(), room, uuid.New()); got != "Maya Chen's call" {
			t.Errorf("%q: %q", room, got)
		}
	}
	name = ""
	if got := Title(context.Background(), channel, uuid.New()); got != "The call" {
		t.Errorf("with no name to read: %q", got)
	}
}
