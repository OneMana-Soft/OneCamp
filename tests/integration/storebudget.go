//go:build integration
// +build integration

package integration

// The store-budget harness (H1): what one request costs in round trips to
// each store the server talks to, counted at the stores' own clients.
//
// Five taps count every call the server makes while a window is open:
//   - Postgres: a pgx tracer next to the session's own (postgresInit's
//     integration-only ConnectTracedForTest). Every statement pgx sends counts
//     once, BEGIN, COMMIT and ROLLBACK among them, whatever context it carries,
//     context.Background() included. A prepare is not counted apart from its
//     run.
//   - Dgraph: a gRPC interceptor on the project's graph client, as the Dgraph
//     budget tests count (countingClient). One gRPC call counts once: a read,
//     a mutation, a commit.
//   - OpenSearch: an http.RoundTripper under the project's client. One HTTP
//     request counts once (a _bulk or _msearch is one).
//   - redis: a go-redis hook. A command counts once, and so does a pipeline
//     or a transaction, however many commands it carries: it is one round
//     trip. The commands a new connection sends to set itself up (HELLO,
//     CLIENT SETINFO) are not counted: whether a request dials depends on the
//     pool, not on the request.
//   - MQTT: a broker client that looks connected and counts each Publish.
//
// A window is the whole process's, so work nothing in the request started
// must not land in it:
//   - before a window opens, the harness waits until no store has been called
//     for quietFor, no call is in flight and no handed-off work runs;
//   - the loops router.Routes starts (startBackgroundLoops) are told apart by
//     the goroutine that makes the call, and counted apart (Background);
//   - workers cmd/server starts (the outbox relay and the like) do not run in
//     the test process, so what they would do with a request's rows is not
//     counted.
//
// Sync is what the request's own goroutine called (ServeHTTP on an httptest
// recorder: from the router's entry to the response written). Async is what
// every other goroutine the request started called, before or after the
// response, until the process went quiet again (and handed-off work,
// helpers.SettleBackground, settled). Told apart by goroutine, not by time, so
// a fire-and-forget goroutine counts the same on every run. Measure is never
// called from two goroutines at once.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/jackc/pgx/v5"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
)

// Store is one of the stores the server calls.
type Store int

const (
	StorePostgres Store = iota
	StoreDgraph
	StoreOpenSearch
	StoreRedis
	StoreMQTT
	storeCount
)

// StoreNames are the stores' names in a budget file, in Store order.
var StoreNames = [storeCount]string{"pg", "dg", "os", "redis", "mqtt"}

// Counts is a number of calls per store.
type Counts [storeCount]int

// Total is the calls to every store.
func (c Counts) Total() int {
	n := 0
	for _, v := range c {
		n += v
	}
	return n
}

func (c Counts) String() string {
	var b strings.Builder
	for i, v := range c {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%d", StoreNames[i], v)
	}
	return b.String()
}

// Window is what one measured call made.
//
// Sync is what the request's own goroutine called, from handler entry to the
// response; Async is what every other goroutine the request started called,
// before or after the response, until the process was quiet. The split is by
// goroutine, not by time, so a fire-and-forget goroutine that sometimes
// finishes before the handler returns counts the same each time; a goroutine
// the handler waits for (errgroup fan-out) counts as async too.
type Window struct {
	Sync       Counts // by the goroutine that ran do
	Async      Counts // by any other goroutine of the request's, until the process was quiet
	Background Counts // by the loops router.Routes starts, while the window was open: not the call's
	// SyncCalls and AsyncCalls say what each counted call was ("pg SELECT
	// ...", "dg /api.Dgraph/Query", "redis get"), in the order made, so a
	// count that moves can be traced to its calls.
	SyncCalls, AsyncCalls []string
	// BackgroundCalls say the same of Background, each after the name it was
	// told apart by ("business/Import.StartReaperLoop: pg UPDATE ..."): the
	// window is the whole process's, so every loop any test's router.Routes
	// started lands its calls here, and one loop's are read by its name.
	BackgroundCalls []string
}

const (
	// quietFor is how long no store may be called, and nothing be in flight,
	// before the process counts as quiet.
	quietFor = 400 * time.Millisecond
	// quietCap is how long the harness waits for quiet before it fails.
	quietCap = 20 * time.Second
)

