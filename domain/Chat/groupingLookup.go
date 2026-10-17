package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
)

// What the graph says about conversations' grouping ids, for the search
// repair that moves chat entries left under an id a group no longer has
// (business/Chat.RepairChatGroupings).

// isGroupingID is a conversation's grouping id as the app writes one: a
// group's (helpers.IsGroupID, as GenerateGroupID makes it) or a direct
// conversation's (helpers.DMPeople, as GetGroupingId makes it). The repair
// asked the graph only about ids written as uuids, so every group, whose id
// is 32 hex characters, read as no conversation's.
func isGroupingID(id string) bool {
	_, _, dm := helpers.DMPeople(id)
	return dm || helpers.IsGroupID(id)
}

// quotedList is the ids that are (is: a grouping id, or a message's uuid), as
// a DQL list of strings. Anything else is left out of a query built from it.
func quotedList(ids []string, is func(string) bool) (string, int) {
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		if is(id) {
			quoted = append(quoted, `"`+id+`"`)
		}
	}
	return `[` + strings.Join(quoted, ", ") + `]`, len(quoted)
}

// ExistingGroupingIDs answers which of the ids name a conversation now.
func ExistingGroupingIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	list, n := quotedList(ids, isGroupingID)
	out := make(map[string]bool, n)
	if n == 0 {
		return out, nil
	}
	if dgraphInit.DgraphClient == nil {
		return nil, fmt.Errorf("no graph connection")
	}
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx, `{ dms(func: eq(dm_grouping_id, `+list+`)) { dm_grouping_id } }`)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		DMs []struct {
			ID string `json:"dm_grouping_id"`
		} `json:"dms"`
	}
	if err := json.Unmarshal(resp.Json, &parsed); err != nil {
		return nil, err
	}
	for _, dm := range parsed.DMs {
		out[dm.ID] = true
	}
	return out, nil
}

// CurrentGroupingIDsOfChats answers, for each message, the grouping id of the
// conversation it is in now. A message the graph doesn't have, or that is in
// no conversation, is left out.
func CurrentGroupingIDsOfChats(ctx context.Context, chatUUIDs []string) (map[string]string, error) {
	list, n := quotedList(chatUUIDs, helpers.IsCanonicalUUID)
	out := make(map[string]string, n)
	if n == 0 {
		return out, nil
	}
	if dgraphInit.DgraphClient == nil {
		return nil, fmt.Errorf("no graph connection")
	}
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx, `{ chats(func: eq(chat_uuid, `+list+`)) { chat_uuid chat_dm { dm_grouping_id } } }`)
	if err != nil {
		return nil, err
	}
	// chat_dm is one uid (not a list), so the graph answers it as an object.
	var parsed struct {
		Chats []struct {
			UUID string `json:"chat_uuid"`
			DM   *struct {
				ID string `json:"dm_grouping_id"`
			} `json:"chat_dm"`
		} `json:"chats"`
	}
	if err := json.Unmarshal(resp.Json, &parsed); err != nil {
		return nil, err
	}
	for _, c := range parsed.Chats {
		if c.DM != nil && c.DM.ID != "" {
			out[c.UUID] = c.DM.ID
		}
	}
	return out, nil
}
