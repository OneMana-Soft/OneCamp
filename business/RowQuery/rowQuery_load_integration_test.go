//go:build integration

package business_test

// Load (notion-db-16, tables L1): writers and readers at once on a table of
// 100,000 rows, against Postgres 12 with every migration. Eight writers
// change cells through the row ledger (PatchRow, a change set each), each on
// rows of its own, while eight readers read windows over HTTP as the web
// does, sorted by a number the writers change, at the top, the middle and
// the end. Held every run, as work: every write answered, no write lost
// (each cell ends at its writer's last value), one change set a write, and
// no read failing on the server (5xx). With $LOAD_OUT set (a quiet machine),
// it also holds a reader's p95 under the writes to 3x its p95 alone, and
// writes both, with the machine's facts, there: the page's "Under writes".
// It runs only when asked ($LOAD_RUN=1): 100,000 rows and minutes of
// writes are a run main grants, never a part of every RowQuery suite, the
// integrator's gates or the sweep (the SR650 powered off under load, 5 Nov).
// Run under heavy.sh as ONE integration suite, beside no other:
// LOAD_RUN=1 integ_rel.sh <worktree> <log> '^TestWritersAndReadersAtOnceOnAHundredThousandRows$' with PKGS='./business/RowQuery/'
// ($LOAD_SECONDS: how long the writers write, 20 s unless set.)

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	rowQueryController "github.com/akashc777/OneCamp/controllers/RowQuery"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

const (
	loadWriters = 8
	loadReaders = 8
	// loadRowsEach is how many rows each writer changes, its own.
	loadRowsEach = 1000
)