// notTheRequests are the functions whose goroutines' calls are never a
// request's: the loops startBackgroundLoops (router/router.go) starts, named
// by package path and function, and go-redis setting up a new connection. A
// guard test holds the loops to that function's list.
var notTheRequests = []string{
	"business/Webhook.StartWebhookCleanupScheduler",
	"business/Import.StartReaperLoop",
	"business/Import.StartCleanupLoop",
	"business/Board.StartBoardSnapshotCleanupLoop",
	"github.com/redis/go-redis/v9.(*baseClient).initConn",
}

// BackgroundLoops are the names in notTheRequests that are loops: what a
// guard compares with startBackgroundLoops.
func BackgroundLoops() []string {
	var out []string
	for _, n := range notTheRequests {
		if strings.HasPrefix(n, "business/") {
			out = append(out, n)
		}
	}
	return out
}

// StoreTaps counts the calls the server makes to each store while a window
// is open.
type StoreTaps struct {
	open       atomic.Bool
	caller     atomic.Uint64 // the goroutine running the measured call, while it runs
	n          [storeCount]atomic.Int64
	async      [storeCount]atomic.Int64
	background [storeCount]atomic.Int64
	inFlight   atomic.Int64
	last       atomic.Int64 // when a store was last called or answered, unix ns
	exclude    []string
	mu         sync.Mutex
	calls      []string // what each call counted in the open window was, by the caller
	asyncCalls []string // and by the request's other goroutines
	bgCalls    []string // and by the goroutines exclude names, after the name
}

// goroutineID reads the id from a stack's first line, "goroutine 12 [...".
func goroutineID(st []byte) uint64 {
	st = bytes.TrimPrefix(st, []byte("goroutine "))
	var id uint64
	for _, c := range st {
		if c < '0' || c > '9' {
			break
		}
		id = id*10 + uint64(c-'0')
	}
	return id
}

// ids are uuids in a call's text, which differ from run to run.
var ids = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

var stacks = sync.Pool{New: func() any { b := make([]byte, 64<<10); return &b }}

// whose is the calling goroutine's id, and the first exclude name its stack
// holds when it is one whose calls are never a request's (notTheRequests,
// AlsoExclude), "" when it may be the request's.
func (s *StoreTaps) whose() (gid uint64, notRequest string) {
	bp := stacks.Get().(*[]byte)
	defer stacks.Put(bp)
	st := (*bp)[:runtime.Stack(*bp, false)]
	for _, name := range s.exclude {
		if bytes.Contains(st, []byte(name)) {
			return goroutineID(st), name
		}
	}
	return goroutineID(st), ""
}

// begin counts a call to st, what it was, and returns what marks it
// answered.
func (s *StoreTaps) begin(st Store, what string) (end func()) {
	gid, notRequest := s.whose()
	if notRequest != "" {
		if s.open.Load() {
			s.background[st].Add(1)
			s.mu.Lock()
			s.bgCalls = append(s.bgCalls, notRequest+": "+StoreNames[st]+" "+ids.ReplaceAllString(what, "<id>"))
			s.mu.Unlock()
		}
		return func() {}
	}
	s.inFlight.Add(1)
	s.last.Store(time.Now().UnixNano())
	if s.open.Load() {
		what = StoreNames[st] + " " + ids.ReplaceAllString(what, "<id>")
		s.mu.Lock()
		if gid == s.caller.Load() {
			s.n[st].Add(1)
			s.calls = append(s.calls, what)
		} else {
			s.async[st].Add(1)
			s.asyncCalls = append(s.asyncCalls, what)
		}
		s.mu.Unlock()
	}
	return func() {
		s.last.Store(time.Now().UnixNano())
		s.inFlight.Add(-1)
	}
}

// AlsoExclude counts apart the calls made on goroutines whose stack names fn
// (a package path and function, as a stack prints it), as it does the
// background loops'.
func (s *StoreTaps) AlsoExclude(fn string) { s.exclude = append(s.exclude, fn) }

func (s *StoreTaps) take() (w Window) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.n {
		w.Sync[i] = int(s.n[i].Swap(0))
		w.Async[i] = int(s.async[i].Swap(0))
	}
	w.SyncCalls, s.calls = s.calls, nil
	w.AsyncCalls, s.asyncCalls = s.asyncCalls, nil
	w.BackgroundCalls, s.bgCalls = s.bgCalls, nil
	return w
}

