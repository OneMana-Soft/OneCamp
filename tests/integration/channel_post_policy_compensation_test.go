//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A channel's post policy lives in two stores: its Postgres row and its graph
// node, and the check made when someone posts reads the graph. A change whose
// graph write failed used to leave Postgres holding the new policy and the
// graph the old one, so who may post depended on which store a path read.

// channelFacts is what both stores hold about a channel and must agree on.
type channelFacts struct {
	PostPolicy string
	Private    bool
	Deleted    bool
}

// channelFactsDiffer names each fact the two stores disagree on: post_policy,
// ch_private and deleted_at. A graph node without a policy has the column's
// default, everyone. Pure.
func channelFactsDiffer(pg, graph channelFacts) []string {
	policy := func(p string) string {
		if p == "" {
			return channelBusiness.ChannelPostPolicyEveryone
		}
		return p
	}
	var differ []string
	if policy(pg.PostPolicy) != policy(graph.PostPolicy) {
		differ = append(differ, "post_policy")
	}
	if pg.Private != graph.Private {
		differ = append(differ, "ch_private")
	}
	if pg.Deleted != graph.Deleted {
		differ = append(differ, "deleted_at")
	}
	return differ
}

// channelStoreFacts reads a channel's facts from its Postgres row and from its
// graph node. A graph node is archived when its deletion time is after 1970,
// as every live-channel filter reads it.
func channelStoreFacts(t *testing.T, env *integration.Env, dg *integration.DgraphEnv, channel uuid.UUID) (pg, graph channelFacts) {
	t.Helper()
	if err := env.PG.QueryRow(`SELECT post_policy, ch_private, deleted_at IS NOT NULL AND deleted_at > '1970-01-02'
		FROM channels WHERE id = $1`, channel).Scan(&pg.PostPolicy, &pg.Private, &pg.Deleted); err != nil {
		t.Fatalf("read the channel's row: %v", err)
	}
	var out struct {
		Ch []struct {
			PostPolicy string     `json:"ch_post_policy"`
			Private    bool       `json:"ch_private"`
			DeletedAt  *time.Time `json:"ch_deleted_at"`
		} `json:"ch"`
	}
	raw := dg.Query(t, fmt.Sprintf(`{ ch(func: eq(ch_uuid, %q)) { ch_post_policy ch_private ch_deleted_at } }`, channel.String()))
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Ch) != 1 {
		t.Fatalf("read the channel's node: %v %s", err, raw)
	}
	node := out.Ch[0]
	graph = channelFacts{PostPolicy: node.PostPolicy, Private: node.Private,
		Deleted: node.DeletedAt != nil && node.DeletedAt.After(time.Unix(86400, 0))}
	return pg, graph
}

