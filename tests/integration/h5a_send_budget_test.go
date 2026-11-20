//go:build integration

package integration_test

// H5A, chat send: the store calls one message makes. A DM's row and its
// sender's read mark go to Postgres in one statement, and so do a channel
// post's (today two each). The statements are counted on the project's own
// pool through a pgx tracer (pgStatements, taskread_generation_test.go); the
// graph's calls through countingClient (dgraph_budgets_test.go).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"google.golang.org/grpc"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// sentSince is what the project's pool sent (BEGIN and COMMIT included) since
// the last take, one line each. The tracer (pgStatements) and countPostgres,
// which points the pool at it until t ends, are taskread_generation_test.go's.
func sentSince(p *pgStatements) []string {
	sent := p.take()
	for i, s := range sent {
		sent[i] = strings.Join(strings.Fields(s), " ")
	}
	return sent
}

// writing is the statements that insert into any of tables.
func writing(sent []string, tables ...string) (out []string) {
	for _, s := range sent {
		for _, tb := range tables {
			if strings.Contains(s, "INSERT INTO "+tb+" ") {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

func short(sent []string) string {
	var b strings.Builder
	for _, s := range sent {
		b.WriteString("\n  " + s[:min(len(s), 110)])
	}
	return b.String()
}

// dmPair is Maya and Jonas, in Postgres and the graph, and the stores a DM
// needs (search answers every write; MQTT is stubbed).
type dmPair struct {
	env         *integration.Env
	dg          *integration.DgraphEnv
	maya, jonas uuid.UUID
	sender      userModels.UserInfo
	to          *dgraphStruct.DgraphUser
}

func newDMPair(t *testing.T) *dmPair {
	t.Helper()
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() { helpers.SettleBackground(10 * time.Second) })

	maya, jonas := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"maya": maya, "jonas": jonas} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya"},
		{"uid": "_:jonas", "dgraph.type": "User", "user_uuid": jonas.String(), "user_name": "jonas"},
	})
	var sender userModels.UserInfo
	sender.UserPostgresInfo.Id = maya
	sender.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids["maya"], Uuid: maya.String(), UserName: "maya"}
	to := &dgraphStruct.DgraphUser{Uid: uids["jonas"], Uuid: jonas.String(), UserName: "jonas"}
	return &dmPair{env: env, dg: dg, maya: maya, jonas: jonas, sender: sender, to: to}
}

func TestH5ADirectMessageWritesItsRowAndItsSendersReadMarkInOneStatement(t *testing.T) {
	ctx := context.Background()
	p := newDMPair(t)
	env, dg, maya, jonas, sender, to := p.env, p.dg, p.maya, p.jonas, p.sender, p.to
	send := func(html string) *chatAdapter.OutputCreateChatForChat {
		t.Helper()
		created, err := chatBusiness.CreateChat(ctx, &chatAdapter.ChatInfo{TextHtml: html, ToUuid: jonas.String()}, &sender, to, jonas, nil)
		if err != nil || created == nil {
			t.Fatalf("send %q: %+v %v", html, created, err)
		}
		return created
	}
	// The first message opens the conversation; the second is the measured one.
	send("<p>hello</p>")
	helpers.SettleBackground(10 * time.Second)

	pg := countPostgres(t, env.DSN)
	graph := countingClient(t, dg.Endpoint)
	created := send("<p>the numbers for Q4</p>")
	sent, graphCalls := sentSince(pg), graph.n.Load()
	t.Logf("a DM before it returned: %d Postgres statements, %d graph calls:%s", len(sent), graphCalls, short(sent))

	if w := writing(sent, "chats", "last_seen_chat"); len(w) != 1 {
		t.Errorf("the message and its sender's read mark took %d statements, want 1:%s", len(w), short(w))
	}
	if len(sent) > 1 || graphCalls > 1 {
		t.Errorf("a DM took %d Postgres statements and %d graph calls before it returned, want at most 1 and 1", len(sent), graphCalls)
	}
	grouping := helpers.GetGroupingId(maya.String(), jonas.String())
	var seen time.Time
	if err := env.PG.QueryRow(`SELECT user_last_seen FROM last_seen_chat WHERE user_id = $1 AND grp_id = $2`, maya, grouping).Scan(&seen); err != nil {
		t.Fatalf("Maya's read mark: %v", err)
	}
	if d := seen.Sub(created.ChatCreated); d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("Maya has read up to %v, want her message's time %v", seen, created.ChatCreated)
	}
	var stored int
	if err := env.PG.QueryRow(`SELECT count(*) FROM chats WHERE id = $1`, created.Uuid).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("the message is stored %d times (%v), want once", stored, err)
	}
}