func (s *StoreTaps) takeBackground() (c Counts) {
	for i := range s.background {
		c[i] = int(s.background[i].Swap(0))
	}
	return c
}

// waitQuiet waits until no store has been called for quietFor, nothing is
// in flight, and no work the server hands off (helpers.RegisterBackground:
// event sinks, the audit writer, search re-index queues) is still running, so
// work that pauses longer than quietFor between calls is still waited for.
func (s *StoreTaps) waitQuiet() error {
	deadline := time.Now().Add(quietCap)
	for {
		busy := helpers.SettleBackground(time.Until(deadline))
		idle := time.Since(time.Unix(0, s.last.Load()))
		if len(busy) == 0 && s.inFlight.Load() == 0 && idle >= quietFor && len(helpers.SettleBackground(0)) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("never quiet in %s: %d store calls in flight, the last %s ago, handed-off work running: %v", quietCap, s.inFlight.Load(), idle.Round(time.Millisecond), busy)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Measure counts the store calls do makes: Sync on its own goroutine, Async on
// the others it started, until the process is quiet. It first waits for quiet, so what an
// earlier call left running is not counted.
func (s *StoreTaps) Measure(do func()) (Window, error) {
	var w Window
	if err := s.waitQuiet(); err != nil {
		return w, fmt.Errorf("before the window opened: %w", err)
	}
	s.take()
	s.takeBackground()
	bp := stacks.Get().(*[]byte)
	s.caller.Store(goroutineID((*bp)[:runtime.Stack(*bp, false)]))
	stacks.Put(bp)
	s.open.Store(true)
	defer s.open.Store(false)
	do()
	s.caller.Store(0) // after it returns, nothing is the call's own
	s.last.Store(time.Now().UnixNano())
	err := s.waitQuiet()
	w = s.take()
	w.Background = s.takeBackground()
	if err != nil {
		return w, fmt.Errorf("after the call returned: %w", err)
	}
	return w, nil
}

// TapStores points the server's stores at counting taps until t ends: its
// Postgres pool (a new one on env, traced), its graph client (on dg), its
// OpenSearch client (on search) and its redis client (SetupRedis first), and an
// MQTT client that looks connected and counts what it publishes.
func TapStores(t *testing.T, env *Env, dg *DgraphEnv, search *OpenSearchEnv) *StoreTaps {
	t.Helper()
	s := &StoreTaps{exclude: append([]string(nil), notTheRequests...)}

	if err := postgresInit.ConnectTracedForTest(context.Background(), env.DSN, pgTap{s}); err != nil {
		t.Fatalf("connect the server's pool, traced: %v", err)
	}
	markConnected(t)

	if redisInit.RedisClient == nil {
		t.Fatal("TapStores taps the server's redis client: call SetupRedis first")
	}
	redisInit.RedisClient.AddHook(redisTap{s})

	conn, err := grpc.NewClient(dg.Endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(s.dgraphIntercept),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256<<20)))
	if err != nil {
		t.Fatalf("a counting graph client: %v", err)
	}
	liveGraph := dgraphInit.DgraphClient
	dgraphInit.DgraphClient = dgo.NewDgraphClient(api.NewDgraphClient(conn))

	searchClient, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{
		Addresses: []string{search.URL},
		Transport: searchTap{s, http.DefaultTransport},
	}})
	if err != nil {
		t.Fatalf("a counting search client: %v", err)
	}
	liveSearch := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = searchClient

	UseMqttClient(t, &countingMqtt{Client: unconnectedMqttClient(), s: s})

	t.Cleanup(func() {
		dgraphInit.DgraphClient = liveGraph
		_ = conn.Close()
		opensearchInit.OpenSearchClient = liveSearch
	})
	return s
}

// pgTap counts each statement pgx sends.
type pgTap struct{ s *StoreTaps }

type pgEndKey struct{}

// statement is a statement's text on one line, cut to its first 90 bytes:
// enough to tell two statements apart in a trace.
func statement(sql string) string {
	sql = strings.Join(strings.Fields(sql), " ")
	if len(sql) > 90 {
		sql = sql[:90]
	}
	return sql
}

func (p pgTap) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, pgEndKey{}, p.s.begin(StorePostgres, statement(data.SQL)))
}

func (pgTap) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if end, ok := ctx.Value(pgEndKey{}).(func()); ok {
		end()
	}
}

