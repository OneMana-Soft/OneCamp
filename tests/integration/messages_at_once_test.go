//go:build integration
// +build integration

package integration_test

// Posts written into one channel at once all land, and so do messages written
// into one DM at once, against a real Dgraph.
// Each is an upsert that adds itself to the channel's ch_posts, and Dgraph
// aborts one of two such transactions that overlap ("Please retry"). Until 19
// Oct 2026 the post writer (models/dgraph/Post CreateOrUpdatePost) didn't
// retry, so of a join and a rename told to a channel together (chat B28), one
// history line was lost; so was one of two people's posts sent at once. The
// chat and comment writers (models/dgraph/Chat, Comment) were the same.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestMessagesWrittenIntoOneConversationAtOnceAllLand' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestMessagesWrittenIntoOneConversationAtOnceAllLand(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)

	const live = "0001-01-01T00:00:00Z"
	channel, grouping := uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "Maya", "user_deleted_at": live},
		{"uid": "_:c", "dgraph.type": "Channel", "ch_uuid": channel, "ch_name": "launch", "ch_deleted_at": live},
		{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": grouping},
	})

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			now, zero := time.Now(), time.Time{}
			_, errs[i] = postDomain.CreateOrUpdateDgraphPost(ctx, &dgraphStruct.DgraphPost{
				Uid: "uid(po)", Uuid: uuid.NewString(), DType: []string{"Post"},
				Text:      fmt.Sprintf("<p>%d</p>", i),
				PostBy:    &dgraphStruct.DgraphUser{Uid: uids["maya"]},
				Channel:   &dgraphStruct.DgraphChannel{Uid: uids["c"], Posts: []*dgraphStruct.DgraphPost{{Uid: "uid(po)"}}},
				CreatedAt: &now, DeletedAt: &zero,
			})
		}(i)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			now, zero, id := time.Now(), time.Time{}, uuid.NewString()
			_, _, errs[n+i] = chatDomain.CreateDgraphChat(ctx, &dgraphStruct.DgraphDm{
				Uid: "uid(dm)", GroupingId: grouping, DType: []string{"Dm"},
				Chats: []*dgraphStruct.DgraphChat{{
					Uid: "uid(ch)", Uuid: id, DType: []string{"Chat"}, CreatedAt: &now, DeletedAt: &zero,
					From: &dgraphStruct.DgraphUser{Uid: uids["maya"]}, DM: &dgraphStruct.DgraphDm{Uid: "uid(dm)"},
					Body: fmt.Sprintf("<p>%d</p>", i),
				}},
			}, id)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("post %d: %v", i, err)
		}
	}

	var got struct {
		C []struct {
			N int `json:"count(ch_posts)"`
		} `json:"c"`
		D []struct {
			N int `json:"count(dm_chats)"`
		} `json:"d"`
	}
	q := fmt.Sprintf(`{ c(func: eq(ch_uuid, %q)) { count(ch_posts) } d(func: eq(dm_grouping_id, %q)) { count(dm_chats) } }`, channel, grouping)
	if err := json.Unmarshal(dg.Query(t, q), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.C) != 1 || got.C[0].N != n {
		t.Errorf("the channel has %+v posts, want %d", got.C, n)
	}
	if len(got.D) != 1 || got.D[0].N != n {
		t.Errorf("the DM has %+v messages, want %d", got.D, n)
	}
}
