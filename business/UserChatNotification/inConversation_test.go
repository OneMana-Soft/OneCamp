package business

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// withGroupPeople answers a group's people from people (by grouping id), or
// with err, for the test's length.
func withGroupPeople(t *testing.T, people map[string][]*dgraphStruct.DgraphUser, err error) *int {
	t.Helper()
	reads := 0
	prev := groupPeople
	groupPeople = func(_ context.Context, grp string) ([]*dgraphStruct.DgraphUser, error) {
		reads++
		if err != nil {
			return nil, err
		}
		p, ok := people[grp]
		if !ok {
			return nil, conversationDomain.ErrNoConversation
		}
		return p, nil
	}
	t.Cleanup(func() { groupPeople = prev })
	return &reads
}

func TestOnlyWhoIsInAConversationNowIsKept(t *testing.T) {
	ctx := context.Background()
	gone := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	never := time.Time{}
	reads := withGroupPeople(t, map[string][]*dgraphStruct.DgraphUser{
		"grp-launch": {
			{Uuid: "ada"}, {Uuid: "BO"}, {Uuid: "dee", DeletedAt: &never},
			{Uuid: "eve", DeletedAt: &gone}, // deactivated
			nil, {Uuid: ""},
		},
	}, nil)

	t.Run("a group keeps its participants as the graph has them, and nobody who left or was deactivated", func(t *testing.T) {
		got, err := InConversationNow(ctx, "grp-launch", []string{"ada", "bo", "cy", "dee", "eve"})
		if want := []string{"ada", "bo", "dee"}; err != nil || !slices.Equal(got, want) {
			t.Errorf("kept %v (%v), want %v: cy left, eve is deactivated", got, err, want)
		}
	})

	t.Run("a DM keeps the two people its id names, without a read", func(t *testing.T) {
		before := *reads
		dm := helpers.GetGroupingId("a1111111-1111-1111-1111-111111111111", "b2222222-2222-2222-2222-222222222222")
		got, err := InConversationNow(ctx, dm, []string{"B2222222-2222-2222-2222-222222222222", "c3333333-3333-3333-3333-333333333333"})
		if want := []string{"B2222222-2222-2222-2222-222222222222"}; err != nil || !slices.Equal(got, want) {
			t.Errorf("kept %v (%v), want %v", got, err, want)
		}
		if *reads != before {
			t.Error("a DM read the graph")
		}
	})

	t.Run("a group the graph doesn't have keeps nobody", func(t *testing.T) {
		if got, err := InConversationNow(ctx, "grp-gone", []string{"ada"}); err != nil || len(got) != 0 {
			t.Errorf("kept %v (%v), want nobody", got, err)
		}
	})

	t.Run("nobody asked about is no read", func(t *testing.T) {
		before := *reads
		if got, err := InConversationNow(ctx, "grp-launch", nil); err != nil || len(got) != 0 || *reads != before {
			t.Errorf("kept %v (%v) after %d reads", got, err, *reads-before)
		}
	})
}

func TestWhoIsInAGroupThatCantBeReadIsAnError(t *testing.T) {
	down := errors.New("graph down")
	withGroupPeople(t, nil, down)
	got, err := InConversationNow(context.Background(), "grp-launch", []string{"ada"})
	if !errors.Is(err, down) || got != nil {
		t.Errorf("kept %v (%v), want the read's error and nobody", got, err)
	}
	// Who hears tells nobody then: GetEligibleUsersForChatActivity and
	// GetLevelsInConversation pass the error on.
}
