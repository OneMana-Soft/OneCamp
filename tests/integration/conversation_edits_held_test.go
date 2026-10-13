//go:build integration
// +build integration

package integration_test

// A direct or group message is edited in the conversation it is in, and a
// deleted one is gone, against real Postgres, Redis and Dgraph.
//
// An edit to a group's message, and to a reply in a conversation, took the
// conversation from the request: its author could publish the edit into
// another group's live stream, and move the message's AI entry there, by
// naming that group (its id is a hash of its people's ids). Now the stored
// message says where it is, a request naming another conversation is refused,
// and whoever edits must be in it.
//
// A deleted direct or group message stayed readable through its thread, and
// took edits, replies and reactions. Now its thread opens on "This message
// was deleted." over the replies, or not at all without them, and it takes
// nothing new.
//
// Run: go test -tags=integration ./tests/integration/ -run TestConversationEditsAreHeld -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestConversationEditsAreHeld(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	people := map[string]uuid.UUID{}
	uids := map[string]string{}
	for _, name := range []string{"sam", "alex", "priya", "maya"} {
		id := uuid.New()
		people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		got := dg.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
		uids[name] = got["u"]
	}
	as := func(name string) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[name])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", name, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[name].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", name, err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	send := func(name string, handler http.HandlerFunc, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(name)))
		return rec.Code, rec.Body.String()
	}
	get := func(name string, handler http.HandlerFunc, params ...string) (int, string) {
		t.Helper()
		rc := chi.NewRouteContext()
		for i := 0; i+1 < len(params); i += 2 {
			rc.URLParams.Add(params[i], params[i+1])
		}
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(as(name), chi.RouteCtxKey, rc)))
		return rec.Code, rec.Body.String()
	}
	created := func(code int, body string) string {
		t.Helper()
		var out struct {
			Data struct {
				Uuid string `json:"uuid"`
				ID   string `json:"comment_id"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		if code != http.StatusOK || (out.Data.Uuid == "" && out.Data.ID == "") {
			t.Fatalf("not created: %d %s", code, body)
		}
		return out.Data.Uuid + out.Data.ID
	}
	group := func(names ...string) string {
		var ids []string
		for _, n := range names {
			ids = append(ids, people[n].String())
		}
		id, _ := helpers.GenerateGroupID(ids)
		return id
	}
	text := func(chat string) string {
		t.Helper()
		var out struct {
			Q []struct {
				Body string `json:"chat_body_text"`
			} `json:"q"`
		}
		_ = json.Unmarshal(dg.Query(t, `{ q(func: eq(chat_uuid, "`+chat+`")) { chat_body_text } }`), &out)
		if len(out.Q) != 1 {
			t.Fatalf("message %s: %+v", chat, out)
		}
		return out.Q[0].Body
	}

	// Sam's group with Alex and Priya; Alex's group with Priya and Maya, which
	// Sam isn't in.
	ours := created(send("sam", chatController.CreateGroupChat, chatAdapter.ChatInfo{
		Participants: []string{uids["sam"], uids["alex"], uids["priya"]}, TextHtml: "<p>Our plan</p>"}))
	theirs := created(send("alex", chatController.CreateGroupChat, chatAdapter.ChatInfo{
		Participants: []string{uids["alex"], uids["priya"], uids["maya"]}, TextHtml: "<p>Their plan</p>"}))
	g1, g2 := group("sam", "alex", "priya"), group("alex", "priya", "maya")
	reply := created(send("sam", chatController.CreateChatComment, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: ours, HTMLText: "<p>First reply</p>"}))

	t.Run("an edit is held to the conversation its message is in", func(t *testing.T) {
		if code, body := send("sam", chatController.UpdateChatInGroup, chatAdapter.ChatInfo{Uuid: ours, GrpUuid: g2, TextHtml: "<p>injected</p>"}); code != http.StatusNotFound || text(ours) != "<p>Our plan</p>" {
			t.Errorf("an edit naming another group: %d %s; the message reads %q", code, body, text(ours))
		}
		if code, body := send("sam", chatController.UpdateChatComment, chatAdapter.InputCreateOrUpdateCommentToChat{Uuid: reply, ChatUuid: theirs, HTMLText: "<p>injected</p>"}); code != http.StatusNotFound {
			t.Errorf("a reply's edit naming another group's message: %d %s", code, body)
		}
		// In its own conversation, it is saved.
		if code, body := send("sam", chatController.UpdateChatInGroup, chatAdapter.ChatInfo{Uuid: ours, GrpUuid: g1, TextHtml: "<p>Our new plan</p>"}); code != http.StatusOK || text(ours) != "<p>Our new plan</p>" {
			t.Errorf("an edit in its own group: %d %s; the message reads %q", code, body, text(ours))
		}
		if code, body := send("sam", chatController.UpdateChatComment, chatAdapter.InputCreateOrUpdateCommentToChat{Uuid: reply, ChatUuid: ours, HTMLText: "<p>First reply, edited</p>"}); code != http.StatusOK {
			t.Errorf("a reply's edit in its own conversation: %d %s", code, body)
		}
	})

	t.Run("a deleted message is gone from its thread and takes nothing new", func(t *testing.T) {
		dg.Mutate(t, map[string]any{"uid": uidOfChat(t, dg, ours), "chat_deleted_at": time.Now().Format(time.RFC3339)})
		code, body := get("sam", chatController.GetDgraphChatByUUIDWithAllComments, "chat_uuid", ours)
		if code != http.StatusOK || strings.Contains(body, "plan") || !strings.Contains(body, "This message was deleted.") || !strings.Contains(body, "First reply") {
			t.Errorf("its thread: %d %s, want the placeholder over the reply", code, body)
		}
		if code, body := send("sam", chatController.UpdateChatInGroup, chatAdapter.ChatInfo{Uuid: ours, GrpUuid: g1, TextHtml: "<p>back</p>"}); code != http.StatusNotFound {
			t.Errorf("editing it: %d %s", code, body)
		}
		if code, body := send("alex", chatController.CreateChatComment, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: ours, HTMLText: "<p>late</p>"}); code != http.StatusNotFound {
			t.Errorf("replying to it: %d %s", code, body)
		}
		if code, body := send("alex", chatController.CreateOrUpdateChatReaction, chatAdapter.InputUpdateReactionForChat{Uuid: ours, EmojiReactionUuid: "+1"}); code != http.StatusNotFound {
			t.Errorf("reacting to it: %d %s", code, body)
		}
		if code, body := send("sam", chatController.UpdateChatComment, chatAdapter.InputCreateOrUpdateCommentToChat{Uuid: reply, ChatUuid: ours, HTMLText: "<p>again</p>"}); code != http.StatusNotFound {
			t.Errorf("editing a reply under it: %d %s", code, body)
		}
	})

	t.Run("a deleted direct message with no replies has no thread, and isn't edited", func(t *testing.T) {
		direct := created(send("sam", chatController.CreateChat, chatAdapter.ChatInfo{ToUuid: people["maya"].String(), TextHtml: "<p>the code is 4512</p>"}))
		dg.Mutate(t, map[string]any{"uid": uidOfChat(t, dg, direct), "chat_deleted_at": time.Now().Format(time.RFC3339)})
		if code, body := get("sam", chatController.GetDgraphChatByUUIDWithAllComments, "chat_uuid", direct); code != http.StatusNotFound || strings.Contains(body, "4512") {
			t.Errorf("its thread: %d %s, want 404", code, body)
		}
		if code, body := send("sam", chatController.UpdateChat, chatAdapter.ChatInfo{Uuid: direct, TextHtml: "<p>back</p>"}); code != http.StatusNotFound {
			t.Errorf("editing it: %d %s", code, body)
		}
		// A message that isn't there is as gone: a 404, not "Couldn't load".
		if code, body := get("sam", chatController.GetDgraphChatByUUIDWithAllComments, "chat_uuid", uuid.NewString()); code != http.StatusNotFound {
			t.Errorf("the thread of a message that isn't there: %d %s, want 404", code, body)
		}
	})
}

// uidOfChat is the graph uid of the message with chatUUID.
func uidOfChat(t *testing.T, dg *integration.DgraphEnv, chatUUID string) string {
	t.Helper()
	var out struct {
		Q []struct {
			Uid string `json:"uid"`
		} `json:"q"`
	}
	_ = json.Unmarshal(dg.Query(t, `{ q(func: eq(chat_uuid, "`+chatUUID+`")) { uid } }`), &out)
	if len(out.Q) != 1 {
		t.Fatalf("no message %s", chatUUID)
	}
	return out.Q[0].Uid
}
