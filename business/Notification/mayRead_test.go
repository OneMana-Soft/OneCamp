package notification

import (
	"context"
	"errors"
	"slices"
	"testing"

	notifylevel "github.com/akashc777/OneCamp/business/NotifyLevel"
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
		got := keepBy(context.Background(), c.gate, "who may read", EventTaskComment, slices.Clone(everyone))
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: emails %v, want %v", c.name, got, c.want)
		}
	}
	asked = 0
	if got := keepBy(context.Background(), readers(maya), "who may read", EventTaskComment, nil); len(got) != 0 || asked != 0 {
		t.Errorf("nobody to email: kept %v and asked the gate %d times", got, asked)
	}
}

// An event's Hears gate reads everyone's setting for its place once and keeps
// whom the one rule (notifylevel.Lets) lets hear it: a project's update is
// Plain, so only Everything hears it; a task given to them is Named, so
// Mentions hears it too; Nothing hears neither, and nor does someone with no
// setting for the place.
func TestHearsKeepsWhomTheirSettingLets(t *testing.T) {
	ada, ben, cy, eve := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	everyone := []uuid.UUID{ada, ben, cy, eve}
	reads := 0
	levelsOf := func(_ context.Context, ids []string) (map[string]string, error) {
		reads++
		if len(ids) != len(everyone) {
			t.Errorf("read the settings of %d people, want all %d at once", len(ids), len(everyone))
		}
		return map[string]string{ada.String(): "block", ben.String(): "mention", cy.String(): "all"}, nil
	}
	for _, c := range []struct {
		name string
		h    notifylevel.Heard
		want []uuid.UUID
	}{
		// Eve has no setting for the place: she hears, as only a mute removes.
		{"a project's update", notifylevel.Plain, []uuid.UUID{cy, eve}},
		{"a comment in their task's thread", notifylevel.Thread, []uuid.UUID{ben, cy, eve}},
		{"a task given to them", notifylevel.Named, []uuid.UUID{ben, cy, eve}},
	} {
		reads = 0
		got := keepBy(context.Background(), hears(levelsOf, c.h), "whose setting lets them hear", EventProjectUpdate, slices.Clone(everyone))
		if !slices.Equal(got, c.want) || reads != 1 {
			t.Errorf("%s: emails %v after %d reads, want %v after one", c.name, got, reads, c.want)
		}
	}
	down := func(context.Context, []string) (map[string]string, error) { return nil, errors.New("db down") }
	if got := keepBy(context.Background(), hears(down, notifylevel.Named), "whose setting lets them hear", EventTaskAssigned, slices.Clone(everyone)); got != nil {
		t.Errorf("settings unread: emails %v, want nobody", got)
	}
}
