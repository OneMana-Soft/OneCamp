package models

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// TestACappedKindsRestoreNamesItsBound: a kind whose create holds a most is
// refused a restore that names no bound, before it reads anything (there is
// no database here), so a caller that forgets the bound can't bring a row
// back past its create's most (V-184 head 2).
func TestACappedKindsRestoreNamesItsBound(t *testing.T) {
	for kind, tb := range tables {
		if !tb.capped {
			continue
		}
		if _, err := Restore(context.Background(), kind, uuid.New()); !errors.Is(err, ErrUnbounded) {
			t.Errorf("%s: a restore with no bound answered %v, want ErrUnbounded", kind, err)
		}
		if err := RestoreAs(context.Background(), kind, uuid.New(), nil); !errors.Is(err, ErrUnbounded) {
			t.Errorf("%s: RestoreAs with no bound answered %v, want ErrUnbounded", kind, err)
		}
	}
}

// TestTheCappedKindsArePinned: the kinds whose create holds a most are a
// project's fields, statuses, forms (and a table's own) and links, the
// milestones each project shows, and the workspace's project statuses and
// labels. A new kind, or a new most on one, changes this list and gives its
// restore its bound.
func TestTheCappedKindsArePinned(t *testing.T) {
	var got []string
	for kind, tb := range tables {
		if tb.capped {
			got = append(got, string(kind))
		}
	}
	slices.Sort(got)
	want := []string{"field", "form", "link", "milestone", "project_label", "project_status", "status"}
	if !slices.Equal(got, want) {
		t.Errorf("the capped kinds are %v, want %v", got, want)
	}
}

// TestBoundLocksAreTakenInOneOrder: a restore takes its bounds' locks sorted
// and once each, as every other restore does, so two restores over the same
// projects (a milestone shown in several) never wait on each other in a ring.
func TestBoundLocksAreTakenInOneOrder(t *testing.T) {
	got := lockOrder([]Bound{{Lock: "milestones:b"}, {Lock: "milestones:a"}, {Lock: "milestones:b"}, {Lock: "milestones:c"}})
	if want := []string{"milestones:a", "milestones:b", "milestones:c"}; !slices.Equal(got, want) {
		t.Errorf("lock order %v, want %v", got, want)
	}
}
