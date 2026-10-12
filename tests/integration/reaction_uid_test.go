//go:build integration
// +build integration

package integration_test

// A reaction is changed or removed by the uid the request names
// (reaction_dgraph_id), and that uid was trusted. Changing a reaction wrote
// it as the reaction's node, and removing one deleted whatever node it named
// once that node said the caller had added it. So a member who named a
// teammate's private doc made it their "reaction", and removing that wiped
// the doc. They could change or remove anyone's reaction too.
//
// A uid now has to be the caller's own reaction on the comment or post the
// request names (business/Reaction MayChange): anything else it names is
// 404, someone else's reaction 403, and nothing is written.
//
// Run: go test -tags=integration ./tests/integration/ -run TestOnlyYourOwnReactionCanBeChangedOrRemoved -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestOnlyYourOwnReactionCanBeChangedOrRemoved(t *testing.T) {
	ctx := context.Background()
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// Reacting reads and writes the graph only: two members, a doc both can
	// read with a comment of Ben's on it, a channel both are in with a post
	// of Ben's, and Ben's private doc, which Ana can't read.
	people := map[string]uuid.UUID{"ana": uuid.New(), "ben": uuid.New()}
	doc, comment, channel, post, notes := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	live := "0001-01-01T00:00:00Z"
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": people["ana"].String(), "user_name": "ana"},
		{"uid": "_:ben", "dgraph.type": "User", "user_uuid": people["ben"].String(), "user_name": "ben"},
		{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "launch", "doc_body": "<p>the launch</p>",
			"doc_private": false, "doc_deleted_at": live, "doc_created_by": map[string]any{"uid": "_:ben"},
			"doc_comments": []map[string]any{{"uid": "_:comment", "dgraph.type": "Comment", "comment_uuid": comment,
				"comment_text": "<p>ship it</p>", "comment_by": map[string]any{"uid": "_:ben"}, "comment_doc": map[string]any{"uid": "_:doc"},
				"comment_created_at": "2026-10-01T10:00:00Z"}}},
		{"uid": "_:channel", "dgraph.type": "Channel", "ch_uuid": channel, "ch_name": "general", "ch_private": false,
			"ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:ana"}, {"uid": "_:ben"}},
			"ch_posts": []map[string]any{{"uid": "_:post", "dgraph.type": "Post", "post_uuid": post, "post_text": "<p>friday</p>",
				"post_by": map[string]any{"uid": "_:ben"}, "post_channel": map[string]any{"uid": "_:channel"},
				"post_created_at": "2026-10-01T10:00:00Z", "post_deleted_at": live}}},
		{"uid": "_:notes", "dgraph.type": "Doc", "doc_uuid": notes, "doc_title": "ben's notes", "doc_body": "<p>only mine</p>",
			"doc_private": true, "doc_deleted_at": live, "doc_created_by": map[string]any{"uid": "_:ben"},
			"doc_editing_users": []map[string]any{{"uid": "_:ben"}}},
	})

	call := func(who string, handler http.HandlerFunc, body any) (int, string) {
		t.Helper()
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[who]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String(), UserName: who}
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).
			WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, u)))
		return rec.Code, rec.Body.String()
	}
	react := func(who string, handler http.HandlerFunc, body any) string {
		t.Helper()
		code, out := call(who, handler, body)
		var answer struct {
			Data struct {
				Uid string `json:"reaction_dgraph_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &answer); code != http.StatusOK || err != nil || answer.Data.Uid == "" {
			t.Fatalf("%s reacting: %d %s", who, code, out)
		}
		return answer.Data.Uid
	}
	type node struct {
		Types   []string `json:"dgraph.type"`
		Title   string   `json:"doc_title"`
		Body    string   `json:"doc_body"`
		Emoji   string   `json:"reaction_emoji_id"`
		AddedBy *struct {
			Uid string `json:"uid"`
		} `json:"reaction_added_by"`
	}
	read := func(uid string) node {
		t.Helper()
		var got struct{ N []node }
		q := fmt.Sprintf(`{ n(func: uid(%s)) { dgraph.type doc_title doc_body reaction_emoji_id reaction_added_by { uid } } }`, uid)
		if err := json.Unmarshal(dg.Query(t, q), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.N) == 0 {
			return node{}
		}
		return got.N[0]
	}
	held := func(q string) []string {
		t.Helper()
		var got struct {
			P []struct {
				R []struct {
					Uid string `json:"uid"`
				} `json:"r"`
			}
		}
		if err := json.Unmarshal(dg.Query(t, q), &got); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range got.P {
			for _, r := range p.R {
				out = append(out, r.Uid)
			}
		}
		slices.Sort(out)
		return out
	}
	onComment := func() []string {
		return held(fmt.Sprintf(`{ p(func: eq(comment_uuid, %q)) { r: comment_reactions { uid } } }`, comment))
	}
	onPost := func() []string {
		return held(fmt.Sprintf(`{ p(func: eq(post_uuid, %q)) { r: post_reactions { uid } } }`, post))
	}
	isReaction := func(uid, emoji, by string) bool {
		r := read(uid)
		return slices.Equal(r.Types, []string{"Reaction"}) && r.Emoji == emoji && r.AddedBy != nil && r.AddedBy.Uid == uids[by]
	}

	// Ana reacts to Ben's comment and changes her mind; so does Ben.
	anaOnComment := react("ana", docController.CreateOrUpdateDocCommentReaction,
		docAdapter.InputUpdateReactionForCommentInDoc{CommentId: comment, EmojiReactionUuid: "thumbs-up"})
	if got := react("ana", docController.CreateOrUpdateDocCommentReaction,
		docAdapter.InputUpdateReactionForCommentInDoc{CommentId: comment, EmojiReactionUuid: "party", ReactionDgraphUid: anaOnComment}); got != anaOnComment {
		t.Errorf("changing her reaction answered with %s, want its own uid %s", got, anaOnComment)
	}
	if !isReaction(anaOnComment, "party", "ana") {
		t.Errorf("Ana's reaction on the comment after she changed it: %+v", read(anaOnComment))
	}
	benOnComment := react("ben", docController.CreateOrUpdateDocCommentReaction,
		docAdapter.InputUpdateReactionForCommentInDoc{CommentId: comment, EmojiReactionUuid: "eyes"})

	// And to his post.
	anaOnPost := react("ana", postController.CreateOrUpdateReaction, postAdapter.InputUpdateReactionForPost{Uuid: post, EmojiUuid: "thumbs-up"})
	if code, out := call("ana", postController.CreateOrUpdateReaction,
		postAdapter.InputUpdateReactionForPost{Uuid: post, EmojiUuid: "party", ReactionDgraphUid: anaOnPost}); code != http.StatusOK {
		t.Errorf("Ana changing her reaction on the post: %d %s", code, out)
	}
	if !isReaction(anaOnPost, "party", "ana") {
		t.Errorf("Ana's reaction on the post after she changed it: %+v", read(anaOnPost))
	}

	// What Ana may not do, each refused before anything is written.
	refused := []struct {
		name    string
		handler http.HandlerFunc
		body    any
		want    int
	}{
		{"Ben's private doc, as her reaction on his comment", docController.CreateOrUpdateDocCommentReaction,
			docAdapter.InputUpdateReactionForCommentInDoc{CommentId: comment, EmojiReactionUuid: "party", ReactionDgraphUid: uids["notes"]}, http.StatusNotFound},
		{"removing Ben's private doc, as her reaction on his comment", docController.DeleteReactionOnCommentDoc,
			docAdapter.InputUpdateReactionForCommentInDoc{CommentId: comment, ReactionDgraphUid: uids["notes"]}, http.StatusNotFound},
		{"Ben's private doc, as her reaction on his post", postController.CreateOrUpdateReaction,
			postAdapter.InputUpdateReactionForPost{Uuid: post, EmojiUuid: "party", ReactionDgraphUid: uids["notes"]}, http.StatusNotFound},
		{"removing Ben's private doc, as her reaction on his post", postController.DeletePostReaction,
			postAdapter.InputDeleteReactionForPost{Uuid: post, ReactionDgraphUid: uids["notes"]}, http.StatusNotFound},
		{"changing Ben's reaction", docController.CreateOrUpdateDocCommentReaction,
			docAdapter.InputUpdateReactionForCommentInDoc{CommentId: comment, EmojiReactionUuid: "party", ReactionDgraphUid: benOnComment}, http.StatusForbidden},
		{"removing Ben's reaction", docController.DeleteReactionOnCommentDoc,
			docAdapter.InputUpdateReactionForCommentInDoc{CommentId: comment, ReactionDgraphUid: benOnComment}, http.StatusForbidden},
		{"her reaction on the comment, named on the post", postController.DeletePostReaction,
			postAdapter.InputDeleteReactionForPost{Uuid: post, ReactionDgraphUid: anaOnComment}, http.StatusNotFound},
		{"her reaction on the post, named on the comment", docController.CreateOrUpdateDocCommentReaction,
			docAdapter.InputUpdateReactionForCommentInDoc{CommentId: comment, EmojiReactionUuid: "eyes", ReactionDgraphUid: anaOnPost}, http.StatusNotFound},
		{"a uid that isn't one", docController.DeleteReactionOnCommentDoc,
			docAdapter.InputUpdateReactionForCommentInDoc{CommentId: comment, ReactionDgraphUid: `0x1", "doc_title": "x`}, http.StatusNotFound},
	}
	for _, r := range refused {
		if code, out := call("ana", r.handler, r.body); code != r.want {
			t.Errorf("%s: %d %s, want %d", r.name, code, out, r.want)
		}
	}

	// Ben's doc is as he left it: a doc, his title and body, no reaction.
	if d := read(uids["notes"]); !slices.Equal(d.Types, []string{"Doc"}) || d.Title != "ben's notes" || d.Body != "<p>only mine</p>" || d.Emoji != "" || d.AddedBy != nil {
		t.Errorf("Ben's private doc was changed: %+v", d)
	}
	// So is his reaction, and neither the comment nor the post holds his doc.
	if !isReaction(benOnComment, "eyes", "ben") {
		t.Errorf("Ben's reaction was changed: %+v", read(benOnComment))
	}
	want := []string{anaOnComment, benOnComment}
	slices.Sort(want)
	if got := onComment(); !slices.Equal(got, want) {
		t.Errorf("the comment holds reactions %v, want %v", got, want)
	}
	if got := onPost(); !slices.Equal(got, []string{anaOnPost}) {
		t.Errorf("the post holds reactions %v, want %v", got, []string{anaOnPost})
	}

	// Ana removes her own reactions, which leaves Ben's.
	if code, out := call("ana", docController.DeleteReactionOnCommentDoc,
		docAdapter.InputUpdateReactionForCommentInDoc{CommentId: comment, ReactionDgraphUid: anaOnComment}); code != http.StatusOK {
		t.Errorf("Ana removing her reaction on the comment: %d %s", code, out)
	}
	if code, out := call("ana", postController.DeletePostReaction,
		postAdapter.InputDeleteReactionForPost{Uuid: post, ReactionDgraphUid: anaOnPost}); code != http.StatusOK {
		t.Errorf("Ana removing her reaction on the post: %d %s", code, out)
	}
	for _, uid := range []string{anaOnComment, anaOnPost} {
		if r := read(uid); len(r.Types) != 0 || r.Emoji != "" || r.AddedBy != nil {
			t.Errorf("Ana's removed reaction %s is still there: %+v", uid, r)
		}
	}
	if got := onComment(); !slices.Equal(got, []string{benOnComment}) {
		t.Errorf("after Ana removed hers, the comment holds %v, want only Ben's %s", got, benOnComment)
	}
	if got := onPost(); len(got) != 0 {
		t.Errorf("after Ana removed hers, the post holds %v", got)
	}
	if !isReaction(benOnComment, "eyes", "ben") {
		t.Errorf("Ben's reaction after Ana removed hers: %+v", read(benOnComment))
	}
}
