//go:build integration
// +build integration

package integration_test

// A reaction in a direct conversation is pushed to whoever wrote the message,
// against real Postgres, Redis and Dgraph. It was pushed to the devices of
// whoever reacted, and the author heard nothing. Now it reaches the author, as
// their setting for the conversation allows ("Nothing" hears nothing).
//
// Run: go test -tags=integration ./tests/integration/ -run TestADirectMessageReactionReachesItsAuthor -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestADirectMessageReactionReachesItsAuthor(t *testing.T) {
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

	var mu sync.Mutex
	var pushes []sentPush
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		pushes = append(pushes, sentPush{data, tokens})
		mu.Unlock()
	})()

	people := map[string]uuid.UUID{}
	for _, name := range []string{"sam", "alex"} {
		id := uuid.New()
		people[name] = id
		for _, q := range []string{
			`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`,
			`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`,
		} {
			if _, err := env.PG.Exec(q, id, name); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
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
	send := func(name string, handler http.HandlerFunc, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(name)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Data
	}
	// pushed answers whose devices the reaction of kind reached.
	pushed := func(kind string, want bool) []string {
		t.Helper()
		var to []string
		find := func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, p := range pushes {
				if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == kind {
					to = slices.Clone(p.tokens)
					return true
				}
			}
			return false
		}
		if want {
			waitFor(t, "a "+kind+" push", find)
		} else {
			time.Sleep(time.Second) // it would be on its way by now
			find()
		}
		mu.Lock()
		pushes = nil
		mu.Unlock()
		return to
	}

	lunch := send("sam", chatController.CreateChat, chatAdapter.ChatInfo{ToUuid: people["alex"].String(), TextHtml: "<p>Lunch?</p>"})["uuid"].(string)

	t.Run("a reaction to a message reaches whoever wrote it", func(t *testing.T) {
		send("alex", chatController.CreateOrUpdateChatReaction, chatAdapter.InputUpdateReactionForChat{Uuid: lunch, EmojiReactionUuid: "thumbs_up"})
		if to := pushed(firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION, true); !slices.Equal(to, []string{"tok-sam"}) {
			t.Errorf("pushed to %v, want sam's phone, not alex's", to)
		}
	})

	t.Run("a reaction to a reply reaches whoever wrote the reply", func(t *testing.T) {
		reply := send("sam", chatController.CreateChatComment, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: lunch, HTMLText: "<p>12:30?</p>"})["comment_id"].(string)
		send("alex", chatController.CreateOrUpdateChatCommentReaction, chatAdapter.InputUpdateReactionForCommentInChat{Uuid: reply, EmojiReactionUuid: "thumbs_up"})
		if to := pushed(firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_COMMENT_REACTION, true); !slices.Equal(to, []string{"tok-sam"}) {
			t.Errorf("pushed to %v, want sam's phone, not alex's", to)
		}
	})

	t.Run("an author who hears nothing from the conversation isn't pushed", func(t *testing.T) {
		grp := helpers.GetGroupingId(people["sam"].String(), people["alex"].String())
		if _, err := env.PG.Exec(`UPDATE users_chat_notification SET notification_type = 'block' WHERE user_id = $1 AND grp_id = $2`, people["sam"], grp); err != nil {
			t.Fatal(err)
		}
		// A message alex hasn't reacted to: an author hears of someone's
		// reactions once, so a second one to lunch would say nothing either way.
		coffee := send("sam", chatController.CreateChat, chatAdapter.ChatInfo{ToUuid: people["alex"].String(), TextHtml: "<p>Coffee?</p>"})["uuid"].(string)
		pushed(firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION, false) // clears what came before
		send("alex", chatController.CreateOrUpdateChatReaction, chatAdapter.InputUpdateReactionForChat{Uuid: coffee, EmojiReactionUuid: "heart"})
		if to := pushed(firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION, false); len(to) != 0 {
			t.Errorf("pushed to %v while sam hears nothing from it", to)
		}
	})
}
