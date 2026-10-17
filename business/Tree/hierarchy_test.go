package business

import (
	"errors"
	"slices"
	"testing"

	"github.com/akashc777/OneCamp/business/Tree/graph"
)

// A move into another space changes the page and each page under it that
// isn't in that space already; out of any space, each page under it in one.
// Those are the pages whose full access the move needs.
func TestASpaceMoveChangesThePagesNotThereYet(t *testing.T) {
	under := []graph.Ref{
		{UUID: "s1"},
		{UUID: "s2", SpaceKind: "project", SpaceUUID: "x"},
		{UUID: "s3", SpaceKind: "team", SpaceUUID: "t"},
		{UUID: "s4", SpaceKind: "project", SpaceUUID: "y"},
	}
	for _, c := range []struct {
		name  string
		m     Move
		under []graph.Ref
		want  []string
	}{
		{"into project x", Move{UUID: "p", SpaceKind: "project", SpaceUUID: "x"}, under, []string{"p", "s1", "s3", "s4"}},
		{"out of any space", Move{UUID: "p", SpaceKind: SpaceNone}, under, []string{"p", "s2", "s3", "s4"}},
		{"a page with nothing under it", Move{UUID: "p", SpaceKind: "team", SpaceUUID: "t"}, nil, []string{"p"}},
	} {
		if got, err := spacedBy(c.m, c.under, len(under)); err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %v %v", c.name, got, err)
		}
	}
}

// A page with more under it than one move reads isn't moved into another
// space part of the way: the pages past what was read would change space
// unchecked.
func TestASpaceMoveOfMoreThanOneReadIsRefusedWhole(t *testing.T) {
	under := []graph.Ref{{UUID: "s1"}, {UUID: "s2"}, {UUID: "s3"}}
	if got, err := spacedBy(Move{UUID: "p", SpaceKind: "project", SpaceUUID: "x"}, under, 2); !errors.Is(err, ErrTooMany) || got != nil {
		t.Errorf("3 pages under a page, 2 read at most: %v %v", got, err)
	}
	if _, err := spacedBy(Move{UUID: "p", SpaceKind: "project", SpaceUUID: "x"}, under, 3); err != nil {
		t.Errorf("as many as one move reads: %v", err)
	}
}
