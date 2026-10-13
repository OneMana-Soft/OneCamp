//go:build integration
// +build integration

package integration_test

// A message is read where it is, and a deleted one is gone, against real
// Postgres and Dgraph.
//
// The text a forward previews (GET /po/getOnlyPostText/{channel}/{post}) was
// read by the post's id alone, after checking only that the asker was in the
// channel named: anyone in #general could read any private channel's posts,
// deleted ones too, with a post's id. Now the post is held to that channel,
// and the channel to the asker.
//
// A deleted message's thread (GET /po/allComments/{post}) returned its text,
// attachments and replies, and a deleted message still took replies and
// reactions: the thread's query never read the deletion time. Now a member
// opens such a thread on a "This message was deleted." placeholder over its
// replies, or not at all when there are none; and it takes nothing new. A
// deleted direct message's preview, and forwarding a deleted message, are
// refused the same way.
//
// Run: go test -tags=integration ./tests/integration/ -run TestMessageReadsAreHeld -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	userAdapter "github.com/akashc777/OneCamp/adapter/User"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestMessageReadsAreHeld(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	people := map[string]uuid.UUID{}
	for _, name := range []string{"sam", "lee", "alex"} {
		id := uuid.New()
		people[name] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.test", name, name)
	}
	general, mine, leadership := uuid.New(), uuid.New(), uuid.New()
	for ch, name := range map[uuid.UUID]string{general: "general", mine: "mine", leadership: "leadership"} {
		exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, $3)`, ch, name, ch != general)
	}
	// In #mine: a live post, a deleted one nobody answered, and a deleted one
	// with a reply. In #leadership, which sam isn't in: a post.
	live, gone, answered, secret := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for post, ch := range map[uuid.UUID]uuid.UUID{live: mine, gone: mine, answered: mine, secret: leadership} {
		exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, post, ch, people["lee"])
	}
	exec(`UPDATE posts SET deleted_at = NOW() WHERE id IN ($1, $2)`, gone, answered)
	zero, deletedAt := "0001-01-01T00:00:00Z", "2026-10-12T09:00:00Z"
	user := func(name string) map[string]any {
		return map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": people[name].String(), "user_name": name}
	}
	post := func(key string, id uuid.UUID, ch, text, deleted string, replies ...string) map[string]any {
		p := map[string]any{"uid": "_:" + key, "dgraph.type": "Post", "post_uuid": id.String(), "post_text": text,
			"post_by": map[string]any{"uid": "_:lee"}, "post_channel": map[string]any{"uid": "_:" + ch},
			"post_created_at": "2026-10-12T08:00:00Z", "post_deleted_at": deleted}
		var comments []map[string]any
		for _, r := range replies {
			comments = append(comments, map[string]any{"dgraph.type": "Comment", "comment_uuid": uuid.NewString(),
				"comment_text": r, "comment_deleted_at": zero, "comment_by": map[string]any{"uid": "_:lee"},
				"comment_created_at": "2026-10-12T08:30:00Z"})
		}
		if comments != nil {
			p["post_comments"] = comments
		}
		return p
	}
	chat := uuid.New()
	dg.Mutate(t, []map[string]any{
		user("sam"), user("lee"), user("alex"),
		{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general", "ch_private": false,
			"ch_deleted_at": zero, "ch_members": []map[string]any{{"uid": "_:sam"}, {"uid": "_:lee"}}},
		{"uid": "_:mine", "dgraph.type": "Channel", "ch_uuid": mine.String(), "ch_name": "mine", "ch_private": true,
			"ch_deleted_at": zero, "ch_members": []map[string]any{{"uid": "_:sam"}, {"uid": "_:lee"}},
			"ch_posts": []map[string]any{
				post("live", live, "mine", "<p>ship it</p>", zero),
				post("gone", gone, "mine", "<p>the old price</p>", deletedAt),
				post("answered", answered, "mine", "<p>the launch price is 40</p>", deletedAt, "<p>40 works</p>"),
			}},
		{"uid": "_:leadership", "dgraph.type": "Channel", "ch_uuid": leadership.String(), "ch_name": "leadership", "ch_private": true,
			"ch_deleted_at": zero, "ch_members": []map[string]any{{"uid": "_:lee"}},
			"ch_posts": []map[string]any{post("secret", secret, "leadership", "<p>the layoffs are on Friday</p>", zero)}},
		{"dgraph.type": "Chat", "chat_uuid": chat.String(), "chat_body_text": "<p>my password is hunter2</p>",
			"chat_from": map[string]any{"uid": "_:sam"}, "chat_to": map[string]any{"uid": "_:alex"},
			"chat_created_at": "2026-10-12T08:00:00Z", "chat_deleted_at": deletedAt},
	})

	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people["sam"])
	if err != nil || pg == nil {
		t.Fatalf("load sam: %v", err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people["sam"].String())
	if err != nil || gr == nil {
		t.Fatalf("load sam's node: %v", err)
	}
	asSam := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})

	get := func(handler http.HandlerFunc, params ...string) (int, string) {
		t.Helper()
		rc := chi.NewRouteContext()
		for i := 0; i+1 < len(params); i += 2 {
			rc.URLParams.Add(params[i], params[i+1])
		}
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(asSam, chi.RouteCtxKey, rc)))
		return rec.Code, rec.Body.String()
	}
	send := func(handler http.HandlerFunc, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asSam))
		return rec.Code, rec.Body.String()
	}
	text := func(ch, p uuid.UUID) (int, string) {
		return get(postController.GetOnlyPostText, "channel_uuid", ch.String(), "post_uuid", p.String())
	}

	t.Run("a post's text is read only where it is", func(t *testing.T) {
		for name, tc := range map[string]struct {
			ch, post uuid.UUID
		}{
			"a private channel's post, asked under a channel they're in": {general, secret},
			"a private channel's post, asked under its own channel":      {leadership, secret},
			"their channel's post, asked under another channel":          {general, live},
			"a deleted post in their channel":                            {mine, gone},
			"a post that doesn't exist":                                  {mine, uuid.New()},
		} {
			code, body := text(tc.ch, tc.post)
			if code != http.StatusNotFound || strings.Contains(body, "layoffs") || strings.Contains(body, "old price") || strings.Contains(body, "ship it") {
				t.Errorf("%s: %d %s, want 404 and none of the text", name, code, body)
			}
		}
		code, body := text(mine, live)
		if code != http.StatusOK || !strings.Contains(body, "ship it") {
			t.Fatalf("their own channel's post: %d %s", code, body)
		}
		// The answer is what it always was: no channel, no deletion time.
		if strings.Contains(body, "post_channel") || strings.Contains(body, "post_deleted_at") {
			t.Errorf("the answer grew: %s", body)
		}
	})

	t.Run("a deleted message's thread shows nothing of it", func(t *testing.T) {
		if code, body := get(postController.PostByUUIDWithAllComments, "post_uuid", gone.String()); code != http.StatusNotFound || strings.Contains(body, "old price") {
			t.Errorf("a deleted message with no replies: %d %s, want 404", code, body)
		}
		code, body := get(postController.PostByUUIDWithAllComments, "post_uuid", answered.String())
		if code != http.StatusOK || strings.Contains(body, "launch price") || !strings.Contains(body, "This message was deleted.") ||
			!strings.Contains(body, "40 works") || strings.Contains(body, "post_by") {
			t.Errorf("a deleted message with a reply: %d %s, want the placeholder over the reply", code, body)
		}
		if code, body := get(postController.PostByUUIDWithAllComments, "post_uuid", live.String()); code != http.StatusOK || !strings.Contains(body, "ship it") {
			t.Errorf("a live message: %d %s", code, body)
		}
		// One that isn't there is as gone as a deleted one: a 404, which the
		// page reads as "This thread isn't available", not "Couldn't load".
		if code, body := get(postController.PostByUUIDWithAllComments, "post_uuid", uuid.NewString()); code != http.StatusNotFound {
			t.Errorf("a message that isn't there: %d %s, want 404", code, body)
		}
	})

	t.Run("a deleted message takes nothing new", func(t *testing.T) {
		replies := func() (n int) {
			if err := env.PG.QueryRow(`SELECT count(*) FROM comments WHERE created_by = $1`, people["sam"]).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		if code, body := send(postController.CreateCommentInPost, postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: gone.String(), HTMLText: "<p>late</p>"}); code != http.StatusNotFound || replies() != 0 {
			t.Errorf("a reply to a deleted message: %d %s, %d replies written", code, body, replies())
		}
		if code, body := send(postController.CreateOrUpdateReaction, postAdapter.InputUpdateReactionForPost{Uuid: gone.String(), EmojiUuid: "+1"}); code != http.StatusNotFound {
			t.Errorf("a reaction to a deleted message: %d %s, want 404", code, body)
		}
		if n := graphCount(t, `{ q(func: eq(post_uuid, "`+gone.String()+`")) { n: count(post_reactions) } }`); n != 0 {
			t.Errorf("the deleted message holds %d reactions", n)
		}
		if code, body := send(userController.FwdUserMessage, userAdapter.UserFwdMsgInput{
			HtmlText: "<p>fyi</p>", ChannelUuid: mine.String(), PostUuid: gone.String(),
			FwdTo: []userAdapter.UserAndChannelFwdMessage{{UserUuid: people["alex"].String()}},
		}); code != http.StatusNotFound {
			t.Errorf("forwarding a deleted message: %d %s, want 404", code, body)
		}
		// A live one still does.
		if code, body := send(postController.CreateCommentInPost, postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: live.String(), HTMLText: "<p>on it</p>"}); code != http.StatusOK || replies() != 1 {
			t.Errorf("a reply to a live message: %d %s", code, body)
		}
	})

	t.Run("a deleted direct message's preview is gone", func(t *testing.T) {
		if code, body := get(chatController.GetChatText, "chat_uuid", chat.String()); code != http.StatusNotFound || strings.Contains(body, "hunter2") {
			t.Errorf("a deleted direct message: %d %s, want 404", code, body)
		}
	})
}
