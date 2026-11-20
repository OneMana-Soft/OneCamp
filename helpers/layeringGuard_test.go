package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// The owner (16 Nov 2026): "in backend make sure DB query is written in domain
// layer and executed in model layer". This ratchet holds where query text lives
// and which layer touches a store driver, file by file, to a baseline that may
// only shrink (checkRatchet in codeRatchet_test.go):
//
//	SQL      a SQL statement (or a constant concatenation that folds to one)
//	SQLF     a SQL fragment: a clause with a $n placeholder, an IS NULL test on a
//	         time column, ON CONFLICT, LEFT JOIN, UNION ALL SELECT
//	DQL      a Dgraph query, upsert block or N-Quad mutation text
//	DRVIMP   an import of a store driver package
//	DRVCALL  a driver method called on a store value (tx.ExecContext, txn.Mutate,
//	         rdb.Get in a file that imports a driver)
//
// Text belongs in domain/ (and the generic executors' own text in models/pgexec
// and models/dgexec); drivers belong in models/ and initializers/. Everything
// else is a baseline line. How it differs from executorGuard_test.go:
// handExecutorBaseline counts FUNCTIONS that run SQL/DQL themselves instead of
// through pgexec/dgexec; this counts where the TEXT is written and which LAYER
// touches a driver. A models/postgres function that hands its own SQL to
// pgexec.Query passes the executor guard and is still a line here.
//
// The classifier is the one BACKEND-OPT-LEAD-1's scan used for the 16 Nov audit
// (release-1010/design/backend-opt/scan/layering/main.go), with one change: a
// string that opens with a Title-case keyword ("Select a project from the
// list") is prose, not a statement. Starting baseline at 86364bc48 (18 Nov
// 2026): 2,181 in 404 lines, 1,918 of them text in models/ feature packages.

const layeringBaseline = "testdata/layering-baseline.txt"

const layeringHeader = "# Query text and store drivers outside their layer (owner, 16 Nov 2026: query text in\n" +
	"# domain/, executed in models/): \"path class count\". Move the text to domain/<X> and run it\n" +
	"# through models/pgexec or models/dgexec, then lower the line (WRITE_LAYERING_BASELINE=1).\n"

// layeringExempt are files whose query text is data for another engine, each
// with its reason. They are listed by path, never by pattern, and each must
// still exist so a rename can't carry the exemption to nothing.
var layeringExempt = map[string]string{
	"business/DataSource/aggregate.go": "SQL for a customer's external database, per dialect",
	"business/DataSource/connector.go": "SQL for a customer's external database, per dialect",
	"business/DataSource/queryplan.go": "SQL for a customer's external database, per dialect",
	"business/RowQuery/compile.go":     "a table filter compiled to SQL text that domain/DataTable runs",
}

func TestQueryTextLivesInDomainAndRunsInModels(t *testing.T) {
	found := map[string]int{}
	seen := map[string]bool{}
	for _, path := range productionGoFiles(t) {
		rel := filepath.ToSlash(strings.TrimPrefix(path, "../"))
		if _, ok := layeringExempt[rel]; ok {
			seen[rel] = true
			continue
		}
		for k, n := range layeringFindings(t, path, rel) {
			found[k] += n
		}
	}
	for rel, why := range layeringExempt {
		if !seen[rel] {
			t.Errorf("layeringExempt lists %s (%s), which no longer exists: drop the line", rel, why)
		}
	}
	checkRatchet(t, found, layeringBaseline, layeringHeader, "WRITE_LAYERING_BASELINE",
		"write the query text in domain/<X> and run it through models/pgexec or models/dgexec; "+
			"business/ and controllers/ never hold SQL/DQL or call a driver; models/ runs text it is given")
}