func (s *StoreTaps) dgraphIntercept(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	defer s.begin(StoreDgraph, method)()
	return invoker(ctx, method, req, reply, cc, opts...)
}

// searchTap counts each HTTP request the search client sends.
type searchTap struct {
	s    *StoreTaps
	next http.RoundTripper
}

func (r searchTap) RoundTrip(req *http.Request) (*http.Response, error) {
	defer r.s.begin(StoreOpenSearch, req.Method+" "+req.URL.Path)()
	return r.next.RoundTrip(req)
}

// redisTap counts each command, and each pipeline once.
type redisTap struct{ s *StoreTaps }

func (redisTap) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r redisTap) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		defer r.s.begin(StoreRedis, cmd.Name())()
		return next(ctx, cmd)
	}
}

func (r redisTap) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		defer r.s.begin(StoreRedis, fmt.Sprintf("pipeline of %d", len(cmds)))()
		return next(ctx, cmds)
	}
}

// countingMqtt is a broker client that looks connected, so the server
// publishes as it does in production, and counts each Publish.
type countingMqtt struct {
	MQTT.Client
	s *StoreTaps
}

func (*countingMqtt) IsConnected() bool      { return true }
func (*countingMqtt) IsConnectionOpen() bool { return true }

func (m *countingMqtt) Publish(topic string, _ byte, _ bool, _ interface{}) MQTT.Token {
	m.s.begin(StoreMQTT, topic)()
	return doneToken{}
}

// doneToken is a publish the broker has taken.
type doneToken struct{}

var closedDone = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

func (doneToken) Wait() bool                     { return true }
func (doneToken) WaitTimeout(time.Duration) bool { return true }
func (doneToken) Done() <-chan struct{}          { return closedDone }
func (doneToken) Error() error                   { return nil }

// The budget file (testdata/store-budgets.tsv) holds each row's sync counts
// as they were last written: per store while the request ran, the audit's
// target for the total, and a note. Like the code ratchets in helpers
// (checkRatchet), whose one count per key does not fit eight columns, a count
// may only go down, and it is lowered in the file on purpose: a rise fails
// naming the store, and so does a fall until the file is rewritten
// (StoreBudgetWriteEnv), so each head's gain shows in its diff.
//
// Async counts are not in the file and hold nothing: a request's async work
// is counted by the time window it lands in, not by the request, so on a
// loaded machine it finishes after its window closed or lands in the next
// row's (b320, at load 17-29: dm createChat async 7 then 5, sidebarNav 0 then
// 4, with every sync count the same). TestStoreBudgets prints them per run.
// V-1121 tags async work with its request and waits for it to drain; until
// then a number in the file would be one run's chance.

// StoreBudgetFile is the budget file, from the package's directory.
const StoreBudgetFile = "testdata/store-budgets.tsv"

// StoreBudgetWriteEnv, set to 1, rewrites the budget file from the counts
// measured.
const StoreBudgetWriteEnv = "STORE_BUDGET_WRITE"

// StoreBudgetAtEnv, set to "target", holds each row's sync total to its
// target_total instead of its counts: the red check of what the later heads
// must reach. The audit's targets count async too; the sync total is what can
// be held until V-1121.
const StoreBudgetAtEnv = "STORE_BUDGET_AT"

const budgetHeader = "route\tsync_pg\tsync_dg\tsync_os\tsync_redis\tsync_mqtt\ttarget_total\tnote"

// Budget is one route's line in the budget file.
type Budget struct {
	Route  string
	Sync   Counts
	Async  int // measured and printed, never in the file nor held (V-1121)
	Target int // -1: the audit gave none ("-")
	Note   string
}

// ReadBudgets reads the budget file, by route.
func ReadBudgets(path string) (map[string]Budget, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) == 0 || lines[0] != budgetHeader {
		return nil, fmt.Errorf("%s: the first line is not %q", path, budgetHeader)
	}
	out := map[string]Budget{}
	for i, line := range lines[1:] {
		f := strings.Split(line, "\t")
		if len(f) != 8 {
			return nil, fmt.Errorf("%s:%d: %d columns; want 8", path, i+2, len(f))
		}
		b := Budget{Route: f[0], Note: f[7], Target: -1}
		for j := range b.Sync {
			if b.Sync[j], err = strconv.Atoi(f[j+1]); err != nil {
				return nil, fmt.Errorf("%s:%d: column %d: %v", path, i+2, j+2, err)
			}
		}
		if f[6] != "-" {
			if b.Target, err = strconv.Atoi(f[6]); err != nil {
				return nil, fmt.Errorf("%s:%d: target_total: %v", path, i+2, err)
			}
		}
		if _, dup := out[b.Route]; dup {
			return nil, fmt.Errorf("%s:%d: %s twice", path, i+2, b.Route)
		}
		out[b.Route] = b
	}
	return out, nil
}