func TestWritersAndReadersAtOnceOnAHundredThousandRows(t *testing.T) {
	if os.Getenv("LOAD_RUN") != "1" {
		t.Skip("a load run: set LOAD_RUN=1 (a run main grants)")
	}
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	statementsGetAMinute(t)
	integration.SetupDgraph(t)
	// What the server logs while the load runs (SetupDgraph sends it nowhere),
	// so a failure names its cause: each distinct line, ids masked, and how often.
	caught := catchLog(t)
	me := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'load@example.test', 'load', 'Load', NOW(), NOW())`, me); err != nil {
		t.Fatal(err)
	}
	owner := dataTable.Actor{UserID: me}
	tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: "Lines", Visibility: "workspace"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	item, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Item", Type: model.FieldText}, owner)
	if err != nil {
		t.Fatal(err)
	}
	amount, err := dataTable.CreateField(ctx, tb.Id, dataTable.FieldInput{Name: "Amount", Type: model.FieldNumber}, owner)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, scaleRows)
	for i := range ids {
		ids[i] = uuid.New()
	}
	fill(ctx, t, tb.Id, me, ids, func(i int) map[string]interface{} {
		return map[string]interface{}{item.Id.String(): fmt.Sprintf("Line %06d", i), amount.Id.String(): float64((i * 7) % 1000)}
	})
	if err := rowQuery.IndexTable(ctx, tb.Id); err != nil {
		t.Fatalf("index: %v", err)
	}
	sets := func() int {
		var n int
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT count(*) FROM data_table_change_sets WHERE table_id = $1`, tb.Id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	setsBefore := sets()

	spec := query.Spec{Sorts: []query.Sort{{Field: "field:" + amount.Id.String(), Desc: true}}}
	places := []int{0, scaleRows / 2, scaleRows - windowSize}
	var fails5xx, notOK atomic.Int64
	// refused keeps each distinct reply a read was refused with, and how often.
	refused := &caughtLines{count: map[string]int{}}
	// read runs the readers until stop closes, and answers each read's wait.
	read := func(stop <-chan struct{}) []time.Duration {
		var mu sync.Mutex
		var took []time.Duration
		var wg sync.WaitGroup
		for r := 0; r < loadReaders; r++ {
			wg.Add(1)
			go func(r int) {
				defer wg.Done()
				for n := 0; ; n++ {
					select {
					case <-stop:
						return
					default:
					}
					start := time.Now()
					rec := overHTTP(t, rowQueryController.QueryRows, "/tables/"+tb.Id.String()+"/query", tb.Id, me,
						map[string]any{"spec": spec, "n": windowSize, "at": places[(r+n)%len(places)]})
					d := time.Since(start)
					code := rec.Code
					if code >= 500 {
						fails5xx.Add(1)
					} else if code != http.StatusOK {
						notOK.Add(1)
					}
					if code != http.StatusOK {
						refused.add(fmt.Sprintf("%d %s", code, rec.Body.String()))
					}
					mu.Lock()
					took = append(took, d)
					mu.Unlock()
				}
			}(r)
		}
		wg.Wait()
		return took
	}
	pct := func(ds []time.Duration, p float64) float64 {
		if len(ds) == 0 {
			return 0
		}
		s := slices.Clone(ds)
		slices.Sort(s)
		return math.Round(float64(s[int(math.Ceil(p*float64(len(s))))-1])/1e4) / 100
	}
	seconds := 20
	if v, err := strconv.Atoi(os.Getenv("LOAD_SECONDS")); err == nil && v > 0 {
		seconds = v
	}
	run := time.Duration(seconds) * time.Second

	// The readers alone first: what each read takes with nobody writing.
	stop := make(chan struct{})
	time.AfterFunc(run/2, func() { close(stop) })
	quiet := read(stop)

	// Then the writers beside them, each on rows of its own, a value each
	// write, so its last value is the cell's.
	last := make([]map[uuid.UUID]float64, loadWriters)
	var writes, writeFails atomic.Int64
	stop = make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < loadWriters; w++ {
		last[w] = map[uuid.UUID]float64{}
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(w)))
			mine := ids[w*loadRowsEach*10 : w*loadRowsEach*10+loadRowsEach]
			for n := 1; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				row := mine[rnd.Intn(len(mine))]
				v := float64(w*1_000_000 + n)
				if _, err := dataTable.PatchRow(ctx, tb.Id, row, map[string]interface{}{amount.Id.String(): v}, owner); err != nil {
					writeFails.Add(1)
					t.Errorf("writer %d: %v", w, err)
					continue
				}
				writes.Add(1)
				last[w][row] = v
			}
		}(w)
	}
	time.AfterFunc(run, func() { close(stop) })
	loaded := read(stop)
	wg.Wait()

	refused.report(t, "read refused")
	caught.report(t, "server logged")
	t.Logf("%d writes in %v (%.0f a second), %d failed; reads alone %d (p50 %.2f ms, p95 %.2f ms), beside the writes %d (p50 %.2f ms, p95 %.2f ms); %d 5xx, %d other refusals",
		writes.Load(), run, float64(writes.Load())/run.Seconds(), writeFails.Load(),
		len(quiet), pct(quiet, .5), pct(quiet, .95), len(loaded), pct(loaded, .5), pct(loaded, .95), fails5xx.Load(), notOK.Load())
	if writes.Load() == 0 || len(quiet) == 0 || len(loaded) == 0 {
		t.Fatalf("nothing measured: %d writes, %d + %d reads", writes.Load(), len(quiet), len(loaded))
	}
	if fails5xx.Load() > 0 || notOK.Load() > 0 {
		t.Errorf("reads failed: %d 5xx, %d other refusals", fails5xx.Load(), notOK.Load())
	}
	// No write lost: each cell holds its writer's last value.
	lost := 0
	for w := range last {
		var mine []uuid.UUID
		for id := range last[w] {
			mine = append(mine, id)
		}
		for at := 0; at < len(mine); at += dataTable.MaxRowsByID {
			some := mine[at:min(at+dataTable.MaxRowsByID, len(mine))]
			rows, err := dataTable.RowsByID(ctx, tb.Id, owner, some)
			if err != nil || len(rows) != len(some) {
				t.Fatalf("read back: %d of %d, %v", len(rows), len(some), err)
			}
			for _, r := range rows {
				var cells map[string]any
				_ = json.Unmarshal([]byte(r.Values), &cells)
				if cells[amount.Id.String()] != last[w][r.Id] {
					lost++
				}
			}
		}
	}
	if lost > 0 {
		t.Errorf("%d cells don't hold their last write", lost)
	}
	if got := sets() - setsBefore; got != int(writes.Load()) {
		t.Errorf("%d change sets for %d writes", got, writes.Load())
	}

	out := os.Getenv("LOAD_OUT")
	if out == "" {
		return
	}
	if q, l := pct(quiet, .95), pct(loaded, .95); l > 3*q {
		t.Errorf("a read's p95 beside the writes, %.2f ms, is past 3x its p95 alone, %.2f ms", l, q)
	}
	report := map[string]any{
		"host":    map[string]any{"cpu": cpuModel(), "cores": runtime.NumCPU(), "load_end": loadNow()},
		"rows":    scaleRows,
		"window":  windowSize,
		"writers": loadWriters, "readers": loadReaders, "seconds": seconds,
		"writes": writes.Load(), "writes_a_second": math.Round(float64(writes.Load()) / run.Seconds()),
		"alone":  map[string]any{"reads": len(quiet), "p50_ms": pct(quiet, .5), "p95_ms": pct(quiet, .95)},
		"beside": map[string]any{"reads": len(loaded), "p50_ms": pct(loaded, .5), "p95_ms": pct(loaded, .95)},
		"lost":   lost, "server_errors": fails5xx.Load(),
	}
	var pg string
	_ = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT version()`).Scan(&pg)
	report["postgres"] = pg
	b, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// caughtLines counts distinct lines, ids and numbers masked, keeping the order each was
// first seen in: the server's log lines (as a slog.Handler) or replies.
type caughtLines struct {
	mu    sync.Mutex
	count map[string]int
	order []string
}

var (
	anID     = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	aNumber  = regexp.MustCompile(`[0-9]+`)
	whiteRun = regexp.MustCompile(`\s+`)
)

func (c *caughtLines) add(line string) {
	line = anID.ReplaceAllString(line, "<id>")
	line = aNumber.ReplaceAllString(line, "N")
	line = whiteRun.ReplaceAllString(line, " ")
	if len(line) > 600 {
		line = line[:600]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count[line] == 0 {
		c.order = append(c.order, line)
	}
	c.count[line]++
}

// report logs the first 20 distinct lines, with how often each came.
func (c *caughtLines) report(t *testing.T, what string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, line := range c.order {
		if i == 20 {
			t.Logf("%s: %d more distinct lines", what, len(c.order)-20)
			break
		}
		t.Logf("%s x%d: %s", what, c.count[line], line)
	}
}

func (c *caughtLines) Enabled(context.Context, slog.Level) bool { return true }

func (c *caughtLines) Handle(_ context.Context, r slog.Record) error {
	line := r.Level.String() + " " + r.Message
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "args" || a.Key == "error" || a.Key == "err" || a.Key == "demoted" {
			line += " " + a.String()
		}
		return true
	})
	c.add(line)
	return nil
}

func (c *caughtLines) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *caughtLines) WithGroup(string) slog.Handler      { return c }

// catchLog points the server's logger at a caughtLines until the test ends.
func catchLog(t *testing.T) *caughtLines {
	c := &caughtLines{count: map[string]int{}}
	saved := helpers.Logger
	helpers.Logger = slog.New(c)
	t.Cleanup(func() { helpers.Logger = saved })
	return c
}