// TestLayeringRatchetSeesANewQueryInBusiness: one file with a SQL string, a
// driver import and a driver call is three lines in business/, two in domain/
// (text is allowed there), one in a models/ feature package (the text) and none
// in the generic executor.
func TestLayeringRatchetSeesANewQueryInBusiness(t *testing.T) {
	const src = `package fixture

import (
	"context"
	"database/sql"
)

const listTasks = "SELECT id FROM tasks WHERE project_id = $1"

func run(ctx context.Context, db *sql.DB, id string) { _, _ = db.QueryContext(ctx, listTasks, id) }
`
	path := filepath.Join(t.TempDir(), "fixture.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		rel  string
		want []string
	}{
		{"business/Fixture/fixture.go", []string{"SQL", "DRVIMP", "DRVCALL"}},
		{"controllers/Fixture/fixture.go", []string{"SQL", "DRVIMP", "DRVCALL"}},
		{"domain/Fixture/fixture.go", []string{"DRVIMP", "DRVCALL"}},
		{"models/postgres/Fixture/fixture.go", []string{"SQL"}},
		{"models/pgexec/fixture.go", nil},
	}
	for _, c := range cases {
		got := layeringFindings(t, path, c.rel)
		want := map[string]int{}
		for _, class := range c.want {
			want[c.rel+" "+class] = 1
		}
		if len(got) != len(want) {
			t.Errorf("%s: found %v, want %v", c.rel, got, want)
			continue
		}
		for k, n := range want {
			if got[k] != n {
				t.Errorf("%s: found %v, want %v", c.rel, got, want)
				break
			}
		}
	}
}

// TestLayeringClassifierExamples pins what counts as query text and as a driver
// call, so prose, gin's c.Query and a vendor's GraphQL never become lines.
func TestLayeringClassifierExamples(t *testing.T) {
	texts := []struct{ s, want string }{
		{"SELECT id FROM tasks WHERE project_id = $1", "SQL"},
		{"select count(*) from tasks", "SQL"},
		{"INSERT INTO task_changes (task_id) VALUES ($1)", "SQL"},
		{"WITH RECURSIVE tree AS (SELECT 1)", "SQL"},
		{" AND deleted_at IS NULL", "SQLF"},
		{"ON CONFLICT (id) DO NOTHING", "SQLF"},
		{`upsert { query { v as var(func: eq(email, "a")) } mutation { set { uid(v) <name> "a" . } } }`, "DQL"},
		{`uid(v) <task_name> "x" .`, "DQL"},
		{`{ q(func: uid($id)) @filter(has(task_name)) { uid } }`, "DQL"},
		{"Select a project from the list", ""},
		{"Update the task set by the import", ""},
		{"query($after: String) { issues(first: 50, after: $after) { nodes { id title } } }", ""},
		{"a short", ""},
	}
	for _, c := range texts {
		if got := layeringClassify(c.s); got != c.want {
			t.Errorf("layeringClassify(%q) = %q, want %q", c.s, got, c.want)
		}
	}

	calls := []struct {
		name, src string
		want      int
	}{
		{"gin's c.Query is a request read", `package p
import "github.com/gin-gonic/gin"
func f(c *gin.Context) { _ = c.Query("filter_value") }`, 0},
		{"url values in a driver file", `package p
import ("database/sql"; "net/http")
var _ *sql.DB
func f(r *http.Request) { _ = r.URL.Query().Get("filter_value") }`, 0},
		{"a package function is an executor", `package p
import ("context"; "github.com/jackc/pgx/v5"; "onecamp/models/pgexec")
var _ pgx.Row
func f(ctx context.Context) { _ = pgexec.Exec(ctx, "", nil) }`, 0},
		{"tx.ExecContext", `package p
import ("context"; "database/sql")
func f(ctx context.Context, tx *sql.Tx) { _, _ = tx.ExecContext(ctx, "") }`, 1},
		{"NewTxn and txn.Mutate on a graph client", `package p
import ("context"; "github.com/dgraph-io/dgo/v240"; "github.com/dgraph-io/dgo/v240/protos/api")
func f(ctx context.Context, c *dgo.Dgraph, mu *api.Mutation) { txn := c.NewTxn(); _, _ = txn.Mutate(ctx, mu) }`, 2},
		{"rdb.Get in a redis file", `package p
import ("context"; "github.com/redis/go-redis/v9")
func f(ctx context.Context, rdb *redis.Client) { _ = rdb.Get(ctx, "k") }`, 1},
		{"Get on a value not named like a store", `package p
import "github.com/redis/go-redis/v9"
var _ *redis.Client
func f(cache interface{ Get(string) string }) { _ = cache.Get("k") }`, 0},
	}
	for _, c := range calls {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", c.src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := layeringCounts(f)["DRVCALL"]; got != c.want {
			t.Errorf("%s: DRVCALL %d, want %d", c.name, got, c.want)
		}
	}
}

