package helpers

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
// WHAT COUNTS AS A REFERENCE. A mention of the function, in a non-test .go file anywhere in the
// module, other than the declaration itself: pkg.Name where pkg is an import of its package, or
// a bare Name in a file of its own package. Matching a bare name anywhere, as this did until
// October 2026, let a name used elsewhere hide a dead function: ten that no binary reached
// passed, among them three called Get, two ExecQueryRow, a business function whose only
// "reference" was its own call to the domain function of the same name, and that domain
// function. Selections (x.Name), declared names, struct fields and composite-literal keys are
// not references; anything else in the package that spells the name still counts, so the check
// still under-reports rather than invents work.
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
		"../router",
		"../initializers",
	}
	// Where references are looked for: everything, so a caller outside the declaration roots
	// still counts.
	refRoots := []string{
		"../business", "../controllers", "../domain", "../helpers", "../middleware",
		"../models", "../services", "../adapter", "../router", "../initializers",
		"../cmd", "../sdk", "../other-services",
	}
	module := goModulePath(t, "../go.mod")

	// A package is its directory, relative to the module root ("business/Doc"): what an import
	// path names after the module, and what a file's own references name.
	type fn struct{ dir, name string }
	type parsed struct {
		dir    string
		isTest bool
		file   *ast.File
		fset   *token.FileSet
	}
	var files []parsed
	for _, root := range refRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
			dir := strings.TrimPrefix(filepath.ToSlash(filepath.Dir(path)), "../")
			files = append(files, parsed{dir: dir, isTest: strings.HasSuffix(path, "_test.go"), file: file, fset: fset})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	// The name each package declares, which an import without an alias is used by.
	pkgName := map[string]string{}
	for _, f := range files {
		if !f.isTest {
			pkgName[f.dir] = f.file.Name.Name
		}
	}

	type decl struct {
		file string
		line int
	}
	declared := map[fn]decl{}          // exported func -> where
	declIdent := map[*ast.Ident]bool{} // the declarations' own names, which reference nothing
	for _, root := range declRoots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("declaration root %s missing — this check would silently pass: %v", root, err)
		}
	}
	inDeclRoots := func(dir string) bool {
		for _, root := range declRoots {
			r := strings.TrimPrefix(root, "../")
			if dir == r || strings.HasPrefix(dir, r+"/") {
				return true
			}
		}
		return false
	}
	for _, f := range files {
		if f.isTest || !inDeclRoots(f.dir) {
			continue
		}
		for _, d := range f.file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			// Plain exported functions only. Methods are excluded because they may satisfy an
			// interface and be called through it, which no reading of names can see.
			if !ok || fd.Recv != nil || fd.Name == nil || !fd.Name.IsExported() {
				continue
			}
			if fd.Name.Name == "Main" || strings.HasPrefix(fd.Name.Name, "Test") {
				continue
			}
			pos := f.fset.Position(fd.Name.Pos())
			declared[fn{f.dir, fd.Name.Name}] = decl{file: pos.Filename, line: pos.Line}
			declIdent[fd.Name] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("no exported functions found — this check stopped enforcing anything")
	}

	refsProd := map[fn]int{} // mentions in non-test files, excluding the declaration
	refsTest := map[fn]int{} // mentions in _test.go files
	for _, f := range files {
		alias := map[string]string{} // the name an import is used by -> its package's dir
		for _, im := range f.file.Imports {
			path, err := strconv.Unquote(im.Path.Value)
			if err != nil || !strings.HasPrefix(path, module+"/") {
				continue
			}
			dir := strings.TrimPrefix(path, module+"/")
			name := pkgName[dir]
			if im.Name != nil {
				name = im.Name.Name
			}
			if name != "" && name != "_" && name != "." {
				alias[name] = dir
			}
		}
		refs := refsProd
		if f.isTest {
			refs = refsTest
		}
		// Idents that name something other than a function of this file's package.
		notRef := map[*ast.Ident]bool{}
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				notRef[x.Sel] = true
				if id, ok := x.X.(*ast.Ident); ok {
					if dir, ok := alias[id.Name]; ok {
						refs[fn{dir, x.Sel.Name}]++
						notRef[id] = true
					}
				}
			case *ast.FuncDecl:
				notRef[x.Name] = true
			case *ast.Field:
				for _, id := range x.Names {
					notRef[id] = true
				}
			case *ast.TypeSpec:
				notRef[x.Name] = true
			case *ast.ValueSpec:
				for _, id := range x.Names {
					notRef[id] = true
				}
			case *ast.CompositeLit:
				for _, e := range x.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							notRef[id] = true
						}
					}
				}
			case *ast.Ident:
				if !notRef[x] && !declIdent[x] {
					refs[fn{f.dir, x.Name}]++
				}
			}
			return true
		})
	}

	dead := map[string]string{} // "dir.Name" -> where, for everything currently unreachable
	for k, where := range declared {
		if refsProd[k] > 0 || exemptFromDeadExportedCheck[k.name] != "" {
			continue
		}
		suffix := ""
		if refsTest[k] > 0 {
			suffix = fmt.Sprintf(" (only its own tests, %d refs)", refsTest[k])
		}
		dead[k.dir+"."+k.name] = fmt.Sprintf("%s:%d%s", where.file, where.line, suffix)
	}

	baseline, err := readDeadExportedBaseline()
	if err != nil {
		t.Fatalf("reading the baseline: %v", err)
	}

	// NEW deadness fails. This is the half with teeth. dead's keys carry their package
	// ("business/Poll.Summary"); a baseline entry is either such a key or, as this edition's
	// older entries are, a bare name that stands for every function of that name.
	var added []string
	deadNames := map[string]bool{}
	for key, where := range dead {
		name := key[strings.LastIndex(key, ".")+1:]
		deadNames[name] = true
		if !baseline[name] && !baseline[key] {
			added = append(added, fmt.Sprintf("%s: %s", where, name))
		}
	}
	// And the baseline may not rot: once something is called or deleted, its entry must go, or
	// the list slowly becomes a place where names are parked and nobody notices they came back
	// to life.
	var stale []string
	for entry := range baseline {
		if _, still := dead[entry]; still || deadNames[entry] {
			continue
		}
		stale = append(stale, entry)
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

// goModulePath is the module go.mod declares: what this module's import paths start with.
func goModulePath(t *testing.T, gomod string) string {
	t.Helper()
	f, err := os.Open(gomod)
	if err != nil {
		t.Fatalf("read %s: %v", gomod, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	t.Fatalf("%s declares no module", gomod)
	return ""
}

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

	// RELEASE-LINE ONLY (main and without-ai, from v2.73.0). Beta's demo seed
	// (business/DemoSeed there) calls these seven; the release lines carry the
	// old seeder instead, since the demo's own code never ships to customers.
	// They stay in their shared packages, identical to beta's, so later ports
	// apply as they are; deleting them here would fork those packages.
	"MarkProjectArchivedInOpenSearch": "called only by beta's demo seed (a refresh takes every " +
		"retired copy of a curated project out of search); the release lines don't carry it.",
	"ChannelIDsWithNamePrefix": "called only by beta's demo seed (the visitor leaves every " +
		"retired copy of the curated channels); the release lines don't carry it.",
	"ArchivedProjectsWithNamePrefix": "called only by beta's demo seed (retired copies of the " +
		"curated projects); the release lines don't carry it.",
	"AddAt": "called only by beta's demo seed (project updates backdated to the run); the " +
		"release lines don't carry it.",
	"ClearForPersonInProject": "called only by beta's demo seed (its showcase time entries); " +
		"the release lines don't carry it.",
	"UpdateDocPermissions": "business/Doc's is called only by beta's demo seed (the docs home's " +
		"shared and team docs); the share routes don't call it; the release lines don't carry the seed.",
	"CreateRow": "models/postgres/DataTable's is called only by beta's demo seed (the vendors and " +
		"formula tables' rows); every other row goes through business/DataTable's CreateRow, which " +
		"checks access; the release lines don't carry the seed.",

	"PointAtSlackForTest": "sends the Slack bridge's Web API calls to a local fake, so the " +
		"integration test can run the whole bridge (both directions, threads, edits, deletions, " +
		"echo suppression) against real stores without a Slack workspace.",
	"ForgetStateForTest": "drops the Slack bridge's cached configuration after the integration " +
		"test writes the bridge tables directly; the cache would otherwise hide them for 30 seconds.",
	"ObservePushesForTest": "shows the integration test every push a path asks for, so whom a " +
		"guest's message, reply or doc comment reaches is checked against real stores. The harness " +
		"has no Firebase project, and without one MultiCastPush sends nothing there is to look at.",

	"HoldRunsForTest": "keeps an import run's lease after its run returns, as if the orchestrator " +
		"were still cleaning up after writing its last status, so the integration test can prove a " +
		"click in that window starts nothing. The test environment's run fails at once, and without " +
		"the hold the window is a few microseconds that load alone sometimes opens.",

	"ForgetAutomationBotForTest": "drops the automation bot identity kept for the process, so an " +
		"integration test against databases of its own resolves the bot there; one resolved by an " +
		"earlier test posts as a user the later test's databases don't have.",

	"ForgetBookingPagesForTest": "drops the booking pages, answers and busy times the calendar " +
		"keeps for a minute by address, after an integration test starts fresh databases: the " +
		"booking test runs once per calendar store in one process, and the store before's " +
		"\"office-hours\" answered for the next store's deleted page. In the integration build " +
		"only (the tag keeps it out of the shipped binary).",

	"ForgetSettingsForTest": "drops the workspace settings cached for 30 seconds, after an " +
		"integration test starts a fresh database; the previous test's choices (the channels new " +
		"members join, say) would otherwise apply in the next one.",

	"UseDirectoryForTest": "stands in for the directory server, so the integration test can sign in " +
		"through LDAP against real stores and prove that someone with two-step on is asked for their " +
		"code before any session exists. The harness has no LDAP server, and everything under test " +
		"comes after the directory's answer.",

	"StartedForTest": "starts the server again, as far as the conversations index is concerned, " +
		"so the integration test of a restore can prove that a process trusts no comparison older " +
		"than itself: someone taken out of a private channel between a backup's Postgres dump and " +
		"its graph export is refused after the restore. The integration tests run in one process " +
		"started before every comparison they make, so nothing else can make a restored stamp " +
		"older than the process. It is built only with the integration tag.",

	"AgentBudgetLimit": "reads the per-agent cap off a context so business/MCPServer's tests can " +
		"assert SpendContext actually carries it. services/AI keeps budgetDimensions unexported, " +
		"so no other package can see the limit. The invariant is production behaviour — a metered " +
		"dimension with no cap would meter and never refuse — and the accessor is the only way to " +
		"check it from where the context is built.",

	"ActorID": "reads off a context who its AI spend is billed to, resolved as the meters resolve " +
		"it, so business/MCPServer's tests can assert SpendContext attributes an MCP call's spend " +
		"to the token's person. services/AI keeps actorFromContext unexported. Until October 2026 " +
		"a field named ActorID elsewhere hid it from this check.",
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
