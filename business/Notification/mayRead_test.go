package notification

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// An event about something only some people may read emails only them
// (Event.MayRead): a gate that can't answer emails nobody, and an event with no
// gate (a digest, a booking) emails everyone it names.
func TestAnEmailGoesOnlyToThoseWhoMayReadWhatItIsAbout(t *testing.T) {
	maya, tom, olu := uuid.New(), uuid.New(), uuid.New()
	everyone := []uuid.UUID{maya, tom, olu}
	asked := 0
	readers := func(ids ...uuid.UUID) func(context.Context, []string) (map[string]bool, error) {
		return func(_ context.Context, people []string) (map[string]bool, error) {
			asked++
			out := map[string]bool{}
			for _, p := range people {
				for _, id := range ids {
					if p == id.String() {
						out[p] = true
					}
				}
			}
			return out, nil
		}
	}
	for _, c := range []struct {
		name string
		gate func(context.Context, []string) (map[string]bool, error)
		want []uuid.UUID
	}{
		{"no gate", nil, everyone},
		{"Maya and Olu may read it", readers(maya, olu), []uuid.UUID{maya, olu}},
		{"nobody may", readers(), []uuid.UUID{}},
		{"the gate can't answer", func(context.Context, []string) (map[string]bool, error) { return nil, errors.New("graph down") }, nil},
	} {
		got := keepReaders(context.Background(), Event{Type: EventTaskComment, MayRead: c.gate}, slices.Clone(everyone))
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: emails %v, want %v", c.name, got, c.want)
		}
	}
	asked = 0
	if got := keepReaders(context.Background(), Event{MayRead: readers(maya)}, nil); len(got) != 0 || asked != 0 {
		t.Errorf("nobody to email: kept %v and asked the gate %d times", got, asked)
	}
}