// postPolicyFixture is a live channel whose row and graph node both hold the
// policy everyone, with the stores a policy change writes set up.
func postPolicyFixture(t *testing.T) (context.Context, *integration.Env, *integration.DgraphEnv, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	channel := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'announcements', false)`, channel); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(),
		"ch_name": "announcements", "ch_private": false, "ch_post_policy": channelBusiness.ChannelPostPolicyEveryone})
	return ctx, env, dg, channel
}

// cacheReader is the graph uid whose basic-info entry the tests cache for a
// channel: the entry the post-time check reads.
const cacheReader = "0x1"

// warmChannelCache reads the channel the way the post-time check does, which
// caches it.
func warmChannelCache(t *testing.T, ctx context.Context, channel uuid.UUID) {
	t.Helper()
	if _, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, channel, cacheReader); err != nil {
		t.Fatalf("read the channel: %v", err)
	}
	if !channelCached(t, ctx, channel) {
		t.Fatal("the fixture is wrong: reading the channel didn't cache it, so a kept cache would prove nothing")
	}
}

// channelCached answers whether the channel's basic-info entry is cached.
func channelCached(t *testing.T, ctx context.Context, channel uuid.UUID) bool {
	t.Helper()
	var entry map[string]any
	found, err := redisStore.GetJSON(ctx, registry.ChannelBasicInfo, []string{channel.String(), cacheReader}, &entry)
	if err != nil {
		t.Fatalf("read the channel's cache entry: %v", err)
	}
	return found
}

// hangingGraph is a graph client whose queries wait until their ctx ends, so a
// test can hold a change inside its graph write. Each query that starts sends
// on started; the client's other calls go to an address that answers nothing.
type hangingGraph struct {
	api.DgraphClient
	started chan struct{}
}

func (h *hangingGraph) Query(ctx context.Context, _ *api.Request, _ ...grpc.CallOption) (*api.Response, error) {
	select {
	case h.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// hangGraph points the graph client at a hangingGraph until restore runs (or
// the test ends). A write already started keeps the hanging client; one that
// starts after restore reaches the real graph.
func hangGraph(t *testing.T) (started <-chan struct{}, restore func()) {
	t.Helper()
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("a graph client with nowhere to go: %v", err)
	}
	h := &hangingGraph{DgraphClient: api.NewDgraphClient(conn), started: make(chan struct{}, 4)}
	live := dgraphInit.DgraphClient
	dgraphInit.DgraphClient = dgo.NewDgraphClient(h)
	var once sync.Once
	restore = func() {
		once.Do(func() {
			dgraphInit.DgraphClient = live
			_ = conn.Close()
		})
	}
	// The put-back is spelled out in the cleanup, where
	// TestTheTestsPutBackWhatTheyChange reads it; when the test already ran
	// restore, the second assignment changes nothing.
	t.Cleanup(func() {
		restore()
		dgraphInit.DgraphClient = live
	})
	return h.started, restore
}

// waitStarted waits for a graph write to reach the hanging client.
func waitStarted(t *testing.T, started <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never reached the graph", what)
	}
}

// changeAsync starts a policy change on ctx and answers its error.
func changeAsync(ctx context.Context, channel uuid.UUID, policy string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- channelBusiness.SetChannelPostPolicy(ctx, channel, policy) }()
	return done
}

func TestAFailedGraphWriteLeavesAChannelsPostPolicyWhereItWas(t *testing.T) {
	ctx, env, dg, channel := postPolicyFixture(t)
	warmChannelCache(t, ctx, channel)

	t.Run("the graph write fails", func(t *testing.T) {
		dg.Break(t)
		if err := channelBusiness.SetChannelPostPolicy(ctx, channel, channelBusiness.ChannelPostPolicyAdminsOnly); err == nil {
			t.Fatal("the graph write failed, so the change must answer an error")
		}
	})
	pg, graph := channelStoreFacts(t, env, dg, channel)
	if pg.PostPolicy != channelBusiness.ChannelPostPolicyEveryone {
		t.Errorf("Postgres kept the policy of a change that failed: %q, want %q", pg.PostPolicy, channelBusiness.ChannelPostPolicyEveryone)
	}
	if differ := channelFactsDiffer(pg, graph); len(differ) > 0 {
		t.Errorf("after the failed change the stores disagree on %v (postgres %+v, graph %+v)", differ, pg, graph)
	}
	if !channelCached(t, ctx, channel) {
		t.Error("a change that changed nothing dropped the channel's cache")
	}

	t.Run("a change that lands reaches both stores", func(t *testing.T) {
		if err := channelBusiness.SetChannelPostPolicy(ctx, channel, channelBusiness.ChannelPostPolicyAdminsOnly); err != nil {
			t.Fatal(err)
		}
		pg, graph := channelStoreFacts(t, env, dg, channel)
		if pg.PostPolicy != channelBusiness.ChannelPostPolicyAdminsOnly {
			t.Errorf("Postgres holds %q, want %q", pg.PostPolicy, channelBusiness.ChannelPostPolicyAdminsOnly)
		}
		if differ := channelFactsDiffer(pg, graph); len(differ) > 0 {
			t.Errorf("the stores disagree on %v (postgres %+v, graph %+v)", differ, pg, graph)
		}
	})

	t.Run("an archived channel's policy can't be changed", func(t *testing.T) {
		gone := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private, deleted_at) VALUES ($1, 'old', false, NOW())`, gone); err != nil {
			t.Fatal(err)
		}
		if err := channelBusiness.SetChannelPostPolicy(ctx, gone, channelBusiness.ChannelPostPolicyAdminsOnly); err == nil {
			t.Fatal("an archived channel took a new post policy")
		}
	})
}

// Two managers set the same policy, and the first one's graph write fails
// after the second has asked (the first closed the tab mid-write). The second
// change must land in both stores, and the first one's failure must never put
// back a policy over it.
func TestASecondPostPolicyChangeOutlivesTheFirstOnesFailedGraphWrite(t *testing.T) {
	ctx, env, dg, channel := postPolicyFixture(t)
	started, restore := hangGraph(t)
	aCtx, cancelA := context.WithCancel(ctx)
	defer cancelA()

	a := changeAsync(aCtx, channel, channelBusiness.ChannelPostPolicyAdminsOnly)
	waitStarted(t, started, "the first change's graph write")
	restore()
	b := changeAsync(ctx, channel, channelBusiness.ChannelPostPolicyAdminsOnly)
	var bErr error
	bDone := false
	select {
	case bErr = <-b:
		bDone = true
	case <-time.After(2 * time.Second): // the second change waits on the first one's row
	}
	cancelA()
	if err := <-a; err == nil {
		t.Fatal("the first change's graph write failed, so it must answer an error")
	}
	if !bDone {
		bErr = <-b
	}
	if bErr != nil {
		t.Fatalf("the second change failed: %v", bErr)
	}

	pg, graph := channelStoreFacts(t, env, dg, channel)
	if pg.PostPolicy != channelBusiness.ChannelPostPolicyAdminsOnly {
		t.Errorf("Postgres holds %q after the second change landed, want %q", pg.PostPolicy, channelBusiness.ChannelPostPolicyAdminsOnly)
	}
	if differ := channelFactsDiffer(pg, graph); len(differ) > 0 {
		t.Errorf("the first change's failure left the stores disagreeing on %v (postgres %+v, graph %+v)", differ, pg, graph)
	}
}

