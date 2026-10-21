//go:build integration
// +build integration

package integration_test

// Per-query budgets for the graph's hot reads, measured on a workspace built
// at a known size (tests/integration/dgfixture).
//
// HOW A PACKAGE ADDS A QUERY (DG4, DG5, DG7, DG9, DG10 and on):
//
//  1. Add a runner to budgetRunners (dgraph_budget_queries_test.go) that
//     calls the real domain or business function on the fixture and returns
//     how many things it read. If the fixture lacks the shape the read needs,
//     add it to dgfixture (and its unit tests) rather than seeding here.
//  2. Add one line to dgraphBudgets below: its name, tier, p95 budget,
//     round trips and why. Measure first (DGRAPH_BUDGET_MEASURE=1 prints
//     every read's p50, p95 and trips and asserts nothing), then set the
//     budget with headroom: about three times the quiet p95, never less
//     than budgetFloor. Measuring runs every runner, so a read with no
//     line yet (or a tier with none, as scale has today) is measured too.
//  3. A read over a sensible target but not yet fixed gets that target in
//     Target and today's ceiling in P95: it is reported KNOWN-OVER and fails
//     only past its ceiling. Fixing it lowers P95 toward Target (the
//     ratchet); once under, drop Target.
//  4. Trips is exact: the Dgraph round trips one call makes. An N+1 fix
//     shows here first, and a read that must stay one query for 10 items and
//     for 500 gets a line for each (see doc.access.10 and doc.access.500).
//
// WHY BUDGETS DON'T FLAKE ON A SHARED MACHINE. The laptop that runs this runs
// about ten agents' gates beside it, at loads of 40 to 100 on 32 threads.
// Dgraph runs in its own process, so a thread's CPU time (tests/timing's
// FastestCPU) can't see its work, and one run's wall time is mostly the
// machine's load. So:
//   - each read warms up, then runs budgetRuns times; its p95 is held to
//     its budget;
//   - before each read, a calibration query (one indexed node by uuid, the
//     least work a round trip can do) runs calibrationRuns times, and its
//     median against calibrationQuiet (the reference median) is how much
//     slower the machine is now; the budget and target are multiplied by
//     that, never by less than 1 or more than calibrationMaxScale. Measured,
//     a read's p95 over the calibration median held within about 1.5x across
//     runs whose calibration moved fourfold (profile.viewer: 30 ms at 1.9 ms
//     calibration, 97 ms at 6.5 ms);
//   - a read over its scaled budget is measured once more, calibration
//     and all, and fails only if it is over again;
//   - budgets have honest headroom (step 2 above).
//   - round trips are counted, not timed, so their assertion is exact.
// A budget can still catch a read several times slower than today; it
// cannot catch 20%, and isn't meant to.
//
// Run: integ_rel.sh <wt> <log> '^TestDgraphQueryBudgets$'
// The scale tier: DGRAPH_FIXTURE_TIER=scale (most of an hour to build).

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/akashc777/OneCamp/tests/integration/dgfixture"
)

// dgraphBudget is one line of the table.
type dgraphBudget struct {
	Query  string        // its runner in budgetRunners
	Tier   string        // the fixture tier it is measured on
	P95    time.Duration // fails above this, times the calibration's scale
	Target time.Duration // a sensible target it is over today (KNOWN-OVER); 0 when it isn't
	Trips  int           // Dgraph round trips one call makes, exactly
	Why    string
}