func TestH5AChannelPostWritesItsRowAndItsSendersReadMarkInOneStatement(t *testing.T) {
	ctx := context.Background()
	w := newPipelineWorld(t)
	integration.StubMqttClient(t)
	ada := w.as(t, "ada")
	post := func(html string) (*sendBusiness.ChannelPost, error) {
		return sendBusiness.PrepareChannelPost(ctx, &ada, &postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: w.channel.String(), HTMLText: html})
	}
	first, err := post("<p>first</p>")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	helpers.SettleBackground(10 * time.Second)
	draft, err := post("<p>the launch is on Monday</p>")
	if err != nil {
		t.Fatal(err)
	}

	pg := countPostgres(t, w.env.DSN)
	before := time.Now()
	made, err := draft.Commit(ctx)
	sent := sentSince(pg)
	if err != nil || made == nil {
		t.Fatalf("post: %+v %v", made, err)
	}
	t.Logf("a channel post before it returned: %d Postgres statements:%s", len(sent), short(sent))

	if wr := writing(sent, "posts", "last_seen_channel"); len(wr) != 1 {
		t.Errorf("the post and its sender's read mark took %d statements, want 1:%s", len(wr), short(wr))
	}
	if len(sent) > 1 {
		t.Errorf("a channel post took %d Postgres statements before it returned, want at most 1", len(sent))
	}
	var seen time.Time
	if err := w.env.PG.QueryRow(`SELECT user_last_seen FROM last_seen_channel WHERE user_id = $1 AND channel_id = $2`, w.people["ada"], w.channel).Scan(&seen); err != nil {
		t.Fatalf("Ada's read mark: %v", err)
	}
	if seen.Before(before.Add(-time.Millisecond)) {
		t.Errorf("Ada has read up to %v, before her post at %v", seen, before)
	}
}

// outboxWrites is the statements of sent that add outbox events, and those
// that release them.
func outboxWrites(sent []string) (added, released []string) {
	for _, s := range sent {
		switch {
		case strings.HasPrefix(s, "INSERT INTO outbox "):
			added = append(added, s)
		case strings.HasPrefix(s, "UPDATE outbox") && strings.Contains(s, "SET state = 'ready'"):
			released = append(released, s)
		}
	}
	return added, released
}

// H5A A2-6: a durable DM's events (its live one, carrying whom the new
// conversation is for, and the rest) are added in one statement and
// released in one (today one each).
func TestH5ADurableDMAddsAndReleasesItsEventsInOneStatementEach(t *testing.T) {
	w := newChatWorld(t)
	integration.StubMqttClient(t)
	ada := w.as(t, "ada")
	pg := countPostgres(t, w.env.DSN)
	w.send(t, ada, chatController.CreateChat, chatAdapter.ChatInfo{ToUuid: w.people["sam"].String(), TextHtml: "<p>the numbers for Q4</p>", ClientMsgID: "local-" + uuid.NewString()[:8]})
	sent := sentSince(pg)
	added, released := outboxWrites(sent)
	t.Logf("a durable DM's first message before it answered: %d Postgres statements:%s", len(sent), short(sent))
	if len(added) != 1 || len(released) != 1 {
		t.Errorf("its events took %d statements to add and %d to release, want 1 and 1:%s", len(added), len(released), short(append(added, released...)))
	}
}

// graphQueries records the query of every graph call made through the
// project's client.
type graphQueries struct {
	mu   sync.Mutex
	sent []string
}

func (g *graphQueries) intercept(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	q := method
	if r, ok := req.(*api.Request); ok {
		q = strings.Join(strings.Fields(r.Query), " ")
	}
	g.mu.Lock()
	g.sent = append(g.sent, q)
	g.mu.Unlock()
	return invoker(ctx, method, req, reply, cc, opts...)
}

// H5A A1-15: a DM reads its recipient once, without their channels, emoji
// or hobbies (today the full profile), and still refuses one who is gone.
func TestH5ADMReadsItsRecipientWithoutTheirChannels(t *testing.T) {
	ctx := context.Background()
	p := newDMPair(t)
	g := &graphQueries{}
	throughInterceptor(t, p.dg.Endpoint, g.intercept)
	dm, err := sendBusiness.PrepareDirectMessage(ctx, &p.sender, &chatAdapter.ChatInfo{TextHtml: "<p>hi</p>", ToUuid: p.jonas.String()})
	if err != nil || dm == nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Logf("a DM's prepare: %d graph calls:%s", len(g.sent), short(g.sent))
	for _, q := range g.sent {
		if strings.Contains(q, "user_channels") || strings.Contains(q, "user_hobbies") {
			t.Errorf("the recipient was read with what a DM never uses: %s", q[:min(len(q), 200)])
		}
	}
	if len(g.sent) != 1 {
		t.Errorf("%d graph calls, want 1 (the recipient)", len(g.sent))
	}
	p.dg.Mutate(t, []map[string]any{{"uid": p.to.Uid, "user_deleted_at": time.Now().UTC().Format(time.RFC3339)}})
	if _, err := sendBusiness.PrepareDirectMessage(ctx, &p.sender, &chatAdapter.ChatInfo{TextHtml: "<p>hi</p>", ToUuid: p.jonas.String()}); err == nil {
		t.Errorf("a DM to a deactivated person was let through")
	}
}

