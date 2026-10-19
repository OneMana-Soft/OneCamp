//go:build integration
// +build integration

package integration_test

// A channel post the durable way (chat A2, MESSAGE_PIPELINE=channel), sent
// through /po/createPost, against real Postgres 12, Redis, Dgraph and the
// broker: the post, its ledger row and its event are written together; its
// side effects run from the outbox (the channel's open apps get it live, search
// gets its entry, the person it mentions is pushed once, every sink has it); a
// second send with its client id answers the first post, even after Redis
// forgot it; an id the ledger can't keep doesn't refuse the post; and an event
// whose writer died is decided by whether the post's node was written.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestADurableChannelPost' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	messagePipeline "github.com/akashc777/OneCamp/business/MessagePipeline"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// pipelineWorld is what a durable post needs: the stores, Ada and Sam in
// #launch (Sam hears of mentions), and what search was sent.
type pipelineWorld struct {
	env     *integration.Env
	channel uuid.UUID
	people  map[string]uuid.UUID
	uids    map[string]string
	mu      sync.Mutex
	indexed []string
}

func newPipelineWorld(t *testing.T) *pipelineWorld {
	t.Helper()
	t.Setenv(messagePipeline.SurfacesEnv, messagePipeline.SurfaceChannel)
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	// The writers' deliveries finish before the stores go (cleanups run last
	// first), so none reaches the next test's database through the shared pool.
	t.Cleanup(func() { helpers.SettleBackground(10 * time.Second) })
	w := &pipelineWorld{env: env, channel: uuid.New(), people: map[string]uuid.UUID{}, uids: map[string]string{}}
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		w.indexed = append(w.indexed, string(body))
		w.mu.Unlock()
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	t.Cleanup(search.Close)
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
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, w.channel)
	var nodes []map[string]any
	for _, name := range []string{"ada", "sam"} {
		id := uuid.New()
		w.people[name] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`, id, name)
		exec(`INSERT INTO users_channel_notification (user_id, channel_id, notification_type) VALUES ($1, $2, 'mention')`, id, w.channel)
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	got := dg.Mutate(t, append(nodes, map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": w.channel.String(),
		"ch_name": "launch", "ch_private": false, "ch_deleted_at": "0001-01-01T00:00:00Z",
		"ch_members": []map[string]any{{"uid": "_:ada"}, {"uid": "_:sam"}}}))
	for _, name := range []string{"ada", "sam"} {
		w.uids[name] = got[name]
	}
	return w
}

// as is a person as the routes see them.
func (w *pipelineWorld) as(t *testing.T, name string) userModels.UserInfo {
	t.Helper()
	ctx := context.Background()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, w.people[name])
	if err != nil || pg == nil {
		t.Fatalf("load %s: %v", name, err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, w.people[name].String())
	if err != nil || gr == nil {
		t.Fatalf("load %s's node: %v", name, err)
	}
	return userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}
}

// post sends in #launch through /po/createPost, answering its status and body.
func (w *pipelineWorld) post(who userModels.UserInfo, html, clientID string) (int, string) {
	raw, _ := json.Marshal(postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: w.channel.String(), HTMLText: html, ClientMsgID: clientID})
	rec := httptest.NewRecorder()
	ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, who)
	postController.CreatePost(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(ctx))
	return rec.Code, rec.Body.String()
}

// send posts in #launch and answers the post's id.
func (w *pipelineWorld) send(t *testing.T, who userModels.UserInfo, html, clientID string) string {
	t.Helper()
	code, body := w.post(who, html, clientID)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	var out struct {
		Data struct {
			Uuid string `json:"uuid"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Data.Uuid == "" {
		t.Fatalf("answer %s: %v", body, err)
	}
	return out.Data.Uuid
}

func (w *pipelineWorld) searchSaw(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range w.indexed {
		if strings.Contains(b, `"index"`) && strings.Contains(b, id) {
			return true
		}
	}
	return false
}

// events is a post's outbox rows as a failure can show them.
func (w *pipelineWorld) events(t *testing.T, post string) string {
	t.Helper()
	rows, err := w.env.PG.Query(`SELECT key, state, array_to_string(sinks_done, ','), attempts, COALESCE(last_error, '') FROM outbox WHERE entities->>'message' = $1 ORDER BY id`, post)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var key, state, done, lastErr string
		var attempts int
		if err := rows.Scan(&key, &state, &done, &attempts, &lastErr); err != nil {
			return err.Error()
		}
		out = append(out, fmt.Sprintf("%s %s done[%s] tries %d %s", key, state, done, attempts, lastErr))
	}
	return strings.Join(out, "; ")
}

