//go:build integration
// +build integration

package integration_test

// When someone is added to a group, the group moves to a new id, and its
// messages, replies and files in search move with it. The chats index has no
// mapping of its own for chat_grp_id, so the field is text, and the move's
// term on it matched nothing: replies and files moved, the messages stayed
// under the old id, and a search hit on one opened a conversation that was no
// longer there. The move now matches the whole id (ChatGroupingIDField).
//
// Run: go test -tags=integration ./tests/integration/ -run TestAGroupsSearchEntriesMoveWithIt -v

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	openSearchChatModels "github.com/akashc777/OneCamp/models/openSearch/Chat"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAGroupsSearchEntriesMoveWithIt(t *testing.T) {
	ctx := context.Background()
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	client := opensearchInit.OpenSearchClient

	// A group's ids are uuids, which text cuts into words at their hyphens.
	oldID, newID := uuid.NewString(), uuid.NewString()
	sam, chat, file, reply := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().Unix()
	chatDomain.CreateChatWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchChat{
		Uuid: chat, ChatBody: "the rollout plan", ChatByUserUuid: sam, ChatGrpId: oldID, ChatCreatedAt: now,
		ChatParticipants: []*openSearchStruct.OpenSearchChatParticipants{{Uuid: sam}},
	}, []*dgraphStruct.DgraphAttachment{{Uuid: file, FileName: "plan.pdf"}})
	commentDomain.CreateChatCommentWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchComment{
		Uuid: reply, CommentBody: "looks good", CommentByUserUuid: sam, CommentChatGrpId: oldID, CommentChatUuid: chat,
	}, nil)
	if _, err := client.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{"chats", "comments", "attachments"}}); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	if err := openSearchChatModels.MigrateChatGroupIDInOpensearch(ctx, oldID, newID); err != nil {
		t.Fatalf("moving the group: %v", err)
	}

	source := func(index, id string) map[string]any {
		t.Helper()
		resp, err := client.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: index, DocumentID: id})
		if err != nil {
			t.Fatalf("read %s/%s: %v", index, id, err)
		}
		raw, _ := io.ReadAll(resp.Inspect().Response.Body)
		var got struct {
			Source map[string]any `json:"_source"`
		}
		_ = json.Unmarshal(raw, &got)
		return got.Source
	}
	for _, c := range []struct{ index, id, field string }{
		{"chats", chat, "chat_grp_id"},
		{"comments", reply, "comment_chat_grp_id"},
		{"attachments", file, "attachment_chat_grp_id"},
	} {
		if got := source(c.index, c.id)[c.field]; got != newID {
			t.Errorf("%s's %s is %v after the move, want the new id %s", c.index, c.field, got, newID)
		}
	}
}