// layeringFindings parses one file and returns its "rel class" counts that sit
// outside the layer allowed to hold them.
func layeringFindings(t *testing.T, path, rel string) map[string]int {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.ToSlash(filepath.Dir(rel))
	out := map[string]int{}
	for class, n := range layeringCounts(f) {
		if !layeringAllowed(dir, class) {
			out[rel+" "+class] += n
		}
	}
	return out
}

// layeringAllowed: domain/ may hold text; models/ and initializers/ may touch
// drivers; models/pgexec and models/dgexec may hold their own generic text.
func layeringAllowed(dir, class string) bool {
	layer := strings.SplitN(dir, "/", 2)[0]
	switch class {
	case "SQL", "SQLF", "DQL":
		return layer == "domain" || dir == "models/pgexec" || dir == "models/dgexec"
	default: // DRVIMP, DRVCALL
		return layer == "models" || layer == "initializers"
	}
}

var (
	layeringSQLStmt = regexp.MustCompile(`(?is)^\s*(?:--[^\n]*\n\s*)*(` +
		`select\s.+?\sfrom\s|select\s+(?:exists|count|coalesce|pg_|nextval|now\(|1\b|\$\d)|` +
		`insert\s+into\s+\w|update\s+\w+(?:\s+\w+)?\s+set\s|delete\s+from\s+\w|` +
		`with\s+(?:recursive\s+)?\w+(?:\s*\([^)]*\))?\s+as\s*(?:materialized\s*|not\s+materialized\s*)?\(|` +
		`create\s+(?:unique\s+|temp\w*\s+|or\s+replace\s+)?(?:index|table|extension|function|trigger|view|sequence|type)\b|` +
		`alter\s+(?:table|index|sequence|type)\s|drop\s+(?:table|index|function|trigger|view|type)\s|` +
		`values\s*\(\s*\$\d)`)
	layeringSQLUpper = regexp.MustCompile(`^\s*(?:TRUNCATE\s|VACUUM\b|ANALYZE\s+\w|COPY\s+\w+.*\b(?:FROM|TO)\b|LOCK\s+TABLE\s|SET\s+LOCAL\s)`)
	layeringSQLFrag  = regexp.MustCompile(`(?is)(\b(?:where|and|or|on|set|limit|offset|order\s+by|group\s+by|returning|join)\b[^"]*\$\d)|` +
		`(\b(?:deleted_at|deleted_time|_at)\s+is\s+(?:not\s+)?null\b)|(\bon\s+conflict\b)|(\bleft\s+join\s+\w+)|(\bunion\s+all\s+select\b)`)
	layeringDQL = regexp.MustCompile(`(?s)(func:\s*\w+\()|(@filter\s*\()|(@recurse)|(\bupsert\s*\{)|(@cascade)|` +
		`(\buid\(\s*\w+\s*\)\s+<[\w.~]+>)|(<_:\w+>\s+<[\w.~]+>)|(_:\w+\s+<[\w.~]+>)|(dgraph\.type)`)
	layeringStoreName = regexp.MustCompile(`^(tx|txn|db|conn|rdb|rc|cli|client|pipe|pool|pg|redis\w*|\w*Tx|\w*Txn|\w*DB|\w*Db|\w*Client|\w*Redis|\w*Conn)$`)
)