// dgraphBudgets is the table: one line a query a tier. Budgets and targets
// are in calibrated ms (see calibrationQuiet): a read's p95 divided by how
// much slower than the reference the machine was when it ran. Each budget is
// about three times the worst calibrated p95 of two measured runs on 18-19
// Oct 2026 (loads 27 to 57), never under budgetFloor; the calibrated p95s are
// in the comment after each line, and the raw numbers in the commit.
var dgraphBudgets = []dgraphBudget{
	{"profile.viewer", "small", 75 * time.Millisecond, 0, 1, "§1.5 1: every request on a Redis miss; channels, projects, teams, DMs with participants, 3 task counts"}, // 23
	{"user.byuuid", "small", budgetFloor, 0, 1, "GetDgraphUserInfoByUUID: no cache, 92 call sites; measured 15/20 ms p50/p95 at a load of 88, one trip, so no cache (DG4)"},
	{"profile.superposter", "small", budgetFloor, 0, 1, "the same read for 20k posts: user_posts has no block, so none are read (the runner fails if that changes)"},                                                                                       // 9
	{"channel.latest", "small", 200 * time.Millisecond, 0, 1, "§1.5 2: #general's newest page, ordered over its ~14k posts"},                                                                                                                               // 43, then 94 at a load of 52
	{"channel.older", "small", 250 * time.Millisecond, 0, 1, "§1.5 2: a page from the middle of #general's history"},                                                                                                                                       // 55, then 115 at a load of 52
	{"dm.latest", "small", 110 * time.Millisecond, 0, 1, "§1.5 3: the viewer's busiest DM (3,000 messages)"},                                                                                                                                               // 37
	{"group.latest", "small", budgetFloor, 0, 1, "§1.5 3: a group of the viewer's"},                                                                                                                                                                        // 15
	{"sidebar.channels", "small", 300 * time.Millisecond, 0, 1, "§1.5 4: the viewer's 23 channels, each with its newest post (F3 roots it at the viewer)"},                                                                                                 // 99
	{"dm.list", "small", 1200 * time.Millisecond, 100 * time.Millisecond, 1, "§1.5 5: all 83 DMs and groups with their newest message, unpaginated, @cascade; DG4 3 pages it"},                                                                             // 252, then 557 in a burst
	{"inbox", "small", 100 * time.Millisecond, 0, 1, "§1.5 6: mentions, comments and reactions, 30 of each"},                                                                                                                                               // 34
	{"docs.top.workspace", "small", 500 * time.Millisecond, 100 * time.Millisecond, 2, "§1.5 7: the workspace's top level, then every row's level in one ResolveAll (DG4: was 3 trips)"},                                                                   // 165
	{"docs.top.private", "small", 300 * time.Millisecond, 0, 2, "§1.5 7: the viewer's private pages, with access (DG4: was 3 trips)"},                                                                                                                      // 96
	{"docs.top.space", "small", 150 * time.Millisecond, 0, 2, "§1.5 7: a project space's top level, with access (DG4: was 3 trips)"},                                                                                                                       // 48
	{"docs.children", "small", 300 * time.Millisecond, 0, 3, "§1.5 7: the wide page's 300 children: the parent's level, the level, its decisions (DG4: was 4 trips)"},                                                                                      // 95
	{"board.kanban", "small", 850 * time.Millisecond, 150 * time.Millisecond, 2, "§1.5 8: a 1,500-card board at the old default page (1,000 a column part): places, then cards; the web asks for board.kanban.first instead once it pages columns (DG4b)"}, // 277
	{"board.kanban.first", "small", 350 * time.Millisecond, 0, 2, "DG4b: the same board with the web's first page of 60 an open column part, every count kept; measured beside board.kanban in one run: p50 205 vs 431 ms, p95 270 vs 659 ms"},             // 116 quiet
	{"board.column", "small", 350 * time.Millisecond, 0, 4, "DG4b: a column's first page, then its next, two calls of places plus cards each"},                                                                                                             // 113 quiet
	{"doc.access.10", "small", budgetFloor, 0, 1, "§1.6: access for 10 docs is one query"},                                                                                                                                                                 // 17
	{"doc.access.500", "small", 300 * time.Millisecond, 0, 1, "§1.6: and for 500, still one"},                                                                                                                                                              // 94
	{"doc.backlinks", "small", 100 * time.Millisecond, 0, 1, "the doc with 2,000 backlinks: its panel's newest 50"},                                                                                                                                        // 33
	{"deps.downstream", "small", 75 * time.Millisecond, 0, 1, "moving the head of a 40-task chain (128 tasks downstream)"},                                                                                                                                 // 25
	// The work graph (DG5): one graph read, then each kind's reader once (ReadMany), however many nodes; trips count the kinds read.
	{"graph.doc.1", "small", 150 * time.Millisecond, 0, 6, "the backlinked doc, one step: 2,000 mentioners capped at 20, 40 sharing posts, its people"},                        // 47, 49
	{"graph.doc.2", "small", 200 * time.Millisecond, 0, 9, "the same, two steps: through docs, tasks and posts to their channels and projects"},                                // 55 (176 and 36 trips before channels were one read)
	{"graph.person.2", "small", 250 * time.Millisecond, 0, 10, "the super poster, two steps: a scan of 20k posts for the ones sharing a doc, their docs and tasks"},            // 72 (185 and 42 trips before)
	{"graph.channel.2", "small", 230 * time.Millisecond, 0, 8, "#general, two steps: ~14k posts, found by the scan of posts sharing a doc, never by walking them"},             // 54, 76
	{"graph.project.2", "small", 170 * time.Millisecond, 0, 9, "the 1,500-card board's project, two steps: its tasks capped at 20, then 4 an edge, and what links them (DG6)"}, // 47, 56; DG6 28 (9 trips: the messages and comments linking its tasks are two more kinds read)
	// The work graph's references (DG6): the outbox sink's read, the sink on a source in step (that read and no write), the reconciler's pages.
	{"refs.source", "small", 50 * time.Millisecond, 0, 1, "a post's body and its refers_to, by uuid"},                                                                                    // 1.7
	{"refs.reconcile.one", "small", 50 * time.Millisecond, 0, 1, "the sink on a post already in step: one read, no write"},                                                               // 1.6
	{"refs.page.message", "small", 60 * time.Millisecond, 0, 1, "the reconciler's page of 500 posts, bodies and edges"},                                                                  // 18
	{"refs.page.doc", "small", 50 * time.Millisecond, 0, 1, "the reconciler's page of 50 docs, bodies and edges"},                                                                        // 3
	{"graph.path.near", "small", 250 * time.Millisecond, 0, 7, "the backlinked doc to #general: one shortest block, three paths, depth 4, 25 an edge"},                                   // 75, 83
	{"graph.path.far", "small", 1400 * time.Millisecond, 0, 7, "the backlinked doc to the board's project: the search's worst case, every capped edge to depth 4"},                       // 456, 284; RELEASE-LINE ONLY until beta takes it after the tags: 7, not 6 (21 Oct: beta 7aa3a43e0 measured 7, 7, 7 and the line 6, 6, 6 and 7 with the same code; the kinds reached follow the fixture's uid order, dgraph_budget_trips_test.go)
	{"graph.expand.12", "small", 210 * time.Millisecond, 0, 9, "Ask AI's graph context: one hop from 12 hits (the 2,000-mention doc and the chain head among them), every node decided"}, // 57 p50, 69 p95 (load 19); Ask AI waits 800
	// Who knows (DG9), after its seeds: the count is one DQL read; the whole answer decides each kind once (ReadMany), counts, then names the people.
	{"whoknows.count.200", "small", 75 * time.Millisecond, 0, 1, "200 items (80 docs, 80 tasks, 40 posts): five @groupby counts, the who block and the evidence, one read"}, // 21, 18
	{"whoknows.rank.200", "small", 400 * time.Millisecond, 0, 7, "the same, as the viewer: each kind decided once (5), the count (1), the people's names (1)"},              // 51, 128
}

