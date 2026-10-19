//go:build integration
// +build integration

package integration

// Proof that the shared servers (shared.go) hide nothing a fresh one per test
// would show.
//
// Two canaries, A and B, run in the same binary in whatever order -shuffle
// gives them. Each first looks at what it was handed: a database of its own
// and no other test's (every earlier one dropped), a graph with no node of the
// project's, a search node with no index a new node lacks, an empty Redis, and
// project clients left by earlier tests that reach nothing. Then it checks
// that the other canary's markers, if it has run, are gone, and leaves
// markers of its own in all four stores. Whichever runs second checks the
// first's.
//
// MinIO isn't shared: the tests that store files start a MinIO of their own
// and remove it, so it needs no canary.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
)

// canaryMarks are what a canary left in each store.
type canaryMarks struct {
	database, graphUUID, index, key string
}

var canaries struct {
	mu   sync.Mutex
	left map[string]canaryMarks
}

func TestLeakCanaryA(t *testing.T) { leakCanary(t, "a", "b") }
func TestLeakCanaryB(t *testing.T) { leakCanary(t, "b", "a") }

func leakCanary(t *testing.T, me, other string) {
	ctx := context.Background()

	// What earlier tests left in the project's clients reaches nothing.
	if c := opensearchInit.OpenSearchClient; c != nil {
		if _, err := c.Cluster.Health(ctx, nil); err == nil {
			t.Error("the search client an earlier test left still reaches a search node")
		}
	}
	if p := postgresInit.DBConn.SqlDB; p != nil {
		if err := p.PingContext(ctx); err == nil {
			t.Error("the project's database pool an earlier test left still reaches a database")
		}
	}
	if c := dgraphInit.DgraphClient; c != nil {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := c.NewReadOnlyTxn().Query(qctx, `{ q(func: has(dgraph.type), first: 1) { uid } }`)
		cancel()
		if err == nil {
			t.Error("the graph client an earlier test left still reaches a graph")
		}
	}
	if redisInit.RedisClient != nil {
		t.Error("the cache client an earlier test left is still set")
	}

	env := SetupEnv(t)
	SetupDgraph(t)
	SetupOpenSearch(t)
	SetupRedis(t)
	pg := env.server
	search := theOpenSearch.current()
	cache := redisInit.RedisClient

	// Nothing from any earlier test.
	var dbs []string
	rows, err := pg.admin.QueryContext(ctx, `SELECT datname FROM pg_database WHERE datname LIKE 'test\_%' ORDER BY datname`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		dbs = append(dbs, n)
	}
	rows.Close()
	if len(dbs) != 1 || dbs[0] != env.database {
		t.Errorf("databases of tests on the server: %v, want only this test's %s", dbs, env.database)
	}
	var tableLeft bool
	if err := env.PG.QueryRowContext(ctx, `SELECT to_regclass('public.leak_canary') IS NOT NULL`).Scan(&tableLeft); err != nil || tableLeft {
		t.Errorf("a canary's table is in a new database (%v): the template took a test's change", err)
	}
	if types := graphTypes(t); len(types) != 0 {
		t.Errorf("the graph handed to the test holds nodes: %v", types)
	}
	names, err := search.indices()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if !search.fresh[n] && !strings.HasPrefix(n, ".") {
			t.Errorf("the search node handed to the test holds the index %s", n)
		}
	}
	if n, err := cache.DBSize(ctx).Result(); err != nil || n != 0 {
		t.Errorf("the cache handed to the test holds %d keys (%v)", n, err)
	}

	// The other canary's markers, if it has run.
	canaries.mu.Lock()
	theirs, ran := canaries.left[other]
	canaries.mu.Unlock()
	if ran {
		failed := t.Failed()
		var exists bool
		if err := pg.admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, theirs.database).Scan(&exists); err != nil || exists {
			t.Errorf("canary %s's database %s is still there (%v)", other, theirs.database, err)
		}
		if n := graphCount(t, theirs.graphUUID); n != 0 {
			t.Errorf("canary %s's graph node is still there (%d)", other, n)
		}
		for _, n := range names {
			if n == theirs.index {
				t.Errorf("canary %s's index %s is still there", other, n)
			}
		}
		if n, err := cache.Exists(ctx, theirs.key).Result(); err != nil || n != 0 {
			t.Errorf("canary %s's cache key is still there (%d, %v)", other, n, err)
		}
		if !t.Failed() || failed {
			t.Logf("canary %s's markers in Postgres, Dgraph, OpenSearch and Redis: checked", other)
		}
	} else {
		t.Logf("canary %s hasn't run yet; it will look for these markers", other)
	}

	// Markers of this canary's own, read back so the check above isn't
	// vacuous for the other.
	mine := canaryMarks{database: env.database, graphUUID: "leak-canary-" + me + "-" + uuid.NewString(), index: "leak-canary-" + me, key: "leak-canary:" + me}
	if _, err := env.PG.ExecContext(ctx, `CREATE TABLE leak_canary (who text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.ExecContext(ctx, `INSERT INTO leak_canary VALUES ($1)`, me); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"uid": "_:c", "user_uuid": mine.graphUUID, "dgraph.type": "User"})
	if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(ctx, &api.Mutation{SetJson: raw, CommitNow: true}); err != nil {
		t.Fatal(err)
	}
	if n := graphCount(t, mine.graphUUID); n != 1 {
		t.Fatalf("the canary's graph node reads back %d times", n)
	}
	if _, err := opensearchInit.OpenSearchClient.Index(ctx, opensearchapi.IndexReq{
		Index: mine.index, Body: strings.NewReader(`{"who":"` + me + `"}`), Params: opensearchapi.IndexParams{Refresh: "true"},
	}); err != nil {
		t.Fatal(err)
	}
	if names, err := search.indices(); err != nil || !hasName(names, mine.index) {
		t.Fatalf("the canary's index isn't listed (%v)", err)
	}
	if err := cache.Set(ctx, mine.key, me, 0).Err(); err != nil {
		t.Fatal(err)
	}
	canaries.mu.Lock()
	if canaries.left == nil {
		canaries.left = map[string]canaryMarks{}
	}
	canaries.left[me] = mine
	canaries.mu.Unlock()
}

// graphTypes is the types of every node in the graph but Dgraph's own.
func graphTypes(t *testing.T) []string {
	t.Helper()
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(context.Background(),
		`{ q(func: has(dgraph.type)) { uid dgraph.type } }`)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Q []struct {
			Types []string `json:"dgraph.type"`
		} `json:"q"`
	}
	if err := json.Unmarshal(resp.GetJson(), &out); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, n := range out.Q {
		for _, ty := range n.Types {
			if !strings.HasPrefix(ty, "dgraph.") {
				types = append(types, ty)
			}
		}
	}
	sort.Strings(types)
	return types
}

// graphCount is how many nodes carry the uuid.
func graphCount(t *testing.T, id string) int {
	t.Helper()
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(context.Background(),
		`query q($id: string) { q(func: eq(user_uuid, $id)) { count(uid) } }`, map[string]string{"$id": id})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Q []struct {
			Count int `json:"count"`
		} `json:"q"`
	}
	if err := json.Unmarshal(resp.GetJson(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Q) == 0 {
		return 0
	}
	return out.Q[0].Count
}

func hasName(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// testFiles parses this directory's test files.
func testFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(here), "*_test.go"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no test files beside %s (%v)", here, err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, p := range paths {
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Base(p)] = f
	}
	return fset, files
}

// TestTheTestsThatApplyMigrationsGetADatabaseMigratedFromBlank: a test that
// applies migrations itself (golang-migrate, or a migration's SQL read from
// migrations/) gets an empty database with every migration applied to it in
// the test (migratesItself), never a copy of the template. The list is the
// harness's; this reads the test files for the tests that need it.
func TestTheTestsThatApplyMigrationsGetADatabaseMigratedFromBlank(t *testing.T) {
	_, files := testFiles(t)
	direct := map[string]string{}
	calls := map[string]map[string]bool{}
	var tests []string
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Recv != nil {
				continue
			}
			name := fd.Name.Name
			if name == "TestTheTestsThatApplyMigrationsGetADatabaseMigratedFromBlank" {
				continue // its patterns are what it looks for, not migrations it applies
			}
			if strings.HasPrefix(name, "Test") && name != "TestMain" {
				tests = append(tests, name)
			}
			calls[name] = map[string]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if se, ok := x.Fun.(*ast.SelectorExpr); ok {
						if id, ok := se.X.(*ast.Ident); ok && id.Name == "migrate" && se.Sel.Name == "New" {
							direct[name] = "golang-migrate"
						}
					}
					if id, ok := x.Fun.(*ast.Ident); ok {
						calls[name][id.Name] = true
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING && (strings.Contains(x.Value, ".sql") || x.Value == `"migrations"`) && direct[name] == "" {
						direct[name] = "a migration's SQL"
					}
				}
				return true
			})
		}
	}
	var reaches func(fn string, seen map[string]bool) bool
	reaches = func(fn string, seen map[string]bool) bool {
		if seen[fn] {
			return false
		}
		seen[fn] = true
		if direct[fn] != "" {
			return true
		}
		for c := range calls[fn] {
			if reaches(c, seen) {
				return true
			}
		}
		return false
	}
	var missing, extra []string
	for _, name := range tests {
		if reaches(name, map[string]bool{}) && !migratesItself[name] {
			missing = append(missing, name)
		}
	}
	for name := range migratesItself {
		if !hasName(tests, name) || !reaches(name, map[string]bool{}) {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("these apply migrations themselves but would get a copy of the template; add them to migratesItself (harness.go): %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("migratesItself names tests that don't apply migrations (or don't exist): %v", extra)
	}
	t.Logf("%d tests apply migrations themselves, each on a database migrated from blank", len(migratesItself))
}

// TestTheTestsPutBackWhatTheyChange: a test that sets another package's
// variable (a client, a switch, a seam) puts it back in the same function, in
// a t.Cleanup or a defer. The one exception: the 46 tests that point
// opensearchInit.OpenSearchClient at a stub of their own, an httptest server
// that answers "ok", holds nothing and is closed when the test ends, so what
// they leave behind refuses every connection, as a fresh binary's would have
// no client to make one with.
func TestTheTestsPutBackWhatTheyChange(t *testing.T) {
	fset, files := testFiles(t)
	allowed := map[string]bool{"opensearchInit.OpenSearchClient": true}
	var left []string
	for _, f := range files {
		imports := map[string]bool{}
		for _, im := range f.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			name := p[strings.LastIndex(p, "/")+1:]
			if im.Name != nil {
				name = im.Name.Name
			}
			imports[name] = true
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			restored := map[string]bool{}
			undo := func(n ast.Node) {
				ast.Inspect(n, func(m ast.Node) bool {
					if as, ok := m.(*ast.AssignStmt); ok {
						for _, l := range as.Lhs {
							if s := packageVar(l, imports); s != "" {
								restored[s] = true
							}
						}
					}
					return true
				})
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.DeferStmt:
					undo(x.Call)
				case *ast.CallExpr:
					if se, ok := x.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "Cleanup" {
						for _, a := range x.Args {
							undo(a)
						}
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if as, ok := n.(*ast.AssignStmt); ok && as.Tok == token.ASSIGN {
					for _, l := range as.Lhs {
						if s := packageVar(l, imports); s != "" && !restored[s] && !allowed[s] {
							left = append(left, fmt.Sprintf("%s: %s", fset.Position(as.Pos()), s))
						}
					}
				}
				return true
			})
		}
	}
	sort.Strings(left)
	for _, l := range left {
		t.Errorf("set and not put back by its function: %s", l)
	}
}

// packageVar is "pkg.Var" when e assigns to an imported package's variable
// (or a field of one).
func packageVar(e ast.Expr, imports map[string]bool) string {
	for {
		se, ok := e.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		if id, ok := se.X.(*ast.Ident); ok {
			if imports[id.Name] {
				return id.Name + "." + se.Sel.Name
			}
			return ""
		}
		e = se.X
	}
}

// startEnviron is the environment as the binary started, for TestMain's
// last check.
var startEnviron = os.Environ()