// During a graph outage two changes in a row both fail. Neither reached the
// graph, so Postgres must end on the policy it started with, not on the first
// change's, which no one was told had worked.
func TestTwoPostPolicyChangesWhoseGraphWritesFailLeaveBothStoresAsTheyWere(t *testing.T) {
	ctx, env, dg, channel := postPolicyFixture(t)
	started, restore := hangGraph(t)
	aCtx, cancelA := context.WithCancel(ctx)
	defer cancelA()
	bCtx, cancelB := context.WithCancel(ctx)
	defer cancelB()

	a := changeAsync(aCtx, channel, channelBusiness.ChannelPostPolicyAdminsOnly)
	waitStarted(t, started, "the first change's graph write")
	b := changeAsync(bCtx, channel, channelBusiness.ChannelPostPolicyEveryone)
	select {
	case <-started: // the second change reached the graph: nothing held the row
	case <-time.After(2 * time.Second):
	}
	cancelA()
	if err := <-a; err == nil {
		t.Fatal("the first change's graph write failed, so it must answer an error")
	}
	cancelB()
	if err := <-b; err == nil {
		t.Fatal("the second change's graph write failed, so it must answer an error")
	}
	restore()

	pg, graph := channelStoreFacts(t, env, dg, channel)
	if pg.PostPolicy != channelBusiness.ChannelPostPolicyEveryone {
		t.Errorf("Postgres holds %q after two failed changes, want the policy it started with, %q", pg.PostPolicy, channelBusiness.ChannelPostPolicyEveryone)
	}
	if differ := channelFactsDiffer(pg, graph); len(differ) > 0 {
		t.Errorf("two failed changes left the stores disagreeing on %v (postgres %+v, graph %+v)", differ, pg, graph)
	}
}

// A graph that never answers holds a change's row for its bounded graph write
// (3 s) and no longer; the change then rolls back: Postgres keeps its policy,
// and the cache, which nothing changed, is kept.
func TestAPostPolicyGraphWriteThatHangsRollsBackWithinItsBound(t *testing.T) {
	ctx, env, dg, channel := postPolicyFixture(t)
	warmChannelCache(t, ctx, channel)
	_, restore := hangGraph(t)
	aCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	began := time.Now()
	err := channelBusiness.SetChannelPostPolicy(aCtx, channel, channelBusiness.ChannelPostPolicyAdminsOnly)
	took := time.Since(began)
	restore()
	if err == nil {
		t.Fatal("the graph never answered, so the change must answer an error")
	}
	if took > 5*time.Second {
		t.Errorf("the change waited %v on a graph that never answered; its graph write is bounded at 3 s", took.Round(time.Second))
	}
	pg, graph := channelStoreFacts(t, env, dg, channel)
	if pg.PostPolicy != channelBusiness.ChannelPostPolicyEveryone {
		t.Errorf("Postgres kept the policy of a change that timed out: %q, want %q", pg.PostPolicy, channelBusiness.ChannelPostPolicyEveryone)
	}
	if differ := channelFactsDiffer(pg, graph); len(differ) > 0 {
		t.Errorf("after the timed-out change the stores disagree on %v (postgres %+v, graph %+v)", differ, pg, graph)
	}
	if !channelCached(t, ctx, channel) {
		t.Error("a change that rolled back dropped the channel's cache")
	}
}

// The comparison above is the test's only witness that the stores agree, so
// it is checked on its own: each fact it must name, and the one default it
// must not mistake for a difference.
func TestTheChannelStoreComparisonNamesEachFact(t *testing.T) {
	live := channelFacts{PostPolicy: "everyone"}
	for _, c := range []struct {
		name      string
		pg, graph channelFacts
		want      []string
	}{
		{"the same", live, live, nil},
		{"a node without a policy has the default", live, channelFacts{}, nil},
		{"post_policy", channelFacts{PostPolicy: "admins_only"}, live, []string{"post_policy"}},
		{"ch_private", channelFacts{PostPolicy: "everyone", Private: true}, live, []string{"ch_private"}},
		{"deleted_at", live, channelFacts{PostPolicy: "everyone", Deleted: true}, []string{"deleted_at"}},
		{"all three", channelFacts{PostPolicy: "admins_only", Private: true, Deleted: true}, live,
			[]string{"post_policy", "ch_private", "deleted_at"}},
	} {
		if got := channelFactsDiffer(c.pg, c.graph); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