const (
	budgetWarmups = 3
	budgetRuns    = 20
	// budgetFloor is the least budget a line may have: a few ms of Go and
	// gRPC are noise at any load.
	budgetFloor = 50 * time.Millisecond
	// calibrationRuns is how many calibration queries make its median.
	calibrationRuns = 31
	// calibrationQuiet is the reference calibration median: about the
	// quietest this machine got while ten agents worked (1.4 to 2 ms at
	// loads of 27 to 50, 18-19 Oct 2026; 6.5 ms at a load of 48 in a burst).
	// Below it the budgets apply as written.
	calibrationQuiet = 1500 * time.Microsecond
	// calibrationMaxScale bounds how far a busy machine stretches a budget.
	calibrationMaxScale = 8.0
)

// budgetResult is one read's measurement.
type budgetResult struct {
	Query    string  `json:"query"`
	Tier     string  `json:"tier"`
	P50ms    float64 `json:"p50_ms"`
	P95ms    float64 `json:"p95_ms"`
	BudgetMS float64 `json:"budget_ms"`
	Scale    float64 `json:"scale"`
	TargetMS float64 `json:"target_ms,omitempty"`
	Trips    int     `json:"trips"`
	Items    int     `json:"items"`
	Status   string  `json:"status"`
	Load1    string  `json:"load1"`
	CalMS    float64 `json:"calibration_ms"`
}

// roundTrips counts the Dgraph calls made through the project's client.
type roundTrips struct{ n atomic.Int64 }

func (c *roundTrips) intercept(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	c.n.Add(1)
	return invoker(ctx, method, req, reply, cc, opts...)
}

// countingClient points the project's graph client at the test's Dgraph
// through a connection that counts every call, until t ends.
func countingClient(t *testing.T, endpoint string) *roundTrips {
	t.Helper()
	c := &roundTrips{}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(c.intercept),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256<<20)))
	if err != nil {
		t.Fatalf("a counting graph client: %v", err)
	}
	live := dgraphInit.DgraphClient
	dgraphInit.DgraphClient = dgo.NewDgraphClient(api.NewDgraphClient(conn))
	t.Cleanup(func() {
		dgraphInit.DgraphClient = live
		_ = conn.Close()
	})
	return c
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

