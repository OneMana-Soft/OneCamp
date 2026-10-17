//go:build integration
// +build integration

package integration_test

// A group's messages left in search under an id it had before someone was
// added (the move never matched a chat entry) are moved, once, to the id the
// group has now: a search hit on one opened a conversation that was no longer
// there. Entries under an id a conversation still has stay as they are, and
// so do those whose message is in no conversation.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheGroupingRepairMovesStrandedMessages -v

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheGroupingRepairMovesStrandedMessages(t *testing.T) {
	ctx := context.Background()
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	dg := integration.SetupDgraph(t)
	client := opensearchInit.OpenSearchClient

	// The group has moved from stale to now; another conversation is where it
	// was; a third message's conversation is gone. Each id is made as the app
	// makes it: a group's from its people (GenerateGroupID, 32 hex
	// characters), a direct conversation's from its two (GetGroupingId).
	group := func(people ...string) string {
		t.Helper()
		id, err := helpers.GenerateGroupID(people)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	sam, maya, kim, bo := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	stale, now := group(sam, maya, bo), group(sam, maya, bo, kim)
	live, gone := helpers.GetGroupingId(sam, kim), group(maya, kim, bo)
	// A group no one has moved: its messages are where they belong.
	settled := group(sam, kim, bo)
	stranded, kept, orphan, home := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:group", "dgraph.type": "Dm", "dm_grouping_id": now, "dm_chats": []map[string]any{{"uid": "_:stranded"}}},
		{"uid": "_:stranded", "dgraph.type": "Chat", "chat_uuid": stranded, "chat_dm": map[string]any{"uid": "_:group"}},
		{"uid": "_:other", "dgraph.type": "Dm", "dm_grouping_id": live, "dm_chats": []map[string]any{{"uid": "_:kept"}}},
		{"uid": "_:kept", "dgraph.type": "Chat", "chat_uuid": kept, "chat_dm": map[string]any{"uid": "_:other"}},
		{"uid": "_:settled", "dgraph.type": "Dm", "dm_grouping_id": settled, "dm_chats": []map[string]any{{"uid": "_:home"}}},
		{"uid": "_:home", "dgraph.type": "Chat", "chat_uuid": home, "chat_dm": map[string]any{"uid": "_:settled"}},
	})
	at := time.Now().Unix()
	for id, grouping := range map[string]string{stranded: stale, kept: live, orphan: gone, home: settled} {
		chatDomain.CreateChatWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchChat{
			Uuid: id, ChatBody: "the rollout plan", ChatByUserUuid: uuid.NewString(), ChatGrpId: grouping, ChatCreatedAt: at,
		}, nil)
	}
	if _, err := client.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{"chats"}}); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	rep, err := chatBusiness.RepairChatGroupings(ctx)
	if err != nil {
		t.Fatalf("the repair: %v", err)
	}
	if want := (chatBusiness.ChatGroupingRepair{IDs: 4, Stale: 2, Moved: 1, Orphaned: 1}); rep != want {
		t.Errorf("the repair: %+v, want %+v", rep, want)
	}

	grouping := func(id string) any {
		t.Helper()
		resp, err := client.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: "chats", DocumentID: id})
		if err != nil {
			t.Fatalf("read chats/%s: %v", id, err)
		}
		raw, _ := io.ReadAll(resp.Inspect().Response.Body)
		var got struct {
			Source map[string]any `json:"_source"`
		}
		_ = json.Unmarshal(raw, &got)
		return got.Source["chat_grp_id"]
	}
	for id, want := range map[string]string{stranded: now, kept: live, orphan: gone, home: settled} {
		if got := grouping(id); got != want {
			t.Errorf("chat %s is under %v, want %s", id, got, want)
		}
	}
}