// WriteBudgets writes the budget file, rows in the order given.
func WriteBudgets(path string, rows []Budget) error {
	var b strings.Builder
	b.WriteString(budgetHeader + "\n")
	for _, r := range rows {
		target := "-"
		if r.Target >= 0 {
			target = strconv.Itoa(r.Target)
		}
		fmt.Fprintf(&b, "%s", r.Route)
		for _, n := range r.Sync {
			fmt.Fprintf(&b, "\t%d", n)
		}
		fmt.Fprintf(&b, "\t%s\t%s\n", target, r.Note)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// OverBudget is what is wrong with measured against its line in the file:
// every sync column that rose or fell, by how much. Async is not held.
func OverBudget(line, measured Budget) []string {
	var out []string
	say := func(col string, was, now int) {
		switch {
		case now > was:
			out = append(out, fmt.Sprintf("%s: %s rose %d -> %d (+%d): a store call was added to this route", measured.Route, col, was, now, now-was))
		case now < was:
			out = append(out, fmt.Sprintf("%s: %s fell %d -> %d (-%d): lower the file (%s=1) so the gain is recorded", measured.Route, col, was, now, was-now, StoreBudgetWriteEnv))
		}
	}
	for i := range measured.Sync {
		say("sync_"+StoreNames[i], line.Sync[i], measured.Sync[i])
	}
	return out
}

// OverTarget is what is wrong with measured's sync total against the audit's
// target. Async is not held.
func OverTarget(line, measured Budget) string {
	if line.Target < 0 || measured.Sync.Total() <= line.Target {
		return ""
	}
	return fmt.Sprintf("%s: sync total %d over target %d (+%d): sync %v", measured.Route, measured.Sync.Total(), line.Target, measured.Sync.Total()-line.Target, measured.Sync)
}

// IdleWindow is what the window of a call that made none says: bad when it
// counted a sync call, which only work left on the measuring goroutine can
// make, so the rows would carry work that is not theirs; note is the async it
// counted, printed and not held (V-1121), as the rows' async is.
func IdleWindow(w Window) (bad, note string) {
	if w.Sync.Total() != 0 {
		bad = fmt.Sprintf("an idle window counted sync %v, async %v: the rows would carry work that is not theirs", w.Sync, w.Async)
	}
	return bad, fmt.Sprintf("idle window counted %d async (reported, not enforced; V-1121): %v", w.Async.Total(), w.Async)
}

// Unrepeatable is what differs between a row's first run (first, measured in
// window a) and its run n (again, in window b) in sync counts, with the sync
// calls that moved; "" when the two runs counted the same sync calls. Async
// is not compared: it is counted by time window, not by request, so under
// load it moves between runs of the same code (V-1121 tags it).
func Unrepeatable(n int, first, again Budget, a, b Window) string {
	if again.Sync == first.Sync {
		return ""
	}
	var moved []string
	for _, c := range CallsMoved(a.SyncCalls, b.SyncCalls) {
		moved = append(moved, "sync "+c)
	}
	return fmt.Sprintf("%s is not repeatable: run 1 counted sync %v, run %d sync %v; the calls that moved:\n  %s",
		again.Route, first.Sync, n, again.Sync, strings.Join(moved[:min(len(moved), 16)], "\n  "))
}

// CallsMoved is how two windows' calls differ, as "+n what" (in b, not a)
// and "-n what" (in a, not b) lines, sorted: what made a count move.
func CallsMoved(a, b []string) []string {
	n := map[string]int{}
	for _, c := range a {
		n[c]--
	}
	for _, c := range b {
		n[c]++
	}
	var out []string
	for c, d := range n {
		if d != 0 {
			out = append(out, fmt.Sprintf("%+d %s", d, c))
		}
	}
	sort.Strings(out)
	return out
}