// layeringDriverImports are the store driver packages (object storage and the
// broker count as drivers but don't make Get/Set/Exec a database call).
var layeringDriverImports = []string{
	"database/sql", "github.com/jackc/pgx", "github.com/lib/pq", "github.com/jmoiron/sqlx",
	"github.com/dgraph-io/dgo", "github.com/opensearch-project/opensearch-go", "github.com/redis/go-redis",
	"github.com/go-redis/redis", "github.com/ClickHouse/clickhouse-go", "github.com/minio/minio-go",
	"github.com/eclipse/paho.mqtt.golang",
}

// layeringDriverCalls are the executor guard's driver methods plus the ones
// that prepare or pipeline. layeringAmbiguous names count only on a store-named
// value in a file that imports a database driver (gin's c.Query isn't one).
var (
	layeringDriverCalls = func() map[string]bool {
		m := map[string]bool{"PrepareContext": true, "Pipelined": true, "TxPipelined": true}
		for name := range executorCalls {
			m[name] = true
		}
		return m
	}()
	layeringAmbiguous = map[string]bool{"Commit": true, "Rollback": true, "Exec": true, "Query": true, "QueryRow": true, "Get": true, "Set": true, "Del": true, "Mutate": true}
)

// layeringCounts is the class -> n count of one parsed file.
func layeringCounts(f *ast.File) map[string]int {
	c := map[string]int{}
	pkgIdents := map[string]bool{}
	hasDB := false
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		name := p[strings.LastIndex(p, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		pkgIdents[name] = true
		for _, d := range layeringDriverImports {
			if !strings.HasPrefix(p, d) {
				continue
			}
			c["DRVIMP"]++
			if !strings.Contains(d, "minio") && !strings.Contains(d, "paho") {
				hasDB = true
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.BinaryExpr, *ast.BasicLit:
			s, ok := layeringFold(v.(ast.Expr))
			if !ok {
				return true
			}
			if class := layeringClassify(s); class != "" {
				c[class]++
			}
			return false
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := sel.Sel.Name
			if !layeringDriverCalls[name] && !(hasDB && layeringAmbiguous[name] && layeringStoreValue(sel.X)) {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && (pkgIdents[id.Name] || (id.Name == "c" && !layeringDriverCalls[name])) {
				return true // a package function (pgexec.Exec, dgexec.Mutate), or gin's c.Get
			}
			c["DRVCALL"]++
		}
		return true
	})
	return c
}

// layeringStoreValue: an ambiguous method counts only on a value named like a
// store handle (tx, txn, db, conn, rdb, client, pipe, ...Tx, ...DB).
func layeringStoreValue(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return layeringStoreName.MatchString(v.Name)
	case *ast.SelectorExpr:
		return layeringStoreName.MatchString(v.Sel.Name)
	}
	return false
}

// layeringFold folds a string literal or a + concatenation into its text; a
// partly constant concatenation folds what it can (a query with a variable
// spliced in still reads as a query).
func layeringFold(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		a, ok1 := layeringFold(v.X)
		b, ok2 := layeringFold(v.Y)
		if ok1 && ok2 {
			return a + b, true
		}
		return a + " " + b, ok1 || ok2
	case *ast.ParenExpr:
		return layeringFold(v.X)
	}
	return "", false
}

// layeringClassify names the query class of one folded string, or "".
func layeringClassify(s string) string {
	if len(s) < 8 {
		return ""
	}
	if layeringDQL.MatchString(s) && (strings.Contains(s, "{") || strings.Contains(s, "<")) {
		return "DQL"
	}
	if !layeringProse(s) && (layeringSQLStmt.MatchString(s) || layeringSQLUpper.MatchString(s)) {
		return "SQL"
	}
	if layeringSQLFrag.MatchString(s) {
		return "SQLF"
	}
	return ""
}

// layeringProse: a string that opens with a Title-case word ("Select a
// project", "Update the task") is a sentence; SQL keywords are all upper or all
// lower case.
func layeringProse(s string) bool {
	w := strings.Fields(s)
	if len(w) == 0 {
		return false
	}
	r := []rune(w[0])
	return len(r) > 1 && unicode.IsUpper(r[0]) && strings.ToLower(string(r[1:])) == string(r[1:]) && unicode.IsLetter(r[1])
}
