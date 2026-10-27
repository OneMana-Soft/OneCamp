package helpers

import (
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestIsCanonicalUUID(t *testing.T) {
	for s, want := range map[string]bool{
		"7d1e5c2a-9b3f-4a6e-8c1d-2f4b6a8e0c3d":          true,
		"7D1E5C2A-9B3F-4A6E-8C1D-2F4B6A8E0C3D":          true,
		"{7d1e5c2a-9b3f-4a6e-8c1d-2f4b6a8e0c3d}":        false,
		"urn:uuid:7d1e5c2a-9b3f-4a6e-8c1d-2f4b6a8e0c3d": false,
		"7d1e5c2a9b3f4a6e8c1d2f4b6a8e0c3d":              false,
		"":                                              false,
		"doc":                                           false,
	} {
		if got := IsCanonicalUUID(s); got != want {
			t.Errorf("%q: %v, want %v", s, got, want)
		}
	}
}

func TestAllCanonicalUUIDs(t *testing.T) {
	id := "7d1e5c2a-9b3f-4a6e-8c1d-2f4b6a8e0c3d"
	if !AllCanonicalUUIDs([]string{id, ""}, nil, []string{"7D1E5C2A-9B3F-4A6E-8C1D-2F4B6A8E0C3D"}) {
		t.Error("lists of uuids (and an empty id, which names no one) are all uuids")
	}
	if AllCanonicalUUIDs([]string{id}, []string{id, "doc"}) {
		t.Error(`"doc" in the second list is not a uuid`)
	}
}

// Ids as text, in their order, each as uuids are written; no ids is an empty
// list, never nil, which a uuid[] bind would send as NULL.
func TestUUIDStringsWritesIdsAsText(t *testing.T) {
	a, b := uuid.MustParse("7D1E5C2A-9B3F-4A6E-8C1D-2F4B6A8E0C3D"), uuid.MustParse("0b2c3d4e-5f60-4718-8293-a4b5c6d7e8f9")
	if got, want := UUIDStrings([]uuid.UUID{a, b}), []string{"7d1e5c2a-9b3f-4a6e-8c1d-2f4b6a8e0c3d", "0b2c3d4e-5f60-4718-8293-a4b5c6d7e8f9"}; !slices.Equal(got, want) {
		t.Errorf("%v; want %v", got, want)
	}
	if got := UUIDStrings(nil); got == nil || len(got) != 0 {
		t.Errorf("no ids: %#v; want an empty list, not nil", got)
	}
}
