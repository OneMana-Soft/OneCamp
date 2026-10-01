package helpers

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A shared column list must have a Scan with the same number of destinations.
//
// WHY THIS EXISTS. Migration 137 added delegation_hop and delegation_chain to ai_agent_tasks.
// agentTaskColumns was extended to select them. scanAgentTask was not extended to receive them.
// Go cannot catch that: Scan takes ...any, so 23 destinations for 25 columns compiles cleanly and
// fails only when a row is read —
//
//	agentTaskWorker: claim failed: sql: expected 25 destination arguments in Scan, not 23
//
// — which is what beta did every five seconds. Every ClaimNextRunnable failed, so NO durable agent
// task ran, on a service whose health check was green and whose schema was fully migrated. Nothing
// caught it: not the build, not unit tests that never touch a database, and not the schema-drift
// check, which compares migration numbers and is satisfied the moment the columns exist.
//
// The pattern it protects is the one this codebase uses on purpose — one exported/shared column
// const so the projection and the scan cannot drift — which only works if something checks that
// they haven't.
//
// COUNTING IS ALL THIS ASSERTS, and the naive version of it was wrong. Splitting the column list
// on every comma reported Marketplace.ListColumns as 11 against a 10-destination scanTemplate,
// because COALESCE(u.username, ”) is ONE column containing a comma. Had that shipped it would
// have failed on correct code and been "fixed" by editing correct code, so the split below tracks
// parenthesis depth. Verified: zero findings across all 14 files that use the pattern, with the
// one real mismatch reintroduced and detected.
//
// The rule is deliberately loose: a column list must match SOME Scan in its own file, not a
// specific one, because the naming does not pair reliably (mcpColumns is read by scanServer). That
// admits a theoretical miss if an unrelated Scan happens to have the new count; it does not admit
// false alarms, which is the trade that keeps a check trusted.
func TestSharedColumnListsMatchTheirScanDestinations(t *testing.T) {
	root := "../models"
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("scan root %s missing — this check would silently pass: %v", root, err)
	}

	var offenders []string
	filesChecked := 0

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil
		}

		columnLists := map[string]int{}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || len(vs.Values) == 0 {
					continue
				}
				if !strings.HasSuffix(vs.Names[0].Name, "Columns") {
					continue
				}
				lit, ok := vs.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if n := countTopLevelColumns(strings.Trim(lit.Value, "`\"")); n > 0 {
					columnLists[vs.Names[0].Name] = n
				}
			}
		}
		if len(columnLists) == 0 {
			return nil
		}
		filesChecked++

		// Every Scan destination count in the file, by enclosing function.
		scanCounts := map[string]int{}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Scan" {
					return true
				}
				if len(call.Args) > scanCounts[fn.Name.Name] {
					scanCounts[fn.Name.Name] = len(call.Args)
				}
				return true
			})
		}

		for name, want := range columnLists {
			matched := false
			for _, got := range scanCounts {
				if got == want {
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			seen := make([]string, 0, len(scanCounts))
			for fn, got := range scanCounts {
				seen = append(seen, fmt.Sprintf("%s=%d", fn, got))
			}
			sort.Strings(seen)
			offenders = append(offenders, fmt.Sprintf(
				"%s: %s selects %d columns but no Scan in the file takes %d destinations (found %s)",
				path, name, want, want, strings.Join(seen, " ")))
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}
	if filesChecked == 0 {
		t.Fatal("no files with a shared column list were found — this check stopped enforcing anything")
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("column list / Scan destination mismatches (%d):\n  %s\n\n"+
			"database/sql only reports this at runtime. The symptom is that every read of that "+
			"table fails while the process looks healthy. Add the missing destination(s) in the "+
			"SAME ORDER as the column list.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// selectProjection captures the column list between SELECT and FROM.
var selectProjection = regexp.MustCompile(`(?is)^\s*SELECT\s+(.*?)\s+FROM\s`)

// The same arity rule for queries written INLINE rather than through a shared column const.
//
// The check above only sees the 14 models that share a *Columns const. Most queries do not: they
// carry their own SELECT next to their own Scan, and are equally able to drift — the failure is
// identical and equally invisible until a row is read.
//
// SCOPE, stated because it is narrow on purpose. This only examines functions containing EXACTLY
// one SELECT literal and EXACTLY one Scan call, and skips projections containing *. Anything
// looser cannot be paired reliably without resolving the query the Scan belongs to, and a check
// that guesses at pairings produces false alarms, which is how a check stops being trusted. 84
// functions qualify today and all 84 agree; the ones it cannot see are not made worse by its
// existence.
func TestInlineSelectProjectionsMatchTheirScans(t *testing.T) {
	roots := []string{"../models", "../domain", "../business", "../controllers"}

	var offenders []string
	checked := 0

	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("scan root %s missing — this check would silently pass: %v", root, err)
		}
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return nil
			}

			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name == nil || fn.Body == nil {
					continue
				}
				var projections []string
				var scanArities []int
				ast.Inspect(fn, func(n ast.Node) bool {
					if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if m := selectProjection.FindStringSubmatch(strings.Trim(lit.Value, "`\"")); m != nil {
							projections = append(projections, m[1])
						}
					}
					if call, ok := n.(*ast.CallExpr); ok {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Scan" {
							scanArities = append(scanArities, len(call.Args))
						}
					}
					return true
				})
				if len(projections) != 1 || len(scanArities) != 1 {
					continue // unpairable; see the scope note above
				}
				projection := strings.TrimSpace(projections[0])
				if strings.Contains(projection, "*") {
					continue // SELECT * has no countable projection
				}

				checked++
				want, got := countTopLevelColumns(projection), scanArities[0]
				if want != got {
					offenders = append(offenders, fmt.Sprintf(
						"%s: %s selects %d columns but Scan takes %d — %s",
						path, fn.Name.Name, want, got, oneLine(projection)))
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", root, walkErr)
		}
	}

	if checked == 0 {
		t.Fatal("no inline SELECT/Scan pairs found — this check stopped enforcing anything")
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("inline query / Scan arity mismatches (%d of %d pairs checked):\n  %s",
			len(offenders), checked, strings.Join(offenders, "\n  "))
	}
}

// oneLine collapses a projection to a single readable line for an error message.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 100 {
		return s[:100] + "..."
	}
	return s
}

// countTopLevelColumns counts comma-separated columns, ignoring commas nested in parentheses.
//
// The nesting matters: COALESCE(u.username, ”) is one column, and counting its comma turns a
// correct file into a reported defect.
func countTopLevelColumns(raw string) int {
	depth, n := 0, 0
	var cur strings.Builder
	flush := func() {
		if strings.TrimSpace(cur.String()) != "" {
			n++
		}
		cur.Reset()
	}
	for _, r := range raw {
		switch r {
		case '(':
			depth++
			cur.WriteRune(r)
		case ')':
			depth--
			cur.WriteRune(r)
		case ',':
			if depth == 0 {
				flush()
				continue
			}
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return n
}