func (w *pipelineWorld) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := w.env.PG.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestADurableChannelPostReachesItsSinksOnceAndASecondSendIsTheFirst(t *testing.T) {
	w := newPipelineWorld(t)
	broker := integration.SetupBroker(t)
	ctx := context.Background()
	ada, sam := w.as(t, "ada"), w.as(t, "sam")
	topic := helpers.GetMqttTopicForChannelMessage(w.channel.String())
	app, _ := broker.Connect(t, "c_dev1_"+w.people["sam"].String()[:8]+"_ab12", w.people["sam"].String())
	app.Subscribe(t, topic)

	var mu sync.Mutex
	pushed := map[string]int{}
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		for _, tok := range tokens {
			pushed[data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID]+"|"+tok]++
		}
		mu.Unlock()
	})()
	pushes := func(post, token string) int {
		mu.Lock()
		defer mu.Unlock()
		return pushed[post+"|"+token]
	}

	clientID := "local-" + uuid.NewString()[:8]
	html := fmt.Sprintf(`<p>Launch at 10 <span data-type="mention" data-id="%s@%s">@sam</span></p>`, sam.UserDgraphInfo.Uuid, w.uids["sam"])
	post := w.send(t, ada, html, clientID)

	// Live: the channel's open app has it. No drain runs in this test, so the
	// writer delivered it itself, as soon as it was written.
	if !receives(app, topic, post, 20*time.Second) {
		t.Fatalf("the channel's open app never got the post; its events: %s", w.events(t, post))
	}
	// Its two events (live, and the rest) run to done, every sink having had one.
	waitFor(t, "the post's events to be done", func() bool {
		return w.count(t, `SELECT COUNT(*) FROM outbox WHERE entities->>'message' = $1 AND state = 'done'`, post) == 2
	})
	var done string
	if err := w.env.PG.QueryRow(`SELECT string_agg(array_to_string(sinks_done, ','), ',') FROM outbox WHERE entities->>'message' = $1`, post).Scan(&done); err != nil {
		t.Fatal(err)
	}
	for _, sink := range []string{"post.realtime", "post.search", "post.notify", "post.events"} {
		if !strings.Contains(done, sink) {
			t.Errorf("%s didn't have the event (done: %s)", sink, done)
		}
	}
	if !w.searchSaw(post) {
		t.Error("search never got the post's entry, written as an index")
	}
	waitFor(t, "Sam's push", func() bool { return pushes(post, "tok-sam") > 0 })
	if n := pushes(post, "tok-ada"); n != 0 {
		t.Errorf("Ada was pushed of her own post %d times", n)
	}
	// The ledger holds it, with the id the app gave it.
	if n := w.count(t, `SELECT COUNT(*) FROM messages WHERE id = $1 AND client_msg_id = $2`, post, clientID); n != 1 {
		t.Errorf("%d ledger rows for the post with its client id", n)
	}

	// The same send again after Redis forgot it: the ledger answers the first post.
	if err := redisInit.RedisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	if again := w.send(t, ada, html, clientID); again != post {
		t.Errorf("the second send made %s, want the first post %s", again, post)
	}
	if n := w.count(t, `SELECT COUNT(*) FROM posts WHERE post_channel = $1`, w.channel); n != 1 {
		t.Errorf("%d posts in the channel after the same send twice", n)
	}
	if n := w.count(t, `SELECT COUNT(*) FROM outbox WHERE entities->>'message' = $1`, post); n != 2 {
		t.Errorf("%d events for one post, want its live one and the rest", n)
	}
	if n := w.count(t, `SELECT COUNT(*) FROM outbox WHERE key = $1`, "channel:"+w.channel.String()); n != 1 {
		t.Errorf("%d live events in the channel for one post", n)
	}
	helpers.SettleBackground(10 * time.Second)
	if n := pushes(post, "tok-sam"); n != 1 {
		t.Errorf("Sam was pushed %d times, want once", n)
	}

	// An id too long for the ledger is one Once never took: the post is sent.
	other := w.send(t, ada, "<p>And the slides</p>", strings.Repeat("x", 100))
	if other == post {
		t.Fatal("a new post answered the first")
	}
	if n := w.count(t, `SELECT COUNT(*) FROM messages WHERE id = $1 AND client_msg_id IS NULL`, other); n != 1 {
		t.Errorf("%d ledger rows for the post without an id the ledger keeps", n)
	}
}

