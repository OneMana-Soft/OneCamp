package helpers

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// An exported function nobody calls is dead, and a test calling it does not make it alive.
//
// WHY THIS EXISTS. services/AI/deadHelper_test.go walks reachability for UNEXPORTED functions and
// documents its own blind spot: exported ones are treated as roots, so they keep everything
// beneath them alive and are never themselves questioned. That blind spot then caught me out.
// helpers.IsForeignKeyViolation and helpers.IsNotNullViolation were added next to
// IsUniqueViolation because a set of three looked more complete than one. Only IsUniqueViolation
// ever had a caller. Both of the others had a passing table-driven test, which is what made them
// look used — the test was the only thing that ever called them.
//
// That is the exact pattern the unexported check was written for in 83fca2d, so the rule already
// existed and the enforcement simply did not reach this far.
//
// WHAT COUNTS AS A REFERENCE. Any mention of the identifier in a non-test .go file anywhere in
// the module, other than the declaration itself. That is deliberately generous: identifiers are
// matched by name without resolving packages, so an unrelated function with the same name in
// another package will keep this quiet. The check therefore under-reports and never invents work.
//
// Exemptions are narrow and each is a real reason, not a way to silence a finding:
//   - cmd and other-services are entry points; nothing in-module calls main.
//   - sdk is a published surface whose callers are outside this repository by definition.
//   - controllers are referenced by router.go, so they need no exemption and get none.
func TestNoDeadExportedFunctions(t *testing.T) {
	// Where declarations are looked for. Required to exist, so a rename cannot quietly turn this
	// into a check that scans nothing.
	declRoots := []string{
		"../business",
		"../controllers",
		"../domain",
		"../helpers",
		"../middleware",
		"../models",
		"../services",
		"../adapter",
	}
	// Where references are looked for: everything, so a caller outside the declaration roots
	// still counts.
	refRoots := []string{
		"../business", "../controllers", "../domain", "../helpers", "../middleware",
		"../models", "../services", "../adapter", "../router", "../initializers",
		"../cmd", "../sdk", "../other-services",
	}

	type decl struct {
		file string
		line int
	}
	declared := map[string]decl{}          // exported func name -> where
	refsProd := map[string]int{}           // name -> mentions in non-test files, excl. declaration
	refsTest := map[string]int{}           // name -> mentions in _test.go files
	declPos := map[string]token.Position{} // to exclude the declaration's own identifier

	collect := func(root string, wantDecls bool) error {
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				// Vendored or generated trees would swamp the result.
				if name := d.Name(); name == "node_modules" || name == "vendor" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return nil // not our job to police unparseable generated files
			}
			isTest := strings.HasSuffix(path, "_test.go")

			if wantDecls && !isTest {
				for _, d := range file.Decls {
					fn, ok := d.(*ast.FuncDecl)
					// Plain exported functions only. Methods are excluded because they may
					// satisfy an interface and be called through it, which no name-based scan
					// can see.
					if !ok || fn.Recv != nil || fn.Name == nil || !fn.Name.IsExported() {
						continue
					}
					if fn.Name.Name == "Main" || strings.HasPrefix(fn.Name.Name, "Test") {
						continue
					}
					pos := fset.Position(fn.Name.Pos())
					declared[fn.Name.Name] = decl{file: path, line: pos.Line}
					declPos[fn.Name.Name] = pos
				}
			}

			ast.Inspect(file, func(n ast.Node) bool {
				ident, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				pos := fset.Position(ident.Pos())
				if isTest {
					refsTest[ident.Name]++
					return true
				}
				// Skip the declaration's own name token.
				if d, seen := declPos[ident.Name]; seen &&
					d.Filename == pos.Filename && d.Line == pos.Line && d.Column == pos.Column {
					return true
				}
				refsProd[ident.Name]++
				return true
			})
			return nil
		})
	}

	for _, root := range declRoots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("declaration root %s missing — this check would silently pass: %v", root, err)
		}
		if err := collect(root, true); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(declared) == 0 {
		t.Fatal("no exported functions found — this check stopped enforcing anything")
	}
	for _, root := range refRoots {
		if err := collect(root, false); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	dead := map[string]string{} // name -> where, for everything currently unreachable
	for name, where := range declared {
		if refsProd[name] > 0 || exemptFromDeadExportedCheck[name] != "" {
			continue
		}
		suffix := ""
		if refsTest[name] > 0 {
			suffix = fmt.Sprintf(" (only its own tests, %d refs)", refsTest[name])
		}
		dead[name] = fmt.Sprintf("%s:%d%s", where.file, where.line, suffix)
	}

	baseline, err := readDeadExportedBaseline()
	if err != nil {
		t.Fatalf("reading the baseline: %v", err)
	}

	// NEW deadness fails. This is the half with teeth.
	var added []string
	for name, where := range dead {
		if !baseline[name] {
			added = append(added, fmt.Sprintf("%s: %s", where, name))
		}
	}
	// And the baseline may not rot: once something is called or deleted, its entry must go, or
	// the list slowly becomes a place where names are parked and nobody notices they came back
	// to life.
	var stale []string
	for name := range baseline {
		if _, still := dead[name]; !still {
			stale = append(stale, name)
		}
	}
	sort.Strings(added)
	sort.Strings(stale)

	if len(added) > 0 {
		t.Errorf("new exported functions that nothing calls (%d) — call them, or delete them:\n  %s",
			len(added), strings.Join(added, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("%d baseline entries are no longer dead — remove them from %s:\n  %s",
			len(stale), deadExportedBaselinePath, strings.Join(stale, "\n  "))
	}
}

const deadExportedBaselinePath = "testdata/dead-exported-baseline.txt"

// exemptFromDeadExportedCheck lists exported functions that only tests call, WITH the reason.
//
// The bar is that the function makes a real production invariant observable from another package,
// and is not itself the thing under test. It is not a place to park something that has no caller
// yet.
var exemptFromDeadExportedCheck = map[string]string{
	"RegisterImportDigester": "an EDITION-CONDITIONAL registration slot, and the only kind of " +
		"exported function that is legitimately uncalled in one build. business/SlackImport ships " +
		"in both editions and declares the slot; business/AI fills it from an init and exists only " +
		"on the AI edition. So this has a real production caller on main and none here, which is " +
		"the property the inversion is FOR: linking the AI packages is what makes import digests " +
		"exist, and not linking them is what makes them absent, with no edit on either side. " +
		"Deleting it would delete the seam and force SlackImport to import AI, which the AI-free " +
		"build cannot do.",

	"RegisterDemoFixture": "the second EDITION-CONDITIONAL registration slot, and it broke the " +
		"AI-free build's test run the moment it shipped. business/DemoSeed is on both editions and " +
		"reads the registrations; business/AIDrill fills this one from an init and exists only on " +
		"the AI edition, so on the AI-free line the slot is declared, read, and never filled. That " +
		"is the inversion working: linking the drill is what gives the demo seeder a drill fixture, " +
		"and not linking it is what makes the seeder skip it, with no edit on either side. Deleting " +
		"it would force DemoSeed to import the drill, which the AI-free build cannot do.",

	"PointAtSlackForTest": "sends the Slack bridge's Web API calls to a local fake, so the " +
		"integration test can run the whole bridge (both directions, threads, edits, deletions, " +
		"echo suppression) against real stores without a Slack workspace.",
	"ForgetStateForTest": "drops the Slack bridge's cached configuration after the integration " +
		"test writes the bridge tables directly; the cache would otherwise hide them for 30 seconds.",
	"ObservePushesForTest": "shows the integration test every push a path asks for, so whom a " +
		"guest's message, reply or doc comment reaches is checked against real stores. The harness " +
		"has no Firebase project, and without one MultiCastPush sends nothing there is to look at.",

	"ForgetSettingsForTest": "drops the workspace settings cached for 30 seconds, after an " +
		"integration test starts a fresh database; the previous test's choices (the channels new " +
		"members join, say) would otherwise apply in the next one.",

	"UseDirectoryForTest": "stands in for the directory server, so the integration test can sign in " +
		"through LDAP against real stores and prove that someone with two-step on is asked for their " +
		"code before any session exists. The harness has no LDAP server, and everything under test " +
		"comes after the directory's answer.",
	"AgentBudgetLimit": "reads the per-agent cap off a context so business/MCPServer's tests can " +
		"assert SpendContext actually carries it. services/AI keeps budgetDimensions unexported, " +
		"so no other package can see the limit. The invariant is production behaviour — a metered " +
		"dimension with no cap would meter and never refuse — and the accessor is the only way to " +
		"check it from where the context is built.",
}

// readDeadExportedBaseline loads the recorded backlog: one function name per line, # for comments.
//
// A FILE rather than a literal in this test, because the point of the list is that it shrinks. In
// a data file each removal is a one-line diff a reviewer can see, and the list cannot quietly grow
// inside a code change.
func readDeadExportedBaseline() (map[string]bool, error) {
	raw, err := os.ReadFile(deadExportedBaselinePath)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = true
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("baseline %s parsed to nothing", deadExportedBaselinePath)
	}
	return out, nil
}
