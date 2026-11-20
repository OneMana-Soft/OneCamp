//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/redis/go-redis/v9"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	dataTable "github.com/akashc777/OneCamp/business/DataTable"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
	calendarModels "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	tableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	taskReadGeneration "github.com/akashc777/OneCamp/models/taskread/generation"
	"github.com/akashc777/OneCamp/router"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/akashc777/OneCamp/tests/integration/dgfixture"
)

// startFakeLoop runs f on a goroutine of its own, as a background loop does.
func startFakeLoop(f func()) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); f() }()
	return &wg
}

// Each tap counts one call to its store once, in its own column and no other;
// what the budgets are made of holds only if this does.
func TestEveryStoreTapCountsOneCallOnce(t *testing.T) {
	env := integration.SetupEnv(t)
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	taps := integration.TapStores(t, env, dg, search)
	ctx := context.Background()
	db := postgresInit.DBConn.SqlDB

	only := func(st integration.Store, n int) (c integration.Counts) {
		c[st] = n
		return c
	}
	check := func(t *testing.T, want, wantAsync integration.Counts, do func() error) {
		t.Helper()
		var callErr error
		w, err := taps.Measure(func() { callErr = do() })
		if err != nil {
			t.Fatal(err)
		}
		if callErr != nil {
			t.Fatalf("the measured call: %v", callErr)
		}
		if w.Sync != want || w.Async != wantAsync {
			t.Errorf("counted sync %v, async %v; want sync %v, async %v", w.Sync, w.Async, want, wantAsync)
		}
	}
	var none integration.Counts

	t.Run("nothing called counts nothing", func(t *testing.T) {
		check(t, none, none, func() error { return nil })
	})
	t.Run("postgres", func(t *testing.T) {
		check(t, only(integration.StorePostgres, 1), none, func() error {
			_, err := db.ExecContext(ctx, "SELECT 1")
			return err
		})
	})
	t.Run("postgres BEGIN and COMMIT count two", func(t *testing.T) {
		check(t, only(integration.StorePostgres, 2), none, func() error {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			return tx.Commit()
		})
	})
	t.Run("postgres on context.Background, on a goroutine the call waits for, counts async", func(t *testing.T) {
		check(t, none, only(integration.StorePostgres, 1), func() error {
			var err error
			startFakeLoop(func() { _, err = db.ExecContext(context.Background(), "SELECT 1") }).Wait()
			return err
		})
	})
	t.Run("postgres after the call returned counts async", func(t *testing.T) {
		done := make(chan error, 1)
		check(t, none, only(integration.StorePostgres, 1), func() error {
			go func() {
				_, err := db.ExecContext(context.Background(), "SELECT pg_sleep(0.05)")
				done <- err
			}()
			return nil
		})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("dgraph", func(t *testing.T) {
		check(t, only(integration.StoreDgraph, 1), none, func() error {
			_, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx, `{ q(func: uid(0x1)) { uid } }`)
			return err
		})
	})
	t.Run("opensearch", func(t *testing.T) {
		check(t, only(integration.StoreOpenSearch, 1), none, func() error {
			_, err := opensearchInit.OpenSearchClient.Info(ctx, nil)
			return err
		})
	})
	t.Run("redis", func(t *testing.T) {
		check(t, only(integration.StoreRedis, 1), none, func() error {
			return redisInit.RedisClient.Set(ctx, "storebudget:one", "1", 0).Err()
		})
	})
	t.Run("a redis pipeline is one round trip", func(t *testing.T) {
		check(t, only(integration.StoreRedis, 1), none, func() error {
			_, err := redisInit.RedisClient.Pipelined(ctx, func(p redis.Pipeliner) error {
				p.Get(ctx, "storebudget:one")
				p.Set(ctx, "storebudget:two", "2", 0)
				p.Del(ctx, "storebudget:one", "storebudget:two")
				return nil
			})
			return err
		})
	})
	t.Run("mqtt", func(t *testing.T) {
		check(t, only(integration.StoreMQTT, 1), none, func() error {
			tok := mqttInit.MqttClient.Publish("storebudget/one", 1, false, []byte("1"))
			tok.Wait()
			return tok.Error()
		})
	})
	t.Run("a background loop's calls are not the request's", func(t *testing.T) {
		const loop = "tests/integration_test.startFakeLoop"
		taps.AlsoExclude(loop)
		var callErr error
		w, err := taps.Measure(func() {
			startFakeLoop(func() { callErr = redisInit.RedisClient.Get(ctx, "storebudget:none").Err() }).Wait()
		})
		if err != nil {
			t.Fatal(err)
		}
		if callErr != nil && callErr != redis.Nil {
			t.Fatal(callErr)
		}
		// Only this loop's background calls are the subtest's: the window is
		// the process's, and the loops an earlier test's router.Routes started
		// (the import reaper's UPDATE every minute) land theirs there too.
		var own []string
		for _, c := range w.BackgroundCalls {
			if what, ok := strings.CutPrefix(c, loop+": "); ok {
				own = append(own, what)
			}
		}
		if w.Sync != none || w.Async != none || !slices.Equal(own, []string{"redis get"}) {
			t.Errorf("a loop's GET counted sync %v, async %v, and as the loop's own background calls %q (every background call: %q); want only the loop's redis get", w.Sync, w.Async, own, w.BackgroundCalls)
		}
	})
}

// Every loop router.Routes starts is one the harness tells apart from a
// request: a loop it does not know would land its calls in whatever row was
// open when it ticked.
func TestStoreBudgetsKnowEveryBackgroundLoop(t *testing.T) {
	src, err := os.ReadFile("../../router/router.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func startBackgroundLoops() {")
	if i < 0 {
		t.Fatal("router/router.go has no startBackgroundLoops: the harness's list of loops (notTheRequests) has to be found anew")
	}
	body = body[i : i+strings.Index(body[i:], "\n}\n")]
	starts := regexp.MustCompile(`\w+\.(Start\w+)\(\)`).FindAllStringSubmatch(body, -1)
	if len(starts) == 0 {
		t.Fatal("startBackgroundLoops starts nothing the guard can read")
	}
	known := integration.BackgroundLoops()
	for _, m := range starts {
		found := false
		for _, k := range known {
			found = found || strings.HasSuffix(k, "."+m[1])
		}
		if !found {
			t.Errorf("router.Routes starts %s, which the store-budget harness does not tell apart from a request: add it to notTheRequests in tests/integration/storebudget.go", m[1])
		}
	}
}

// budgetRow is a route the store budgets hold: one request, as the app sends
// it, by the fixture's viewer.
type budgetRow struct {
	name, method, path, body string
	apiToken                 bool // the viewer's API token (the /v1 API), not a session
	target                   int  // the audit's total for the route; -1 when it gave none
	after                    func(t *testing.T, call func(method, path, body string) (int, string))
	// before readies what the row reads, ahead of each of its calls (the
	// warm one and the measured one), outside the window.
	before func(t *testing.T)
}

// storeBudgetRuns is how many times the whole set runs, in one process: each
// row's sync counts must be the same every time; async is printed per run.
const storeBudgetRuns = 3

// What each route costs in store calls, held to testdata/store-budgets.tsv:
// a sync count that rises fails, and so does one that falls until the file is
// lowered (STORE_BUDGET_WRITE=1 rewrites it). STORE_BUDGET_AT=target holds
// each row's sync total to the audit's target instead. Async counts are
// printed per run and hold nothing (V-1121; see the run-to-run check).
func TestStoreBudgets(t *testing.T) {
	t.Setenv("JWT_SECRET", "store-budget-integration-jwt-secret")
	t.Setenv("CSRF_DISABLED", "true")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("the search indices: %v", err)
	}
	taps := integration.TapStores(t, env, dg, search)
	f, err := dgfixture.Write(ctx, dgfixture.Small, dgfixture.DefaultSeed, dgraphInit.DgraphClient, env.PG)
	if err != nil {
		t.Fatalf("the fixture: %v", err)
	}
	askable(t, env)
	// The fixture writes the graph and users; a channel post's row in
	// Postgres points at its channel's row, so #general gets one, as the app
	// makes it.
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'general', false)`, f.General); err != nil {
		t.Fatalf("#general's row: %v", err)
	}

	viewer := uuid.MustParse(f.Viewer.UUID)
	session, err := helpers.SignSessionToken(viewer.String(), helpers.TokenTypeAccess, time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	apiToken, err := apiTokenBusiness.CreateToken(ctx, "store budgets", apiTokenBusiness.AllScopes, 30, viewer, nil, false)
	if err != nil {
		t.Fatalf("the viewer's API token: %v", err)
	}
	routes := router.Routes()
	call := func(token bool, method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token {
			req.Header.Set("Authorization", "Bearer "+apiToken.Plaintext)
		} else {
			req.AddCookie(&http.Cookie{Name: "Authorization", Value: session})
			req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "store-budget-device"})
		}
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	asViewer := func(method, path, body string) (int, string) { return call(false, method, path, body) }

	owned := ownedDoc(t, f)
	// The viewer's own view of their doc, ended a minute ago: an owner opens it
	// anyway, after the doc is decided again from its side.
	ownedEnd := accessModels.Expiry{ResourceKind: "doc", ResourceUUID: uuid.MustParse(owned), PrincipalKind: "user",
		PrincipalUUID: viewer, Level: "view", ExpiresAt: time.Now().Add(-time.Minute)}
	task, from := boardTask(t, f)
	to := "inReview"
	if from == to {
		to = "todo"
	}
	move := func(status string) string {
		return fmt.Sprintf(`{"task_uuid":%q,"task_status":%q,"before_task_uuid":"","after_task_uuid":""}`, task, status)
	}
	text := `"<p>What the launch still needs</p>"`
	later := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) // an older page's edge past every message
	post := generalPost(t, f)
	rows := []budgetRow{
		{name: "createPost", method: "POST", path: "/po/createPost", body: `{"channel_id":"` + f.General + `","post_text_html":` + text + `}`, target: 6},
		{name: "dm createChat", method: "POST", path: "/dm/createChat", body: `{"to_uuid":"` + f.SuperPoster.UUID + `","text_html":` + text + `}`, target: -1},
		{name: "group createChat", method: "POST", path: "/groupChat/createChat", body: `{"grp_id":"` + f.ViewerGroup + `","text_html":` + text + `}`, target: -1},
		{name: "createComment", method: "POST", path: "/po/createComment", body: `{"post_id":"` + post + `","comment_text_html":` + text + `}`, target: -1},
		{name: "latestPosts", method: "GET", path: "/po/latestPosts/" + f.General, target: -1},
		{name: "dm latestChat", method: "GET", path: "/dm/latestChat/" + f.SuperPoster.UUID, target: 3},
		{name: "group latestChat", method: "GET", path: "/groupChat/latestChat/" + f.ViewerGroup, target: -1},
		{name: "sidebarNav", method: "GET", path: "/user/sidebarNav", target: 9},
		{name: "activity/unread", method: "GET", path: "/activity/unread", target: -1},
		{name: "activity/unified", method: "GET", path: "/activity/unified", target: -1},
		{name: "chat-threads/unread", method: "GET", path: "/chat-threads/unread", target: -1},
		// A search keeps the calendars its reader reaches for 30 s, until a
		// calendar writer moves ReachWrites: "search" is the first one, after a
		// write (an update that matches no calendar still moves it), and
		// "search, again" the next one, which reads no calendar.
		{name: "search", method: "GET", path: "/search/unifiedSearch/launch", target: 8,
			before: func(t *testing.T) {
				if err := calendarModels.UpdateCalendar(context.Background(), uuid.Nil, nil, nil); err != nil {
					t.Fatalf("moving ReachWrites before the first search: %v", err)
				}
			}},
		{name: "search, again", method: "GET", path: "/search/unifiedSearch/launch", target: -1},
		{name: "v1/unread", method: "GET", path: "/v1/unread", apiToken: true, target: 6},
		{name: "ai/ask", method: "POST", path: "/ai/ask", body: `{"question":"What does the launch still need?"}`, target: 14,
			before: func(t *testing.T) { askCorpus(t, f, post, task) }},
		{name: "moveTask", method: "POST", path: "/task/moveTask", body: move(to), target: 17,
			after: func(t *testing.T, call func(method, path, body string) (int, string)) {
				if code, out := call("POST", "/task/moveTask", move(from)); code != http.StatusOK {
					t.Fatalf("moving the task back answered %d: %.300s", code, out)
				}
			}},
		{name: "project list", method: "GET", path: "/project/taskList/" + f.BoardProject + "?pageSize=500&pageIndex=0", target: 5}, // the web task table's page
		{name: "project board", method: "GET", path: "/project/taskListForKanban/" + f.BoardProject, target: 7},
		// A conversation's older page and a message's thread (H5B): the
		// group's membership gate is read in the page's own query, a DM's
		// other person only when its page comes back empty.
		{name: "dm olderChats", method: "GET", path: "/dm/oldChats/" + f.SuperPoster.UUID + "/" + later, target: -1},
		{name: "group olderChats", method: "GET", path: "/groupChat/oldChats/" + f.ViewerGroup + "/" + later, target: -1},
		{name: "chat thread", method: "GET", path: "/dm/chatWithAllComments/" + groupChat(t, f), target: -1},
		// A doc open (H6 A2-2): the doc grants due to end are kept between
		// opens, so the measured open reads none; with a grant of the doc's
		// whose end has passed (written just before, so the open reads the due
		// grants again), the doc is decided again from its side.
		{name: "doc open", method: "GET", path: "/doc/getDocInfo/" + owned, target: -1},
		{name: "doc open, grant due", method: "GET", path: "/doc/getDocInfo/" + owned, target: -1,
			before: func(t *testing.T) {
				if err := accessModels.SetExpiry(ctx, ownedEnd); err != nil {
					t.Fatalf("a grant of the doc that has ended: %v", err)
				}
			},
			after: func(t *testing.T, _ func(method, path, body string) (int, string)) {
				if err := accessModels.ClearExpiry(ctx, "doc", []uuid.UUID{ownedEnd.ResourceUUID}, ownedEnd.PrincipalKind, ownedEnd.PrincipalUUID); err != nil {
					t.Fatalf("clearing the grant's end: %v", err)
				}
			}},
	}

	rows = append(rows, tableRows(t, viewer)...)

	// Nothing called, the window counts no sync call: no loop or leftover
	// work of the setup lands in a row's own goroutine. What it counts async
	// is printed, not held: under load the setup's background work lands in
	// it as a route's lands in the next row's (V-1121).
	idle, err := taps.Measure(func() {})
	if err != nil {
		t.Fatalf("an idle window: %v: the rows would carry work that is not theirs", err)
	}
	bad, note := integration.IdleWindow(idle)
	t.Log(note)
	if bad != "" {
		t.Fatal(bad)
	}

	// Each measured call follows the same call unmeasured, so what the
	// server keeps for a while (settings and webhook caches hold 30 s,
	// prepared statements) is warm: a budget is what a route costs a running
	// server, not its first call after start or after a cache lapsed. The
	// task-read decision's caches (models/taskread/generation) are the same
	// for every row: the generation moves before the warm-up, so the warm-up
	// reads the decision and fills them, and their clock is held until the
	// measured call is made, so on a loaded machine it still hits.
	measured := make([][]integration.Budget, storeBudgetRuns)
	windows := make([][]integration.Window, storeBudgetRuns)
	// A row that stops the test with the clock held still lets it go.
	t.Cleanup(func() { taskReadGeneration.HoldClockForTest()() })
	for run := range storeBudgetRuns {
		for _, r := range rows {
			if err := redisStore.BumpGeneration(context.Background(), registry.TaskReadGeneration, "storebudget warm-up"); err != nil {
				t.Fatalf("run %d, %s: moving the task-read generation: %v", run+1, r.name, err)
			}
			release := taskReadGeneration.HoldClockForTest()
			if r.before != nil {
				r.before(t)
			}
			if code, out := call(r.apiToken, r.method, r.path, r.body); code < 200 || code > 299 {
				release()
				t.Errorf("run %d, %s: warming up, %s %s answered %d: %.300s", run+1, r.name, r.method, r.path, code, out)
				continue
			}
			if r.after != nil {
				r.after(t, asViewer)
			}
			if r.before != nil {
				r.before(t)
			}
			var code int
			var out string
			w, err := taps.Measure(func() { code, out = call(r.apiToken, r.method, r.path, r.body) })
			release()
			if err != nil {
				t.Fatalf("run %d, %s: %v", run+1, r.name, err)
			}
			if code < 200 || code > 299 {
				t.Errorf("run %d, %s: %s %s answered %d, so its count is not the route's: %.300s", run+1, r.name, r.method, r.path, code, out)
				continue
			}
			if r.after != nil {
				r.after(t, asViewer)
			}
			measured[run] = append(measured[run], integration.Budget{Route: r.name, Sync: w.Sync, Async: w.Async.Total(), Target: r.target})
			windows[run] = append(windows[run], w)
		}
		if t.Failed() {
			return // every row that did not answer 2xx is named above
		}
	}
	// Each row's sync counts must be the same in every run. Async is not
	// compared, nor held to the file or a target: the taps count it by the
	// time window it lands in, not by the request that started it, so on a
	// loaded machine a route's background work finishes after its window
	// closed or lands in the next row's (b320, at load 17-29: dm createChat
	// async 7 then 5, sidebarNav 0 then 4, every sync count the same). It is
	// printed per run below. V-1121 tags async work with its request and
	// waits for it to drain; until then it is reported, not enforced.
	for run := 1; run < storeBudgetRuns; run++ {
		for i, m := range measured[run] {
			if bad := integration.Unrepeatable(run+1, measured[0][i], m, windows[0][i], windows[run][i]); bad != "" {
				t.Error(bad)
			}
		}
	}
	if t.Failed() {
		return
	}

	file := integration.StoreBudgetFile
	if os.Getenv(integration.StoreBudgetWriteEnv) == "1" {
		lines, _ := integration.ReadBudgets(file)
		for i, m := range measured[0] {
			if l, ok := lines[m.Route]; ok {
				measured[0][i].Note = l.Note
			}
		}
		if err := integration.WriteBudgets(file, measured[0]); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s: %d rows", file, len(measured[0]))
		return
	}
	lines, err := integration.ReadBudgets(file)
	if err != nil {
		t.Fatalf("%v (%s=1 writes it)", err, integration.StoreBudgetWriteEnv)
	}
	atTarget := os.Getenv(integration.StoreBudgetAtEnv) == "target"
	for i, m := range measured[0] {
		// Printed first, so a new route's count shows before its line is written.
		async := make([]string, storeBudgetRuns)
		for run := range measured {
			async[run] = fmt.Sprint(measured[run][i].Async)
		}
		t.Logf("%s: sync %v, async %s (reported, not enforced)", m.Route, m.Sync, strings.Join(async, "/"))
		line, ok := lines[m.Route]
		if !ok {
			t.Errorf("%s has no line in %s: %s=1 adds it", m.Route, file, integration.StoreBudgetWriteEnv)
			continue
		}
		delete(lines, m.Route)
		if atTarget {
			if over := integration.OverTarget(line, m); over != "" {
				t.Errorf("AT TARGET %s", over)
			}
			continue
		}
		for _, p := range integration.OverBudget(line, m) {
			t.Error(p)
		}
	}
	for route := range lines {
		t.Errorf("%s has a line in %s but no row: a row removed is a budget gone; %s=1 rewrites the file", route, file, integration.StoreBudgetWriteEnv)
	}
}

// boardTask is a live top-level task of the board project and its status,
// read from the project's own task edge (unmeasured, before any window), by
// uuid order: the fixture's uuids are the same on every load, its uids not,
// so every process moves the same task.
// ownedDoc is a live doc of the fixture's that the viewer owns.
func ownedDoc(t *testing.T, f *dgfixture.Fixture) string {
	t.Helper()
	got, err := docBusiness.DecideDocs(context.Background(), docBusiness.Viewer{UID: f.ViewerUID(), UUID: f.Viewer.UUID}, f.ViewerDocs)
	if err != nil {
		t.Fatalf("deciding the viewer's docs: %v", err)
	}
	for _, id := range f.ViewerDocs {
		if d := got[strings.ToLower(id)]; d.Owner && !d.Deleted {
			return id
		}
	}
	t.Fatalf("none of the fixture's %d docs is the viewer's own", len(f.ViewerDocs))
	return ""
}

func boardTask(t *testing.T, f *dgfixture.Fixture) (string, string) {
	t.Helper()
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(context.Background(),
		`query q($p: string) { q(func: eq(project_uuid, $p)) {
			project_tasks (orderasc: task_uuid, first: 200) @filter(not has(task_parent_task)) { task_uuid task_status task_deleted_at }
		} }`, map[string]string{"$p": f.BoardProject})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Q []struct {
			Tasks []struct {
				UUID    string    `json:"task_uuid"`
				Status  string    `json:"task_status"`
				Deleted time.Time `json:"task_deleted_at"`
			} `json:"project_tasks"`
		} `json:"q"`
	}
	if err := json.Unmarshal(resp.Json, &got); err != nil {
		t.Fatal(err)
	}
	for _, p := range got.Q {
		for _, task := range p.Tasks {
			if task.Deleted.Year() <= 1 && task.UUID != "" {
				return task.UUID, task.Status
			}
		}
	}
	t.Fatalf("the board project %s has no live top-level task among its first 200", f.BoardProject)
	return "", ""
}

// generalPost is a live post of #general, which every person can see.
// groupChat is a live message of the viewer's group, read straight from the
// graph (unmeasured, before any window): the thread the "chat thread" row
// opens.
func groupChat(t *testing.T, f *dgfixture.Fixture) string {
	t.Helper()
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(context.Background(),
		`query q($g: string) { q(func: eq(dm_grouping_id, $g)) { dm_chats (first: 50) { chat_uuid chat_deleted_at } } }`,
		map[string]string{"$g": f.ViewerGroup})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Q []struct {
			Chats []struct {
				UUID    string    `json:"chat_uuid"`
				Deleted time.Time `json:"chat_deleted_at"`
			} `json:"dm_chats"`
		} `json:"q"`
	}
	if err := json.Unmarshal(resp.Json, &got); err != nil {
		t.Fatal(err)
	}
	for _, g := range got.Q {
		for _, c := range g.Chats {
			if c.Deleted.Year() <= 1 && c.UUID != "" {
				return c.UUID
			}
		}
	}
	t.Fatal("the viewer's group has no live message")
	return ""
}

func generalPost(t *testing.T, f *dgfixture.Fixture) string {
	t.Helper()
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(context.Background(),
		`query q($c: string) { q(func: eq(ch_uuid, $c)) { ch_posts (first: 50) { post_uuid post_deleted_at } } }`,
		map[string]string{"$c": f.General})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Q []struct {
			Posts []struct {
				UUID    string    `json:"post_uuid"`
				Deleted time.Time `json:"post_deleted_at"`
			} `json:"ch_posts"`
		} `json:"q"`
	}
	if err := json.Unmarshal(resp.Json, &got); err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Q {
		for _, p := range c.Posts {
			if p.Deleted.Year() <= 1 && p.UUID != "" {
				return p.UUID
			}
		}
	}
	t.Fatalf("#general %s has no live post among its first 50", f.General)
	return ""
}

// askable turns the AI on with a stub model in place of a provider: an ask
// costs the server's own store calls and nothing of a model's.
func askable(t *testing.T, env *integration.Env) {
	t.Helper()
	const dim = askDim
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/embed":
			var in struct {
				Input any `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			n := 1
			if list, ok := in.Input.([]any); ok {
				n = len(list)
			}
			vecs := make([][]float32, n)
			for i := range vecs {
				vecs[i] = make([]float32, dim)
				vecs[i][0] = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vecs})
		default:
			_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"The launch needs its checklist signed off."},"done":true}`)
		}
	}))
	t.Cleanup(model.Close)
	t.Setenv("OLLAMA_HOST", model.URL)
	if _, err := env.PG.Exec(`UPDATE ai_settings SET embedding_dimension = $1, rate_limit_per_min = 1000, context_window_tokens = 32768`, dim); err != nil {
		t.Fatal(err)
	}
	if err := ai.InitAIService(); err != nil || !ai.GetService().IsEnabled() {
		t.Fatalf("the AI did not come up: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.PG.Exec(`UPDATE ai_settings SET enabled = false`)
		_ = ai.ReloadAIService(context.Background())
	})
	if err := opensearchInit.RecreateAIEmbeddingsIndex(context.Background(), dim); err != nil {
		t.Fatal(err)
	}
}

// askDim is the stub model's embedding size.
const askDim = 8

// askCorpus gives an ask the same things to find every time: an AI index
// holding the #general post and the board's task, and nothing else. The rows
// before the ask add posts, comments and chats on every run, and the stub
// model embeds every text alike, so which five of a growing index an ask got
// back changed from run to run, and the graph context and its liveness reads
// (a post or a task among the hits) came and went with them: ai/ask counted
// 15 async calls in one run and 7 in the next of the same process.
func askCorpus(t *testing.T, f *dgfixture.Fixture, post, task string) {
	t.Helper()
	ctx := context.Background()
	if err := opensearchInit.RecreateAIEmbeddingsIndex(ctx, askDim); err != nil {
		t.Fatal(err)
	}
	docs := []ai.EmbeddingDoc{
		{ContentText: "What the launch still needs", ContentType: "post", ContentUUID: post,
			ChannelUUID: f.General, ChannelName: "general", AuthorUUID: f.Viewer.UUID, AuthorName: f.Viewer.Name, CreatedDate: 1},
		{ContentText: "Sign off the launch checklist", ContentType: "task", ContentUUID: task, TaskUUID: task,
			ProjectUUID: f.BoardProject, TaskCreatedByUUID: f.Viewer.UUID, AuthorUUID: f.Viewer.UUID, AuthorName: f.Viewer.Name, CreatedDate: 1},
	}
	for _, d := range docs {
		if err := ai.StoreEmbedding(ctx, d); err != nil {
			t.Fatalf("the ask's %s: %v", d.ContentType, err)
		}
	}
	index := []string{opensearchInit.AIEmbeddingsIndexName}
	if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: index}); err != nil {
		t.Fatal(err)
	}
	resp, err := opensearchInit.OpenSearchClient.Indices.Count(ctx, &opensearchapi.IndicesCountReq{Indices: index})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Count != len(docs) {
		t.Fatalf("the ask's index holds %d things, not %d", resp.Count, len(docs))
	}
}

// tableRows are the table routes H7 holds, on the viewer's own small linked
// tables (the rows measure a route's shape, not its scale): orders link to a
// vendor and a project, and vendors sum their orders.
//   - "table rows page": the vendors' rows page (a relation, two rollups);
//   - "table paste": three orders pasted at once, each linking a vendor and
//     a project (the batch write);
//   - "table cell edit": an order's amount changed, after it was set to
//     another value outside the window.
func tableRows(t *testing.T, viewer uuid.UUID) []budgetRow {
	t.Helper()
	ctx := context.Background()
	me := dataTable.Actor{UserID: viewer}
	table := func(name string) (uuid.UUID, uuid.UUID) {
		tb, err := dataTable.CreateTable(ctx, dataTable.TableInput{Name: name, Visibility: "workspace"}, me)
		if err != nil {
			t.Fatalf("the %s table: %v", name, err)
		}
		fields, err := tableModel.ListFields(ctx, tb.Id)
		if err != nil || len(fields) == 0 {
			t.Fatalf("the %s table's name field: %v", name, err)
		}
		return tb.Id, fields[0].Id
	}
	field := func(tb uuid.UUID, name, typ string, cfg map[string]interface{}) uuid.UUID {
		f, err := dataTable.CreateField(ctx, tb, dataTable.FieldInput{Name: name, Type: typ, Config: cfg}, me)
		if err != nil {
			t.Fatalf("the %s field: %v", name, err)
		}
		return f.Id
	}
	row := func(tb uuid.UUID, values map[string]interface{}) uuid.UUID {
		r, err := dataTable.CreateRow(ctx, tb, dataTable.RowInput{Values: values}, me)
		if err != nil {
			t.Fatalf("a row: %v", err)
		}
		return r.Id
	}
	vendors, vName := table("Vendors")
	projects, pName := table("Projects")
	orders, oName := table("Orders")
	twoWay := func(to uuid.UUID) map[string]interface{} {
		return map[string]interface{}{"relation_target": "table", "table_id": to.String(), "two_way": true}
	}
	amount := field(orders, "Amount", "number", nil)
	vendor := field(orders, "Vendor", "relation", twoWay(vendors))
	project := field(orders, "Project", "relation", twoWay(projects))
	vFields, err := tableModel.ListFields(ctx, vendors)
	if err != nil {
		t.Fatalf("the vendors' fields: %v", err)
	}
	var vOrders uuid.UUID
	for _, f := range vFields {
		if f.Type == "relation" {
			vOrders = f.Id
		}
	}
	field(vendors, "Spend", "rollup", map[string]interface{}{"relation": vOrders.String(), "field": amount.String(), "aggregate": "sum"})
	field(vendors, "Orders made", "rollup", map[string]interface{}{"relation": vOrders.String(), "aggregate": "count"})
	acme, globex := row(vendors, map[string]interface{}{vName.String(): "Acme"}), row(vendors, map[string]interface{}{vName.String(): "Globex"})
	launch := row(projects, map[string]interface{}{pName.String(): "Launch"})
	var edited uuid.UUID
	for i, v := range []uuid.UUID{acme, globex, acme, globex} {
		edited = row(orders, map[string]interface{}{oName.String(): fmt.Sprintf("Order %d", i+1), amount.String(): 100 * (i + 1),
			vendor.String(): []interface{}{v.String()}, project.String(): []interface{}{launch.String()}})
	}
	paste, _ := json.Marshal(map[string]interface{}{"create": []interface{}{
		map[string]interface{}{"values": map[string]interface{}{oName.String(): "Pasted 1", amount.String(): 5, vendor.String(): []string{acme.String()}, project.String(): []string{launch.String()}}},
		map[string]interface{}{"values": map[string]interface{}{oName.String(): "Pasted 2", amount.String(): 6, vendor.String(): []string{globex.String()}, project.String(): []string{launch.String()}}},
		map[string]interface{}{"values": map[string]interface{}{oName.String(): "Pasted 3", amount.String(): 7, vendor.String(): []string{acme.String()}, project.String(): []string{launch.String()}}},
	}})
	return []budgetRow{
		{name: "table rows page", method: "POST", path: "/tables/" + vendors.String() + "/query", body: `{"n":50}`, target: -1},
		{name: "table paste", method: "POST", path: "/tables/" + orders.String() + "/rows/batch", body: string(paste), target: -1},
		{name: "table cell edit", method: "POST", path: "/tables/" + orders.String() + "/rows/" + edited.String() + "/patch",
			body: `{"values":{"` + amount.String() + `":42}}`, target: -1,
			before: func(t *testing.T) {
				if _, err := dataTable.PatchRow(ctx, orders, edited, map[string]interface{}{amount.String(): 41}, me); err != nil {
					t.Fatalf("setting the amount the edit changes: %v", err)
				}
			}},
	}
}
