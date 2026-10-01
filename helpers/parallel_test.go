package helpers

import (
	"strings"
	"testing"
)

func TestRequireSameLengthAcceptsAlignedInputs(t *testing.T) {
	err := RequireSameLength("bulk insert posts",
		NamedLen{Name: "postUUIDs", Len: 3},
		NamedLen{Name: "channelUUIDs", Len: 3},
	)
	if err != nil {
		t.Errorf("aligned inputs rejected: %v", err)
	}

	// Empty but aligned is fine: callers guard the zero case themselves.
	if err := RequireSameLength("empty",
		NamedLen{Name: "a", Len: 0},
		NamedLen{Name: "b", Len: 0},
	); err != nil {
		t.Errorf("empty aligned inputs rejected: %v", err)
	}
}

// The error has to say which inputs disagreed and by how much, because the whole point is
// diagnosing drift that happened in a caller somewhere else.
func TestRequireSameLengthNamesTheOffenders(t *testing.T) {
	err := RequireSameLength("bulk insert chats",
		NamedLen{Name: "chatUUIDs", Len: 4},
		NamedLen{Name: "chatGrpIDs", Len: 3},
	)
	if err == nil {
		t.Fatal("mismatched lengths accepted — a short slice would panic inside the db helper")
	}
	for _, want := range []string{"bulk insert chats", "chatUUIDs=4", "chatGrpIDs=3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// A longer trailing slice is the silent direction — rows get dropped with no error — so it
// must be caught just as firmly as the short one.
func TestRequireSameLengthCatchesTheSilentDirection(t *testing.T) {
	if err := RequireSameLength("notifications",
		NamedLen{Name: "toUUIDs", Len: 2},
		NamedLen{Name: "grpIDs", Len: 5},
	); err == nil {
		t.Error("a longer parallel slice was accepted; those rows would be dropped silently")
	}
}

func TestRequireSameLengthComparesEveryField(t *testing.T) {
	// The mismatch is in the third field, so a check that only compared the first two
	// would pass this.
	if err := RequireSameLength("four fields",
		NamedLen{Name: "a", Len: 2},
		NamedLen{Name: "b", Len: 2},
		NamedLen{Name: "c", Len: 7},
		NamedLen{Name: "d", Len: 2},
	); err == nil {
		t.Error("a mismatch beyond the first pair was not detected")
	}
}

func TestRequireSameLengthRejectsTooFewFields(t *testing.T) {
	if err := RequireSameLength("one field", NamedLen{Name: "a", Len: 1}); err == nil {
		t.Error("comparing a single field should be reported as a caller mistake")
	}
	if err := RequireSameLength("no fields"); err == nil {
		t.Error("comparing nothing should be reported as a caller mistake")
	}
}