// TestH5AGroupMessageWritesItsRowAndReadMarkInOneStatementAtAnySize: a group
// message writes its row and its sender's read mark in one statement, and
// marks no one else, in a group of 3 as in a group of 50.
func TestH5AGroupMessageWritesItsRowAndReadMarkInOneStatementAtAnySize(t *testing.T) {
	ctx := context.Background()
	p := newDMPair(t)
	pg := countPostgres(t, p.env.DSN)
	graph := countingClient(t, p.dg.Endpoint)
	// groupOf is Maya, Jonas and n-2 more people, in Postgres and the graph.
	groupOf := func(n int) []*dgraphStruct.DgraphUser {
		t.Helper()
		ids := make([]uuid.UUID, n-2)
		nodes := make([]map[string]any, n-2)
		for i := range ids {
			ids[i] = uuid.New()
			name := fmt.Sprintf("member%d-%d", n, i)
			if _, err := p.env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
				VALUES ($1, $2, $3, $4, NOW(), NOW())`, ids[i], ids[i].String()+"@example.test", name, name); err != nil {
				t.Fatal(err)
			}
			nodes[i] = map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": ids[i].String(), "user_name": name}
		}
		uids := p.dg.Mutate(t, nodes)
		people := []*dgraphStruct.DgraphUser{&p.sender.UserDgraphInfo, p.to}
		for i, id := range ids {
			name := nodes[i]["user_name"].(string)
			people = append(people, &dgraphStruct.DgraphUser{Uid: uids[name], Uuid: id.String(), UserName: name})
		}
		return people
	}
	for _, n := range []int{3, 50} {
		people := groupOf(n)
		// The first message opens the group; the second is the measured one.
		opened := &chatAdapter.ChatInfo{TextHtml: "<p>hello all</p>"}
		if created, err := chatBusiness.CreateChatForGroup(ctx, opened, &p.sender, nil, people); err != nil || created == nil || opened.GrpUuid == "" {
			t.Fatalf("group of %d: opening it: %+v %v", n, created, err)
		}
		helpers.SettleBackground(10 * time.Second)
		pg.take()
		graph.n.Store(0)
		created, err := chatBusiness.CreateChatForGroup(ctx, &chatAdapter.ChatInfo{TextHtml: "<p>the numbers for Q4</p>", GrpUuid: opened.GrpUuid}, &p.sender, nil, people)
		sent, graphCalls := sentSince(pg), graph.n.Load()
		if err != nil || created == nil {
			t.Fatalf("group of %d: send: %+v %v", n, created, err)
		}
		t.Logf("a group of %d's message before it returned: %d Postgres statements, %d graph calls:%s", n, len(sent), graphCalls, short(sent))
		if w := writing(sent, "chats", "last_seen_chat"); len(w) != 1 {
			t.Errorf("group of %d: the message and its sender's read mark took %d statements, want 1:%s", n, len(w), short(w))
		}
		if len(sent) > 1 || graphCalls > 1 {
			t.Errorf("group of %d: the message took %d Postgres statements and %d graph calls before it returned, want at most 1 and 1", n, len(sent), graphCalls)
		}
		var marks int
		if err := p.env.PG.QueryRow(`SELECT count(*) FROM last_seen_chat WHERE grp_id = $1 AND user_id <> $2 AND user_last_seen >= $3`,
			opened.GrpUuid, p.maya, created.ChatCreated).Scan(&marks); err != nil || marks != 0 {
			t.Errorf("group of %d: %d other people were marked as having read the message (%v), want none", n, marks, err)
		}
	}
}

// TestH5ADMSearchEntryShowsItsRecipientAsBefore: the light recipient read
// keeps what a sync DM's search entry shows of its recipient: their profile
// picture's key, and their name from their email when they have no other.
func TestH5ADMSearchEntryShowsItsRecipientAsBefore(t *testing.T) {
	ctx := context.Background()
	p := newDMPair(t)
	var mu sync.Mutex
	var bodies strings.Builder
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, err := io.ReadAll(r.Body); err == nil {
			mu.Lock()
			bodies.Write(b)
			mu.Unlock()
		}
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	quiet := uuid.New()
	if _, err := p.env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'quietperson7@example.test', $2, $3, NOW(), NOW())`, quiet, quiet.String(), quiet.String()); err != nil {
		t.Fatal(err)
	}
	p.dg.Mutate(t, []map[string]any{{"uid": "_:quiet", "dgraph.type": "User", "user_uuid": quiet.String(),
		"user_email_id": "quietperson7@example.test", "user_profile_object_key": "profiles/quiet-7.png"}})

	dm, err := sendBusiness.PrepareDirectMessage(ctx, &p.sender, &chatAdapter.ChatInfo{TextHtml: "<p>are you there?</p>", ToUuid: quiet.String()})
	if err != nil || dm == nil {
		t.Fatalf("prepare: %v", err)
	}
	if made, err := dm.Commit(ctx); err != nil || made == nil {
		t.Fatalf("send: %+v %v", made, err)
	}
	helpers.SettleBackground(10 * time.Second)
	mu.Lock()
	sent := bodies.String()
	mu.Unlock()
	for _, want := range []string{`"user_profile_object_key":"profiles/quiet-7.png"`, `"user_name":"quietperson7"`} {
		if !strings.Contains(sent, want) {
			t.Errorf("the DM's search entry doesn't show its recipient's %s", want)
		}
	}
}
