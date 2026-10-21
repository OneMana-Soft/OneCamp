//go:build integration
// +build integration

package integration_test

// DMs and group messages the durable way (chat A2, MESSAGE_PIPELINE=dm,group),
// sent through /dm/createChat and /groupChat/createChat, against real
// Postgres 12, Redis, Dgraph and the broker: a first DM's people are told the
// conversation is theirs and get the message, search gets its entry, the
// other person is pushed once, every sink has its event, and a second send
// with its client id answers the first message even after Redis forgot it; a
// new group's first message reaches its people the same way; and a message
// whose writer died is decided by whether its node was written, a retry
// before that being told it is still being sent.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestADurableDM|TestADurableGroup|TestADurableChat' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	messagePipeline "github.com/akashc777/OneCamp/business/MessagePipeline"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// chatWorld is what a durable DM or group message needs: the stores, Ada, Sam
// and Jonas, and what search was sent and who was pushed.
type chatWorld struct {
	env     *integration.Env
	people  map[string]uuid.UUID
	uids    map[string]string
	mu      sync.Mutex
	indexed []string
	pushed  map[string]int // message|token
}

func newChatWorld(t *testing.T) *chatWorld {
	t.Helper()
	t.Setenv(messagePipeline.SurfacesEnv, messagePipeline.SurfaceDM+","+messagePipeline.SurfaceGroup)
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
	w := &chatWorld{env: env, people: map[string]uuid.UUID{}, uids: map[string]string{}, pushed: map[string]int{}}
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
	t.Cleanup(firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		w.mu.Lock()
		for _, tok := range tokens {
			w.pushed[data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID]+"|"+tok]++
		}
		w.mu.Unlock()
	}))

	var nodes []map[string]any
	for _, name := range []string{"ada", "sam", "jonas"} {
		id := uuid.New()
		w.people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`, id, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": "0001-01-01T00:00:00Z"})
	}
	got := dg.Mutate(t, nodes)
	for _, name := range []string{"ada", "sam", "jonas"} {
		w.uids[name] = got[name]
	}
	return w
}

func (w *chatWorld) as(t *testing.T, name string) userModels.UserInfo {
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

// post sends through a route as who, answering its status and body.
func (w *chatWorld) post(who userModels.UserInfo, route http.HandlerFunc, in chatAdapter.ChatInfo) (int, string) {
	raw, _ := json.Marshal(in)
	rec := httptest.NewRecorder()
	ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, who)
	route(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(ctx))
	return rec.Code, rec.Body.String()
}

// send sends and answers the message's id.
func (w *chatWorld) send(t *testing.T, who userModels.UserInfo, route http.HandlerFunc, in chatAdapter.ChatInfo) string {
	t.Helper()
	code, body := w.post(who, route, in)
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

func (w *chatWorld) searchSaw(id string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range w.indexed {
		if strings.Contains(b, `"index"`) && strings.Contains(b, id) {
			return b
		}
	}
	return ""
}

func (w *chatWorld) pushes(message, token string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pushed[message+"|"+token]
}

func (w *chatWorld) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := w.env.PG.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// sinksDone is every sink that had one of a message's events, sorted.
func (w *chatWorld) sinksDone(t *testing.T, message string) string {
	t.Helper()
	var done string
	if err := w.env.PG.QueryRow(`SELECT COALESCE(string_agg(array_to_string(sinks_done, ','), ','), '') FROM outbox WHERE entities->>'message' = $1`, message).Scan(&done); err != nil {
		t.Fatal(err)
	}
	names := strings.Split(done, ",")
	sort.Strings(names)
	return strings.Join(names, ",")
}

// heard is the order a person's app heard of things in, until wait: "access"
// for being told a conversation is theirs, "message" for the live message.
// Anything else (a desktop notice, a read mark) is passed over.
func heard(c *integration.BrokerClient, message string, wait time.Duration) []string {
	var order []string
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		m, ok := c.Next(time.Until(deadline))
		if !ok {
			break
		}
		var msg struct {
			Type int8 `json:"type"`
		}
		_ = json.Unmarshal(m.Payload, &msg)
		switch {
		case msg.Type == mqttStruct.MESSAGE_CONVERSATION_ACCESS:
			order = append(order, "access")
		case msg.Type == mqttStruct.MESSAGE_CHAT && strings.Contains(string(m.Payload), message):
			order = append(order, "message")
		}
	}
	return order
}

func TestADurableDMReachesItsPeopleOnceAndASecondSendIsTheFirst(t *testing.T) {
	w := newChatWorld(t)
	broker := integration.SetupBroker(t)
	ctx := context.Background()
	ada := w.as(t, "ada")
	sam := w.people["sam"].String()
	grp := helpers.GetGroupingId(w.people["ada"].String(), sam)
	app, _ := broker.Connect(t, "c_dev1_"+sam[:8]+"_ab12", sam)
	app.Subscribe(t, helpers.GetMqttTopicForUserActivity(sam), helpers.GetMqttTopicForDmMessage(grp))

	clientID := "local-" + uuid.NewString()[:8]
	html := fmt.Sprintf(`<p>the numbers for Q4, <span data-type="mention" data-id="%s@%s">@sam</span></p>`, sam, w.uids["sam"])
	in := chatAdapter.ChatInfo{ToUuid: sam, TextHtml: html, ClientMsgID: clientID}
	chat := w.send(t, ada, chatController.CreateChat, in)

	// No drain runs in this test: what reaches Sam's app the writer delivered
	// itself, and his app is told the DM is his before its first message (one
	// that mentions him: a mention is a list the sinks' read must take).
	if got := strings.Join(heard(app, chat, 15*time.Second), ","); got != "access,message" {
		t.Errorf("Sam's app heard %q, want access, then the message", got)
	}
	waitFor(t, "the message's events to be done", func() bool {
		return w.count(t, `SELECT COUNT(*) FROM outbox WHERE entities->>'message' = $1 AND state = 'done'`, chat) == 2
	})
	if got := w.sinksDone(t, chat); got != "chat.access,chat.events,chat.notify,chat.realtime,chat.search" && got != "chat.access,chat.embed,chat.events,chat.notify,chat.realtime,chat.search" {
		t.Errorf("the sinks that had it: %s", got)
	}
	entry := w.searchSaw(chat)
	if entry == "" || !strings.Contains(entry, sam) || !strings.Contains(entry, w.people["ada"].String()) {
		t.Errorf("search got %q: an index with both people", entry)
	}
	waitFor(t, "Sam's push", func() bool { return w.pushes(chat, "tok-sam") > 0 })
	if n := w.pushes(chat, "tok-ada"); n != 0 {
		t.Errorf("Ada was pushed of her own message %d times", n)
	}
	if n := w.count(t, `SELECT COUNT(*) FROM messages WHERE id = $1 AND client_msg_id = $2`, chat, clientID); n != 1 {
		t.Errorf("%d ledger rows for the message with its client id", n)
	}

	// The same send again after Redis forgot it: the ledger answers the first message.
	if err := redisInit.RedisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	if again := w.send(t, ada, chatController.CreateChat, in); again != chat {
		t.Errorf("the second send made %s, want the first message %s", again, chat)
	}
	if n := w.count(t, `SELECT COUNT(*) FROM chats WHERE grp_id = $1`, grp); n != 1 {
		t.Errorf("%d messages in the DM after the same send twice", n)
	}
	if n := w.count(t, `SELECT COUNT(*) FROM outbox WHERE entities->>'message' = $1`, chat); n != 2 {
		t.Errorf("%d events for one message", n)
	}
	helpers.SettleBackground(10 * time.Second)
	if n := w.pushes(chat, "tok-sam"); n != 1 {
		t.Errorf("Sam was pushed %d times, want once", n)
	}

	// A later DM: no access event, the message only.
	next := w.send(t, ada, chatController.CreateChat, chatAdapter.ChatInfo{ToUuid: sam, TextHtml: "<p>and the slides</p>"})
	if got := strings.Join(heard(app, next, 8*time.Second), ","); got != "message" {
		t.Errorf("Sam's app heard %q of a later DM, want the message only", got)
	}
}

func TestADurableGroupsFirstMessageReachesItsPeople(t *testing.T) {
	w := newChatWorld(t)
	broker := integration.SetupBroker(t)
	ada := w.as(t, "ada")
	ids := []string{w.people["ada"].String(), w.people["sam"].String(), w.people["jonas"].String()}
	grp, err := helpers.GenerateGroupID(ids)
	if err != nil {
		t.Fatal(err)
	}
	jonas := w.people["jonas"].String()
	app, _ := broker.Connect(t, "c_dev1_"+jonas[:8]+"_cd34", jonas)
	app.Subscribe(t, helpers.GetMqttTopicForUserActivity(jonas), helpers.GetMqttTopicForDmMessage(grp))

	// A new group's people are named by their graph uids, as the app names them.
	graphIDs := []string{w.uids["ada"], w.uids["sam"], w.uids["jonas"]}
	chat := w.send(t, ada, chatController.CreateGroupChat, chatAdapter.ChatInfo{Participants: graphIDs, TextHtml: "<p>welcome both</p>", ClientMsgID: "local-grp-1"})
	if got := strings.Join(heard(app, chat, 15*time.Second), ","); got != "access,message" {
		t.Errorf("Jonas's app heard %q, want access, then the message", got)
	}
	waitFor(t, "the message's events to be done", func() bool {
		return w.count(t, `SELECT COUNT(*) FROM outbox WHERE entities->>'message' = $1 AND state = 'done'`, chat) == 2
	})
	entry := w.searchSaw(chat)
	for _, id := range ids {
		if !strings.Contains(entry, id) {
			t.Errorf("search's entry leaves out %s: %q", id, entry)
		}
	}
	waitFor(t, "the others' pushes", func() bool { return w.pushes(chat, "tok-sam") > 0 && w.pushes(chat, "tok-jonas") > 0 })
	if n := w.count(t, `SELECT COUNT(*) FROM chats WHERE grp_id = $1`, grp); n != 1 {
		t.Errorf("%d messages in the new group", n)
	}
}

func TestADurableChatWhoseWriterDiedIsDecidedByItsNode(t *testing.T) {
	w := newChatWorld(t)
	integration.StubMqttClient(t)
	ctx := context.Background()
	ada := w.as(t, "ada")
	sam := w.people["sam"].String()
	grp := helpers.GetGroupingId(w.people["ada"].String(), sam)
	held := func(chat string, sinks ...string) int64 {
		t.Helper()
		tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		id, err := outboxBusiness.Add(ctx, tx, outboxBusiness.Entry{Topic: chatBusiness.MessageTopic, Kind: chatBusiness.MessageCreated, Key: "chat:" + chat,
			Entities: map[string]string{"message": chat, "conversation": grp, "kind": "dm"}, Sinks: sinks})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return id
	}

	// Its writer died before writing the node: the rows (and the DM's
	// conversation row, which nothing else has) and the event are there.
	lost := uuid.New()
	conversation := uuid.New()
	for _, row := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO chats (id, created_by, grp_id) VALUES ($1, $2, $3)`, []any{lost, w.people["ada"], grp}},
		{`INSERT INTO conversations (id, kind, ref) VALUES ($1, 'dm', $2)`, []any{conversation, grp}},
		{`INSERT INTO messages (id, conversation_id, author_id, client_msg_id, created_at) VALUES ($1, $2, $3, 'local-lost', NOW())`, []any{lost, conversation, w.people["ada"]}},
	} {
		if _, err := w.env.PG.Exec(row.q, row.args...); err != nil {
			t.Fatalf("%s: %v", row.q, err)
		}
	}
	lostEvent := held(lost.String(), "chat.search")

	// Her app tries the send again: it is still being sent.
	if code, body := w.post(ada, chatController.CreateChat, chatAdapter.ChatInfo{ToUuid: sam, TextHtml: "<p>lost</p>", ClientMsgID: "local-lost"}); code != http.StatusConflict || !strings.Contains(body, "This message is still being sent") {
		t.Errorf("a retry of a send whose node was never written: %d %s", code, body)
	}

	// Past the writer's time, the drain asks.
	if _, err := w.env.PG.Exec(`UPDATE outbox SET created_at = NOW() - interval '1 minute' WHERE id = $1`, lostEvent); err != nil {
		t.Fatal(err)
	}
	if err := outboxBusiness.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	helpers.SettleBackground(10 * time.Second)
	if state, _ := stateOf(t, w.env, lostEvent); state != "cancelled" {
		t.Errorf("the lost message's event: %s, want cancelled", state)
	}
	if n := w.count(t, `SELECT COUNT(*) FROM chats WHERE id = $1`, lost) + w.count(t, `SELECT COUNT(*) FROM messages WHERE id = $1`, lost) + w.count(t, `SELECT COUNT(*) FROM conversations WHERE id = $1`, conversation); n != 0 {
		t.Errorf("the lost message's rows are still there (%d)", n)
	}
	if w.searchSaw(lost.String()) != "" {
		t.Error("search got the lost message")
	}

	// Once its rows are taken back, the app's next try is a new message, the
	// DM's first.
	redo := w.send(t, ada, chatController.CreateChat, chatAdapter.ChatInfo{ToUuid: sam, TextHtml: "<p>lost</p>", ClientMsgID: "local-lost"})
	if redo == lost.String() {
		t.Error("the retry answered the message that was taken back")
	}
	waitFor(t, "the new message's events to be done", func() bool {
		return w.count(t, `SELECT COUNT(*) FROM outbox WHERE entities->>'message' = $1 AND state = 'done'`, redo) == 2
	})
	var opened string
	if err := w.env.PG.QueryRow(`SELECT COALESCE(entities->>'opened', '') FROM outbox WHERE entities->>'message' = $1 AND key LIKE 'dm:%'`, redo).Scan(&opened); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opened, sam) {
		t.Errorf("the DM's first message says it was opened for %q", opened)
	}
}
