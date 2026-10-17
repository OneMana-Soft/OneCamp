package business

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	openSearchChatModels "github.com/akashc777/OneCamp/models/openSearch/Chat"
)

// A group's messages left in search under an id it had before someone was
// added move to the id it has now; ids a conversation still has, and ids
// whose message is in no conversation, are left as they are.
func TestRepairChatGroupingsMovesWhatNoConversationHas(t *testing.T) {
	pages := [][]openSearchChatModels.GroupingSample{
		{{GroupingID: "old-1", ChatUUID: "c1"}, {GroupingID: "live", ChatUUID: "c2"}, {GroupingID: "old-2", ChatUUID: "c3"}},
		{{GroupingID: "old-3", ChatUUID: "c4"}},
	}
	var afters []string
	var moved [][2]string
	restore := seamGroupingRepair(
		func(_ context.Context, after json.RawMessage, _ int) ([]openSearchChatModels.GroupingSample, json.RawMessage, error) {
			afters = append(afters, string(after))
			page := pages[0]
			pages = pages[1:]
			if len(pages) == 0 {
				return page, nil, nil
			}
			return page, json.RawMessage(`{"id":"old-2"}`), nil
		},
		func(_ context.Context, ids []string) (map[string]bool, error) {
			return map[string]bool{"live": true}, nil
		},
		func(_ context.Context, chats []string) (map[string]string, error) {
			// c3's conversation went with it.
			return map[string]string{"c1": "new-1", "c4": "new-3"}, nil
		},
		func(_ context.Context, from, to string) error {
			moved = append(moved, [2]string{from, to})
			return nil
		},
	)
	defer restore()

	rep, err := RepairChatGroupings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := (ChatGroupingRepair{IDs: 4, Stale: 3, Moved: 2, Orphaned: 1}); rep != want {
		t.Errorf("repair: %+v, want %+v", rep, want)
	}
	if want := [][2]string{{"old-1", "new-1"}, {"old-3", "new-3"}}; !reflect.DeepEqual(moved, want) {
		t.Errorf("moved %v, want %v", moved, want)
	}
	if want := []string{"", `{"id":"old-2"}`}; !reflect.DeepEqual(afters, want) {
		t.Errorf("pages asked after %v, want %v", afters, want)
	}
}

// A move that fails ends the pass with its error, so the startup job tries
// again later rather than recording itself done.
func TestRepairChatGroupingsStopsAtAFailedMove(t *testing.T) {
	restore := seamGroupingRepair(
		func(context.Context, json.RawMessage, int) ([]openSearchChatModels.GroupingSample, json.RawMessage, error) {
			return []openSearchChatModels.GroupingSample{{GroupingID: "old", ChatUUID: "c1"}}, nil, nil
		},
		func(context.Context, []string) (map[string]bool, error) { return map[string]bool{}, nil },
		func(context.Context, []string) (map[string]string, error) { return map[string]string{"c1": "new"}, nil },
		func(context.Context, string, string) error { return errors.New("search is busy") },
	)
	defer restore()
	if _, err := RepairChatGroupings(context.Background()); err == nil {
		t.Fatal("a failed move went unsaid")
	}
}

func seamGroupingRepair(
	ids func(context.Context, json.RawMessage, int) ([]openSearchChatModels.GroupingSample, json.RawMessage, error),
	existing func(context.Context, []string) (map[string]bool, error),
	current func(context.Context, []string) (map[string]string, error),
	move func(context.Context, string, string) error,
) func() {
	was := []any{chatGroupingIDs, existingGroupings, currentGroupings, moveGrouping}
	chatGroupingIDs, existingGroupings, currentGroupings, moveGrouping = ids, existing, current, move
	return func() {
		chatGroupingIDs = was[0].(func(context.Context, json.RawMessage, int) ([]openSearchChatModels.GroupingSample, json.RawMessage, error))
		existingGroupings = was[1].(func(context.Context, []string) (map[string]bool, error))
		currentGroupings = was[2].(func(context.Context, []string) (map[string]string, error))
		moveGrouping = was[3].(func(context.Context, string, string) error)
	}
}
