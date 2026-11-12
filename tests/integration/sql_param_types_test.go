//go:build integration
// +build integration

package integration_test

// Every SQL statement the server writes out whole must be one Postgres can
// prepare without being told its parameters' types (V-705). Postgres types a
// parameter from where it first meets it; one first met where nothing types it
// ($4 IS NULL, CASE WHEN $4 IS NULL ... ELSE $4, jsonb_build_object('k', $1))
// fails at prepare with 42P08 or 42P18, at run time, on every call, and a
// caller that only logs the error hides it (UpdateTaskPRState did, for every
// pull request's state, checks and review). The parse order isn't the text's
// (an UPDATE reads its WHERE before its SET), so a grep can't tell: this asks
// the Postgres the server runs on (12, as the harness does), against the
// migrated schema.
//
// It reads every non-test Go file of the backend and prepares each string
// constant, or constant concatenation, that is a whole statement (SELECT,
// INSERT, UPDATE, DELETE or WITH) with a $n parameter. A statement built at
// run time (fmt verbs, strings.Builder) isn't seen; one that fails for any
// other reason (a fragment, a temporary table) is counted, not judged.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <tree> <log> '^TestEveryStatementsParametersHaveATypePostgresCanTell$'

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/akashc777/OneCamp/tests/integration"
)

func TestEveryStatementsParametersHaveATypePostgresCanTell(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	conn, err := pgconn.Connect(ctx, env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	statements := backendStatements(t)
	prepared, other, sawPRState := 0, map[string]int{}, false
	for _, s := range statements {
		_, err := conn.Prepare(ctx, "", s.sql, nil)
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			prepared++
			sawPRState = sawPRState || (strings.HasSuffix(s.at, "domain/Task/taskDomain.go") && strings.Contains(s.sql, "github_pr_is_draft"))
		case errors.As(err, &pgErr) && (pgErr.Code == "42P08" || pgErr.Code == "42P18"):
			t.Errorf("%s:%d: Postgres can't type a parameter (%s %s): %s", s.at, s.line, pgErr.Code, pgErr.Message, strings.Join(strings.Fields(s.sql), " "))
		case errors.As(err, &pgErr):
			other[pgErr.Code]++
		default:
			t.Fatalf("%s:%d: %v", s.at, s.line, err)
		}
	}
	t.Logf("%d statements with parameters: %d prepared, others failed for reasons this doesn't judge %v", len(statements), prepared, other)
	// The walk found the backend: hundreds of statements, the pull request
	// state's write among them.
	if prepared < 500 || !sawPRState {
		t.Errorf("prepared %d statements (want 500 or more), UpdateTaskPRState's among them: %v", prepared, sawPRState)
	}
}

type backendStatement struct {
	at   string
	line int
	sql  string
}

var (
	statementStart = regexp.MustCompile(`(?is)^\s*(--[^\n]*\n\s*)*(SELECT|INSERT|UPDATE|DELETE|WITH)\b`)
	sqlParameter   = regexp.MustCompile(`\$[0-9]`)
	fmtVerb        = regexp.MustCompile(`%[-+# 0-9.*]*[sdvqTtxXfgw]`)
)

// backendStatements is every whole statement with a parameter that the
// backend's non-test Go files spell as a constant, by file (in lexical order)
// and line.
func backendStatements(t *testing.T) []backendStatement {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the backend's root %s: %v", root, err)
	}
	var out []backendStatement
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (name == "vendor" || name == "node_modules" || name == "tests" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			expr, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			s, whole := constantString(expr)
			if !whole {
				return true
			}
			if statementStart.MatchString(s) && sqlParameter.MatchString(s) && !fmtVerb.MatchString(s) {
				out = append(out, backendStatement{at: rel, line: fset.Position(expr.Pos()).Line, sql: s})
			}
			return false
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// constantString is a string literal's value, or a "+" of them, parenthesised
// or not; whole is false for anything else.
func constantString(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return constantString(e.X)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, ok := constantString(e.X)
		if !ok {
			return "", false
		}
		r, ok := constantString(e.Y)
		return l + r, ok
	}
	return "", false
}
