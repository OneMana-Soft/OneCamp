//go:build integration
// +build integration

package integration_test

// Search in one conversation (QA F-15): a channel header's Search opened the
// whole workspace's search, with the channel lost. Searching within a channel
// finds its messages only; within a DM, that conversation's only; and within a
// channel the person isn't in, nothing.
//
// Run: go test -tags=integration ./tests/integration/ -run TestSearchWithinOneConversation -v

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestSearchWithinOneConversation(t *testing.T) {
	ctx := context.Background()
	// A hit is shown only while its post or chat is still there (search's
	// liveness check reads Postgres, domain/Liveness), so every entry below is
	// a row as well as an index entry. With no database the check gives up and
	// keeps every hit, which is how this test once passed without the rows.
	env := integration.ConnectServerPostgres(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	me, maya := seedPerson(t, env.PG, "Me"), seedPerson(t, env.PG, "Maya")
	launch, design, theirs := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().Unix()
	for channel, body := range map[string]string{launch: "rollout in launch", design: "rollout in design", theirs: "rollout in theirs"} {
		post := uuid.NewString()
		exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, false)`, channel, body)
		exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, post, channel, me)
		postDomain.CreatePostWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchPost{
			Uuid: post, PostBody: body, PostByUserUuid: me, PostChannelUuid: channel, PostCreatedAt: now,
		}, nil)
	}
	dm := helpers.GetGroupingId(me, maya)
	chat := uuid.NewString()
	exec(`INSERT INTO chats (id, grp_id, created_by) VALUES ($1, $2, $3)`, chat, dm, maya)
	chatDomain.CreateChatWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchChat{
		Uuid: chat, ChatBody: "rollout in our DM", ChatByUserUuid: maya, ChatGrpId: dm, ChatCreatedAt: now,
		ChatParticipants: []*openSearchStruct.OpenSearchChatParticipants{{Uuid: maya}, {Uuid: me}},
	}, nil)
	if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{"posts", "chats"}}); err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	mine := []*dgraphStruct.DgraphChannel{{Uuid: launch}, {Uuid: design}}

	// found: what a search for "rollout" within a conversation finds, as text.
	found := func(within globalSearchBusiness.Within) []string {
		t.Helper()
		p, err := globalSearchBusiness.GetUnifiedGlobalSearchPage(ctx, me, me+"@example.test", mine, nil, nil, "rollout", nil, within)
		if err != nil {
			t.Fatal(err)
		}
		var said []string
		for _, h := range p.Page {
			switch {
			case h.Post != nil:
				said = append(said, h.Post.PostBody)
			case h.Chat != nil:
				said = append(said, h.Chat.ChatBody)
			}
		}
		sort.Strings(said)
		return said
	}
	equal := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// Entries show in a search within a second of being written.
	everything := []string{"rollout in design", "rollout in launch", "rollout in our DM"}
	deadline := time.Now().Add(20 * time.Second)
	for got := found(globalSearchBusiness.Within{}); !equal(got, everything); got = found(globalSearchBusiness.Within{}) {
		if time.Now().After(deadline) {
			t.Fatalf("everywhere: %v, want %v", got, everything)
		}
		time.Sleep(250 * time.Millisecond)
	}
	for name, c := range map[string]struct {
		within globalSearchBusiness.Within
		want   []string
	}{
		"within #launch":              {globalSearchBusiness.Within{ChannelUUID: launch}, []string{"rollout in launch"}},
		"within the DM":               {globalSearchBusiness.Within{GroupingID: dm}, []string{"rollout in our DM"}},
		"within a channel I'm not in": {globalSearchBusiness.Within{ChannelUUID: theirs}, nil},
	} {
		if got := found(c.within); !equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
