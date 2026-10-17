package business

// Every chat entry in search under the id its conversation has now, once.
//
// A group moves to a new id when someone is added (helpers.GenerateGroupID),
// and its search entries move with it (MigrateChatGroupIDInOpensearch). Until
// this release the move never matched a chat entry, whose chat_grp_id is text
// (openSearchStruct.ChatGroupingIDField), so a group's messages from before
// each add stayed under an id no conversation has: a search hit on one opened
// a conversation that was no longer there, and a search within the group
// missed them. Replies and files moved; their fields are keywords.
//
// This lists the ids chat entries carry, keeps those that name no
// conversation now, asks the graph where one message of each lives, and moves
// that id's entries there. An id whose message is in no conversation (deleted
// with it) is counted and left. It runs in the background at startup until it
// has succeeded once, which system_configs records. Running it again is
// harmless: it moves only what is under an id nothing has.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Chat"
	"github.com/akashc777/OneCamp/helpers"
	openSearchChatModels "github.com/akashc777/OneCamp/models/openSearch/Chat"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

const (
	chatGroupingRepairKey  = "chats_search_grouping_v1"
	chatGroupingRepairPage = 500
	// chatGroupingRepairTries bounds the retries in one process; the next
	// start tries again.
	chatGroupingRepairTries = 10
)

// Search and the graph as the repair reads and writes them. Seams for tests.
var (
	chatGroupingIDs   = openSearchChatModels.ChatGroupingIDs
	existingGroupings = domain.ExistingGroupingIDs
	currentGroupings  = domain.CurrentGroupingIDsOfChats
	moveGrouping      = openSearchChatModels.MigrateChatGroupIDInOpensearch
)

// ChatGroupingRepair is what one pass did.
type ChatGroupingRepair struct {
	IDs      int // grouping ids the chat entries carry
	Stale    int // of which no conversation has now
	Moved    int // stale ids whose entries were moved to their conversation's id
	Orphaned int // stale ids whose message is in no conversation, left as they are
}

func (r ChatGroupingRepair) String() string {
	return fmt.Sprintf("%d grouping ids in chat entries, %d no conversation has: %d moved to their conversation's id, %d in no conversation, left",
		r.IDs, r.Stale, r.Moved, r.Orphaned)
}

// StartChatGroupingRepair runs RunChatGroupingRepair in the background until
// it succeeds, ctx ends, or the tries run out.
func StartChatGroupingRepair(ctx context.Context) {
	helpers.RetryInBackground(ctx, "chat.search-grouping-repair",
		helpers.Retries{First: 30 * time.Second, Cap: 30 * time.Minute, Tries: chatGroupingRepairTries},
		func(ctx context.Context) error {
			rep, ran, err := RunChatGroupingRepair(ctx)
			if err != nil {
				helpers.MessageLogs.ErrorLog.Printf("Chat search grouping repair failed (will retry): %v (so far: %s)", err, rep)
				return err
			}
			if ran {
				helpers.MessageLogs.InfoLog.Printf("Chat search grouping repair: %s", rep)
			}
			return nil
		})
}

// RunChatGroupingRepair repairs the chat entries' grouping ids, unless that
// has been done, and then records that it has, with what it did. ran is
// whether it ran this time.
func RunChatGroupingRepair(ctx context.Context) (rep ChatGroupingRepair, ran bool, err error) {
	if done, derr := configModel.IsDone(chatGroupingRepairKey); derr != nil || done {
		return rep, false, derr
	}
	rep, err = RepairChatGroupings(ctx)
	if err != nil {
		return rep, true, err
	}
	return rep, true, configModel.UpsertConfig(chatGroupingRepairKey, "done: "+rep.String())
}

// RepairChatGroupings moves the chat entries under an id no conversation has
// to the id their conversation has now, a page of ids at a time. Idempotent.
func RepairChatGroupings(ctx context.Context) (rep ChatGroupingRepair, err error) {
	var after json.RawMessage
	for {
		samples, next, err := chatGroupingIDs(ctx, after, chatGroupingRepairPage)
		if err != nil {
			return rep, err
		}
		if err := repairChatGroupingPage(ctx, &rep, samples); err != nil {
			return rep, err
		}
		if next == nil {
			return rep, nil
		}
		after = next
	}
}

func repairChatGroupingPage(ctx context.Context, rep *ChatGroupingRepair, samples []openSearchChatModels.GroupingSample) error {
	rep.IDs += len(samples)
	ids := make([]string, len(samples))
	for i, s := range samples {
		ids[i] = s.GroupingID
	}
	existing, err := existingGroupings(ctx, ids)
	if err != nil {
		return err
	}
	var stale []openSearchChatModels.GroupingSample
	var chats []string
	for _, s := range samples {
		if !existing[s.GroupingID] {
			stale = append(stale, s)
			chats = append(chats, s.ChatUUID)
		}
	}
	rep.Stale += len(stale)
	if len(stale) == 0 {
		return nil
	}
	current, err := currentGroupings(ctx, chats)
	if err != nil {
		return err
	}
	for _, s := range stale {
		to := current[s.ChatUUID]
		if to == "" || to == s.GroupingID {
			rep.Orphaned++
			continue
		}
		if err := moveGrouping(ctx, s.GroupingID, to); err != nil {
			return err
		}
		rep.Moved++
	}
	return nil
}
