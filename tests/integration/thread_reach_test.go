//go:build integration

package integration_test

// A thread is read where its channel is (V-1087). Whoever may read a channel's
// posts (channelBusiness.CanRead, as latestPosts asks it) reads its threads:
// its members, its managers, and, for a public channel, anyone signed in; an
// archived channel's, read only. A private channel's non-member reads none.
// The thread's access decision says the same, and replying stays its members'.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// channelUID is the graph's uid of the channel with id.
func channelUID(t *testing.T, dg *integration.DgraphEnv, id string) string {
	t.Helper()
	return graphUID(t, dg, "ch_uuid", id)
}

// graphUID is the graph uid of the one node whose predicate is id.
func graphUID(t *testing.T, dg *integration.DgraphEnv, predicate, id string) string {
	t.Helper()
	var got struct {
		C []struct {
			UID string `json:"uid"`
		} `json:"c"`
	}
	if err := json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ c(func: eq(%s, %q)) { uid } }`, predicate, id)), &got); err != nil || len(got.C) != 1 {
		t.Fatalf("the node of %s %s in the graph: %v %+v", predicate, id, err, got)
	}
	return got.C[0].UID
}

// archiveChannel gives the channel with id a deletion time an hour ago: an
// archived channel, as deleting one leaves it.
func archiveChannel(t *testing.T, w *b25World, id string) {
	t.Helper()
	w.dg.Mutate(t, map[string]any{"uid": channelUID(t, w.dg, id), "ch_deleted_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
}

func TestAThreadIsReadableWhereItsChannelIs(t *testing.T) {
	w := newB25World(t)
	// Decisions read the graph, whatever an earlier test left the access
	// source at (CR-372 F2).
	t.Cleanup(conversationDomain.StartedForTest(conversationDomain.StartedAt()))
	open, closed, archived := uuid.NewString(), uuid.NewString(), uuid.NewString()
	openPost, closedPost, archivedPost := uuid.NewString(), uuid.NewString(), uuid.NewString()
	// Maya and Jonas are in the public channels and Sam isn't; Jonas is in the
	// private one, and Maya manages it without being in it.
	w.channel(t, open, "open-"+open[:8], false, "", []string{"maya", "jonas"}, nil, map[string]string{openPost: "jonas"})
	w.channel(t, closed, "closed-"+closed[:8], true, "", []string{"jonas"}, []string{"maya"}, map[string]string{closedPost: "jonas"})
	w.channel(t, archived, "archived-"+archived[:8], false, "", []string{"maya", "jonas"}, nil, map[string]string{archivedPost: "jonas"})
	archiveChannel(t, w, archived)

	for _, c := range []struct {
		name, who, channel, post string
		want                     int
	}{
		{"a public channel's non-member", "sam", open, openPost, http.StatusOK},
		{"a public channel's member", "jonas", open, openPost, http.StatusOK},
		{"a private channel's member", "jonas", closed, closedPost, http.StatusOK},
		{"a private channel's manager who isn't in it", "maya", closed, closedPost, http.StatusOK},
		{"a private channel's non-member", "sam", closed, closedPost, http.StatusForbidden},
		{"an archived public channel's member", "jonas", archived, archivedPost, http.StatusOK},
		{"an archived public channel's non-member", "sam", archived, archivedPost, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			thread, data, msg := w.call(t, c.who, postController.PostByUUIDWithAllComments, http.MethodGet, nil, "post_uuid", c.post)
			posts, _, _ := w.call(t, c.who, postController.GetLatestPosts, http.MethodGet, nil, "channel_uuid", c.channel)
			if posts != c.want {
				t.Fatalf("the channel's posts: %d, want %d", posts, c.want)
			}
			if thread != posts {
				t.Errorf("the thread: %d %q, its channel's posts %d: a thread reads as its channel's posts do", thread, msg, posts)
			}
			if thread == http.StatusOK {
				var got struct {
					Uuid string `json:"post_uuid"`
				}
				if err := json.Unmarshal(data, &got); err != nil || got.Uuid != c.post {
					t.Errorf("the thread read is of %q, not %q (%v)", got.Uuid, c.post, err)
				}
			}
		})
	}

	// Reading a thread isn't replying in it: that stays the channel's members'.
	if code, _, _ := w.call(t, "sam", postController.CreateCommentInPost, http.MethodPost, postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: openPost, HTMLText: "<p>From outside</p>"}); code == http.StatusOK {
		t.Error("a public channel's non-member replied in its thread")
	}

	// The thread's access decision, from the graph, says what the route does:
	// one nobody may read isn't there for them (conversationBusiness.Resolve).
	for _, c := range []struct {
		name, who, post string
		want            accessBusiness.Level
		archived        bool
	}{
		{"a public channel's non-member", "sam", openPost, conversationBusiness.LevelRead, false},
		{"a public channel's member", "jonas", openPost, conversationBusiness.LevelReply, false},
		{"a private channel's manager who isn't in it", "maya", closedPost, conversationBusiness.LevelRead, false},
		{"a private channel's non-member", "sam", closedPost, accessBusiness.LevelNone, false},
		{"an archived public channel's non-member", "sam", archivedPost, conversationBusiness.LevelRead, true},
	} {
		ctx := w.as(t, c.who)
		who := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
		d, err := conversationBusiness.Resolve(ctx, &who, conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: c.post})
		if c.want == accessBusiness.LevelNone && errors.Is(err, conversationBusiness.ErrNotFound) {
			err = nil
		}
		if err != nil || d.Level != c.want || d.Archived != c.archived {
			t.Errorf("%s's decision: %s archived %v (%v), want %s archived %v", c.name, d.Level, d.Archived, err, c.want, c.archived)
		}
	}
}