func TestADurableChannelPostWhoseWriterDiedIsDecidedByItsNode(t *testing.T) {
	w := newPipelineWorld(t)
	integration.StubMqttClient(t)
	ctx := context.Background()
	ada := w.as(t, "ada")
	key := "channel:" + w.channel.String()
	held := func(post string, sinks ...string) int64 {
		t.Helper()
		tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		id, err := outboxBusiness.Add(ctx, tx, outboxBusiness.Entry{Topic: postBusiness.MessageTopic, Kind: postBusiness.MessageCreated, Key: key,
			Entities: map[string]string{"message": post, "conversation": w.channel.String(), "kind": "channel"}, Sinks: sinks})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return id
	}

	// Its writer died before writing the node: the rows and the event are there, the node isn't.
	lost := uuid.New()
	for _, row := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, []any{lost, w.channel, w.people["ada"]}},
		{`INSERT INTO conversations (id, kind, ref) VALUES ($1, 'channel', $2) ON CONFLICT DO NOTHING`, []any{w.channel, w.channel.String()}},
		{`INSERT INTO messages (id, conversation_id, author_id, client_msg_id, created_at) VALUES ($1, $2, $3, 'local-lost', NOW())`, []any{lost, w.channel, w.people["ada"]}},
	} {
		if _, err := w.env.PG.Exec(row.q, row.args...); err != nil {
			t.Fatalf("%s: %v", row.q, err)
		}
	}
	lostEvent := held(lost.String(), "post.search")

	// Her app tries the send again: it is still being sent, not answered with
	// a post that has no node.
	if code, body := w.post(ada, "<p>lost</p>", "local-lost"); code != http.StatusConflict || !strings.Contains(body, "This message is still being sent") {
		t.Errorf("a retry of a send whose node was never written: %d %s", code, body)
	}

	// Its writer died after writing the node: a post with its node, and its event held.
	draft, err := sendBusiness.PrepareChannelPost(ctx, &ada, &postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: w.channel.String(), HTMLText: "<p>kept</p>"})
	if err != nil {
		t.Fatal(err)
	}
	made, err := draft.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keptEvent := held(made.Uuid, "post.search")
	helpers.SettleBackground(10 * time.Second)
	w.mu.Lock()
	w.indexed = nil // what the post's own write sent
	w.mu.Unlock()

	// Within the writers' time nothing is decided; past it, the drain asks.
	if err := outboxBusiness.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _ := stateOf(t, w.env, keptEvent); state != "held" {
		t.Fatalf("decided within its writer's time: %s", state)
	}
	if _, err := w.env.PG.Exec(`UPDATE outbox SET created_at = NOW() - interval '1 minute' WHERE id IN ($1, $2)`, lostEvent, keptEvent); err != nil {
		t.Fatal(err)
	}
	if err := outboxBusiness.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	helpers.SettleBackground(10 * time.Second)

	if state, _ := stateOf(t, w.env, lostEvent); state != "cancelled" {
		t.Errorf("the lost post's event: %s, want cancelled", state)
	}
	if n := w.count(t, `SELECT COUNT(*) FROM posts WHERE id = $1`, lost) + w.count(t, `SELECT COUNT(*) FROM messages WHERE id = $1`, lost); n != 0 {
		t.Errorf("the lost post's rows are still there (%d)", n)
	}
	if state, done := stateOf(t, w.env, keptEvent); state != "done" || strings.Join(done, ",") != "post.search" {
		t.Errorf("the written post's event: %s %v, want released and done", state, done)
	}
	if !w.searchSaw(made.Uuid) || w.searchSaw(lost.String()) {
		t.Error("search should have the written post again, and never the lost one")
	}

	// Once its rows are taken back, the app's next try is a new post.
	redo := w.send(t, ada, "<p>lost</p>", "local-lost")
	if redo == lost.String() {
		t.Error("the retry answered the post that was taken back")
	}
}