func ms(d time.Duration) float64 { return math.Round(float64(d)/1e4) / 100 }

func load1() string {
	b, _ := os.ReadFile("/proc/loadavg")
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return "?"
	}
	return f[0]
}

// calibrate is how many times slower than quiet the machine reads now.
func calibrate(ctx context.Context, t *testing.T, viewerUUID string) (float64, time.Duration) {
	t.Helper()
	q := `query c($id: string) { c(func: eq(user_uuid, $id)) { uid user_name } }`
	times := make([]time.Duration, calibrationRuns)
	for i := range times {
		start := time.Now()
		txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
		_, err := txn.QueryWithVars(ctx, q, map[string]string{"$id": viewerUUID})
		_ = txn.Discard(ctx)
		times[i] = time.Since(start)
		if err != nil {
			t.Fatalf("calibration: %v", err)
		}
	}
	sort.Slice(times, func(a, b int) bool { return times[a] < times[b] })
	median := percentile(times, 0.5)
	return min(max(float64(median)/float64(calibrationQuiet), 1), calibrationMaxScale), median
}

// measure times one read: its p50 and p95 over budgetRuns runs after
// budgetWarmups, the round trips of one call, and what it read.
func measure(ctx context.Context, f *dgfixture.Fixture, run budgetRunner, trips *roundTrips) (p50, p95 time.Duration, perCall, items int, err error) {
	for i := 0; i < budgetWarmups; i++ {
		if _, err = run(ctx, f); err != nil {
			return
		}
	}
	before := trips.n.Load()
	if items, err = run(ctx, f); err != nil {
		return
	}
	perCall = int(trips.n.Load() - before)
	times := make([]time.Duration, budgetRuns)
	for i := range times {
		start := time.Now()
		if _, err = run(ctx, f); err != nil {
			return
		}
		times[i] = time.Since(start)
	}
	sort.Slice(times, func(a, b int) bool { return times[a] < times[b] })
	return percentile(times, 0.5), percentile(times, 0.95), perCall, items, nil
}

// linesFor is the tier's lines; measuring only, every runner, those with no
// line for the tier under the floor budget, so a new tier (scale) or a new
// read is measured before its line is written.
func linesFor(tier string, measureOnly bool) []dgraphBudget {
	var out []dgraphBudget
	have := map[string]bool{}
	for _, b := range dgraphBudgets {
		if b.Tier == tier {
			out = append(out, b)
			have[b.Query] = true
		}
	}
	if !measureOnly {
		return out
	}
	var rest []string
	for name := range budgetRunners {
		if !have[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		out = append(out, dgraphBudget{Query: name, Tier: tier, P95: budgetFloor, Trips: 1, Why: "measured only"})
	}
	return out
}

// TestDgraphBudgetTableIsWhole: every line has a runner and a reason, a
// budget no less than the floor and a known tier, and every runner a line.
func TestDgraphBudgetTableIsWhole(t *testing.T) {
	seen := map[string]bool{}
	for _, b := range dgraphBudgets {
		key := b.Query + "/" + b.Tier
		switch {
		case seen[key]:
			t.Errorf("%s has two lines", key)
		case budgetRunners[b.Query] == nil:
			t.Errorf("%s has no runner in budgetRunners", b.Query)
		case b.Tier != dgfixture.Small.Name && b.Tier != dgfixture.Scale.Name:
			t.Errorf("%s: no tier %q", b.Query, b.Tier)
		case b.P95 < budgetFloor:
			t.Errorf("%s: a %v budget is under the %v floor", b.Query, b.P95, budgetFloor)
		case b.Target != 0 && b.Target >= b.P95:
			t.Errorf("%s: a known-over target (%v) must be under its ceiling (%v)", b.Query, b.Target, b.P95)
		case b.Trips < 1 || strings.TrimSpace(b.Why) == "":
			t.Errorf("%s: say its round trips and why", b.Query)
		}
		seen[key] = true
	}
	for name := range budgetRunners {
		found := false
		for _, b := range dgraphBudgets {
			found = found || b.Query == name
		}
		if !found {
			t.Errorf("runner %s has no line in dgraphBudgets", name)
		}
	}
}

// TestDgraphQueryBudgets builds the fixture tier DGRAPH_FIXTURE_TIER names
// (small by default) and holds every read of that tier to its line.
func TestDgraphQueryBudgets(t *testing.T) {
	tier, err := dgfixture.TierFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	measureOnly := os.Getenv("DGRAPH_BUDGET_MEASURE") == "1"
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	start := time.Now()
	f, err := dgfixture.Write(ctx, tier, dgfixture.DefaultSeed, dgraphInit.DgraphClient, env.PG)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture %s: %d N-Quads in %d batches, built in %v (load %s); %v", tier.Name, f.Quads, f.Batches, time.Since(start).Round(time.Millisecond), load1(), f.Counts)

	trips := countingClient(t, dg.Endpoint)
	var results []budgetResult
	for _, b := range linesFor(tier.Name, measureOnly) {
		t.Run(b.Query, func(t *testing.T) {
			run := budgetRunners[b.Query]
			var r budgetResult
			for attempt := 1; ; attempt++ {
				scale, cal := calibrate(ctx, t, f.Viewer.UUID)
				p50, p95, perCall, items, err := measure(ctx, f, run, trips)
				if err != nil {
					t.Fatalf("%s: %v", b.Query, err)
				}
				limit := time.Duration(float64(b.P95) * scale)
				r = budgetResult{Query: b.Query, Tier: tier.Name, P50ms: ms(p50), P95ms: ms(p95), BudgetMS: ms(limit), Scale: math.Round(scale*100) / 100,
					TargetMS: ms(time.Duration(float64(b.Target) * scale)), Trips: perCall, Items: items, Status: "OK", Load1: load1(), CalMS: ms(cal)}
				if target := time.Duration(float64(b.Target) * scale); b.Target != 0 && p95 > target {
					r.Status = fmt.Sprintf("KNOWN-OVER %.1fx target", float64(p95)/float64(target))
				}
				if measureOnly || p95 <= limit || attempt == 2 {
					if !measureOnly && p95 > limit {
						r.Status = "OVER"
					}
					break
				}
				t.Logf("%s: p95 %v over %v (scale %.2f, load %s); measuring once more", b.Query, p95, limit, scale, r.Load1)
			}
			t.Logf("budget %-20s %s p50=%7.2fms p95=%7.2fms budget=%7.2fms (x%.2f) trips=%d items=%d cal=%.2fms load=%s %s",
				r.Query, r.Tier, r.P50ms, r.P95ms, r.BudgetMS, r.Scale, r.Trips, r.Items, r.CalMS, r.Load1, r.Status)
			results = append(results, r)
			if measureOnly {
				return
			}
			if bad, want := tripsOff(b.Query, r.Trips, b.Trips); bad {
				t.Errorf("%s makes %d Dgraph round trips a call, its line says %s", b.Query, r.Trips, want)
			}
			if r.Status == "OVER" {
				t.Errorf("%s: p95 %.2f ms over its %.2f ms budget (%v scaled x%.2f by the calibration) twice", b.Query, r.P95ms, r.BudgetMS, b.P95, r.Scale)
			}
		})
	}
	// The budgets still catch a real slowdown: profile.viewer run ten times a
	// call, as code ten times slower would take, is over its budget.
	t.Run("a ten times slower read is caught", func(t *testing.T) {
		if measureOnly || tier.Name != dgfixture.Small.Name {
			t.Skip("measuring only, or not the small tier")
		}
		var line dgraphBudget
		for _, b := range dgraphBudgets {
			if b.Query == "profile.viewer" && b.Tier == tier.Name {
				line = b
			}
		}
		one := budgetRunners["profile.viewer"]
		slow := func(ctx context.Context, f *dgfixture.Fixture) (n int, err error) {
			for i := 0; i < 10 && err == nil; i++ {
				n, err = one(ctx, f)
			}
			return n, err
		}
		scale, _ := calibrate(ctx, t, f.Viewer.UUID)
		_, p95, _, _, err := measure(ctx, f, slow, trips)
		if err != nil {
			t.Fatal(err)
		}
		limit := time.Duration(float64(line.P95) * scale)
		t.Logf("ten profile reads a call: p95 %v against the %v budget (x%.2f)", p95, limit, scale)
		if p95 <= limit {
			t.Errorf("ten profile reads a call (p95 %v) are within the %v budget: it would miss a read ten times slower", p95, limit)
		}
	})
	if out := os.Getenv("DGRAPH_BUDGET_OUT"); out != "" {
		raw, _ := json.MarshalIndent(map[string]any{"tier": tier.Name, "quads": f.Quads, "counts": f.Counts, "results": results}, "", "  ")
		if err := os.WriteFile(out, raw, 0o644); err != nil {
			t.Errorf("writing %s: %v", out, err)
		}
	}
}
