package helpers

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The integration harness (tests/integration) imports no application package,
// and no integration test can stop compiling without make verify failing.
//
// WHY THIS EXISTS. On 17 Oct 2026 the shared harness began calling four
// packages' Forget…ForTest itself. To do that it imported business/Calendar,
// business/Settings, business/SlackBridge and business/User, and through them
// about 130 more of the application's packages. Eight of those packages have
// integration tests inside the package (package x, not x_test) that import
// the harness: business/Channel, business/Doc, business/SlackImport,
// business/User, domain/Doc, domain/LastSeenChat, domain/Liveness and
// services/AI. That is an import cycle, which Go refuses, so thirteen tests
// stopped compiling. Nothing showed it for a day:
//   - the cycle exists only in the integration build;
//   - the unit gate and make verify built without that tag;
//   - the integration runners build ./tests/integration alone, so a -run
//     naming one of the thirteen matched nothing there and reported ok.
//
// Now each such package registers its Forget…ForTest with
// tests/integration/processcache from an integration-only init, and the harness
// forgets whatever registered. make verify vets the module's trees with the
// tag, so a cycle fails it. These checks explain why, and they catch the import
// before any test exists to cycle with it.

// harnessDir is the integration harness, seen from this package.
const harnessDir = "../tests/integration"

// aboveModels are the module's trees the harness may not import: business,
// domain, and everything that imports them. helpers, initializers and models
// are what it may use.
var aboveModels = []string{"adapter", "business", "cmd", "controllers", "domain", "middleware", "router", "services"}

// integrationBuild selects files as `go test -tags integration` does.
func integrationBuild() build.Context {
	ctxt := build.Default
	ctxt.BuildTags = append(slices.Clone(ctxt.BuildTags), "integration")
	return ctxt
}

// TestTheIntegrationHarnessImportsNoApplicationPackage: the harness's own
// files, and its sub-packages', import nothing from business, domain or the
// trees above them. A package keeping process-wide state that each test must
// start without registers it with tests/integration/processcache. The harness
// never imports the package to reset the state itself.
func TestTheIntegrationHarnessImportsNoApplicationPackage(t *testing.T) {
	module := goModulePath(t, "../go.mod")
	ctxt := integrationBuild()
	var dirs int
	var found []string
	err := filepath.WalkDir(harnessDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if name := d.Name(); path != harnessDir && (strings.HasPrefix(name, ".") || name == "testdata") {
			return fs.SkipDir
		}
		p, err := ctxt.ImportDir(path, 0)
		var noGo *build.NoGoError
		if errors.As(err, &noGo) {
			return nil
		}
		if err != nil {
			return err
		}
		dirs++
		for _, imp := range p.Imports {
			rel, ok := strings.CutPrefix(imp, module+"/")
			if !ok {
				continue
			}
			if top, _, _ := strings.Cut(rel, "/"); slices.Contains(aboveModels, top) {
				for _, f := range p.GoFiles {
					if fileImports(t, filepath.Join(path, f), imp) {
						found = append(found, fmt.Sprintf("%s imports %s", filepath.ToSlash(strings.TrimPrefix(filepath.Join(path, f), "../")), rel))
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if dirs == 0 {
		t.Fatalf("no package under %s: this check stopped looking", harnessDir)
	}
	sort.Strings(found)
	for _, f := range found {
		t.Errorf("%s. The harness may use helpers, initializers and models, never business, domain or what imports them. "+
			"Packages with an integration test of their own import the harness, so the harness importing them back is an import cycle, "+
			"and it shows only in the integration build. To reset a package's process-wide state before each test, register it with "+
			"tests/integration/processcache from an integration-tagged init() in that package.", f)
	}
}

// TestNoPackageTestedThroughTheHarnessIsOneTheHarnessLinks: a package whose own
// test files (package x, not x_test) import the harness can't be one the
// harness links, directly or through anything, or those tests can't compile.
// The harness imports no application package (above), but it still reaches a
// few through the initializers it points at its servers. initializers/mqttInit
// imports domain/User, and initializers/redis imports business/UserFCMToken.
// So none of those packages may test through the harness from inside. This
// reads every package the integration build has.
func TestNoPackageTestedThroughTheHarnessIsOneTheHarnessLinks(t *testing.T) {
	module := goModulePath(t, "../go.mod")
	harness := module + "/tests/integration"
	pkgs := integrationPackages(t, module)
	if _, ok := pkgs[harness]; !ok {
		t.Fatalf("the integration build has no %s: this check stopped looking", harness)
	}
	// What the harness links: every module package its non-test files reach,
	// with the import that first reached each.
	from := map[string]string{harness: ""}
	queue := []string{harness}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, imp := range pkgs[p].imports {
			if _, seen := from[imp]; !seen && strings.HasPrefix(imp, module+"/") {
				from[imp] = p
				queue = append(queue, imp)
			}
		}
	}
	var tested []string
	for path, p := range pkgs {
		if p.testsThroughHarness != "" {
			tested = append(tested, path)
		}
	}
	if len(tested) == 0 {
		t.Fatal("no package's own tests import the harness: this check stopped looking")
	}
	sort.Strings(tested)
	short := func(p string) string { return strings.TrimPrefix(p, module+"/") }
	for _, path := range tested {
		if _, linked := from[path]; !linked {
			continue
		}
		chain := []string{short(path)}
		for p := from[path]; p != ""; p = from[p] {
			chain = append([]string{short(p)}, chain...)
		}
		t.Errorf("%s imports the harness from inside its package, and the harness links it (%s): an import cycle, so its integration tests "+
			"can't compile. Make the test an external one (package %s_test) if it needs nothing unexported, or keep the harness from reaching %s.",
			pkgs[path].testsThroughHarness, strings.Join(chain, " → "), pkgs[path].name, short(path))
	}
	t.Logf("%d packages test through the harness from inside; the harness links %d of the module's packages", len(tested), len(from))
}

// TestEveryProcessCacheIsForgottenAsAnIntegrationTestStarts: every package
// function named Forget…ForTest, which drops what a process keeps of its
// databases, is registered with tests/integration/processcache by an init()
// in its own package. The harness calls processcache.ForgetAll as each test
// starts, so a new cache can't be missed, and the harness needn't know who
// keeps one.
func TestEveryProcessCacheIsForgottenAsAnIntegrationTestStarts(t *testing.T) {
	module := goModulePath(t, "../go.mod")
	registry := module + "/tests/integration/processcache"
	forgetter := regexp.MustCompile(`^Forget[A-Z]\w*ForTest$`)
	declared := map[string][]string{}          // dir -> its Forget…ForTest functions
	registered := map[string]map[string]bool{} // dir -> functions an init() registers
	harnessForgets := false
	roots := []string{"../adapter", "../business", "../controllers", "../domain", "../helpers", "../initializers",
		"../middleware", "../models", "../router", "../services", harnessDir}
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("%s is missing: this check would pass without looking (%v)", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Contains(src, []byte("ForTest")) && !bytes.Contains(src, []byte("processcache")) {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
			if err != nil {
				return err
			}
			dir := filepath.Dir(path)
			alias := ""
			for _, im := range f.Imports {
				if p, _ := strconv.Unquote(im.Path.Value); p == registry {
					alias = "processcache"
					if im.Name != nil {
						alias = im.Name.Name
					}
				}
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv != nil || fd.Body == nil {
					continue
				}
				if forgetter.MatchString(fd.Name.Name) {
					declared[dir] = append(declared[dir], fd.Name.Name)
				}
				if alias == "" {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if x, ok := sel.X.(*ast.Ident); !ok || x.Name != alias {
						return true
					}
					switch {
					case sel.Sel.Name == "Register" && fd.Name.Name == "init" && len(call.Args) == 2:
						if fn, ok := call.Args[1].(*ast.Ident); ok {
							if registered[dir] == nil {
								registered[dir] = map[string]bool{}
							}
							registered[dir][fn.Name] = true
						}
					case sel.Sel.Name == "ForgetAll" && dir == filepath.Clean(harnessDir):
						harnessForgets = true
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(declared) == 0 {
		t.Fatal("no Forget…ForTest anywhere: this check stopped looking")
	}
	var missing []string
	for dir, names := range declared {
		for _, n := range names {
			if !registered[dir][n] {
				missing = append(missing, filepath.ToSlash(strings.TrimPrefix(dir, "../"))+"."+n)
			}
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("%s drops state the process keeps, and no init() in its package registers it with processcache, so integration "+
			"tests don't start without that state. Add a file behind //go:build integration with "+
			"func init() { processcache.Register(\"<package>: <what it keeps>\", %s) }.", m, m[strings.LastIndex(m, ".")+1:])
	}
	if !harnessForgets {
		t.Error("no file of the harness calls processcache.ForgetAll, so what the packages registered is never forgotten (teststate.go did, as each test started)")
	}
}

// TestMakeVerifyVetsEveryIntegrationTaggedFile: make verify's
// `go vet -tags integration` names a tree for every file behind the
// integration tag, so an integration test that can't compile fails verify.
// Nothing else on the way to a release compiles those files.
func TestMakeVerifyVetsEveryIntegrationTaggedFile(t *testing.T) {
	mk, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	recipe := regexp.MustCompile(`(?ms)^verify:[^\n]*\n((?:\t[^\n]*\n|\n)*)`).FindSubmatch(mk)
	if recipe == nil {
		t.Fatal("the Makefile has no verify target")
	}
	vet := regexp.MustCompile(`(?m)^\tgo vet -tags integration((?:[^\n\\]|\\\n)*)$`).FindSubmatch(recipe[1])
	if vet == nil {
		t.Fatal("make verify no longer vets with the integration tag: an integration test that stops compiling would pass it again")
	}
	trees := map[string]bool{}
	for _, arg := range strings.Fields(strings.ReplaceAll(string(vet[1]), "\\\n", " ")) {
		switch {
		case arg == ".":
			trees["."] = true
		case strings.HasPrefix(arg, "./") && strings.HasSuffix(arg, "/..."):
			trees[strings.TrimSuffix(strings.TrimPrefix(arg, "./"), "/...")] = true
		}
	}
	module := goModulePath(t, "../go.mod")
	var tagged, outside []string
	for _, p := range integrationPackages(t, module) {
		if len(p.integrationOnly) == 0 {
			continue
		}
		tagged = append(tagged, p.dir)
		covered := p.dir == "." && trees["."]
		for d := p.dir; !covered && d != "." && d != "/"; d = filepath.Dir(d) {
			covered = trees[filepath.ToSlash(d)]
		}
		if !covered {
			outside = append(outside, fmt.Sprintf("%s (%s)", p.dir, strings.Join(p.integrationOnly, ", ")))
		}
	}
	if len(tagged) == 0 {
		t.Fatal("no file behind the integration tag anywhere: this check stopped looking")
	}
	sort.Strings(outside)
	for _, o := range outside {
		t.Errorf("%s: behind the integration tag, in a tree make verify's go vet -tags integration doesn't name. Add the tree there.", o)
	}
}

// modulePackage is what the integration build has in one directory.
type modulePackage struct {
	dir                 string   // relative to the module root, "." for the root
	name                string   // the package clause
	imports             []string // what its non-test files import
	testsThroughHarness string   // its own test file that imports the harness, if any
	integrationOnly     []string // files only the integration build has
}

// integrationPackages reads every directory of the module as
// `go test -tags integration` would build it, and as the default build would.
// It skips the root's data/ (the containers' own), dot directories,
// node_modules, vendor, testdata, and any directory with a go.mod of its own
// (another module).
func integrationPackages(t *testing.T, module string) map[string]*modulePackage {
	t.Helper()
	harness := module + "/tests/integration"
	withTag, without := integrationBuild(), build.Default
	out := map[string]*modulePackage{}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != ".." {
				return fs.SkipDir // unreadable: the containers' data
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != ".." {
			if name := d.Name(); path == filepath.Join("..", "data") || strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "testdata" {
				return fs.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return fs.SkipDir // another module (other-services/code-runner)
			}
		}
		p, err := withTag.ImportDir(path, 0)
		var noGo *build.NoGoError
		if errors.As(err, &noGo) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		rel, _ := filepath.Rel("..", path)
		rel = filepath.ToSlash(rel)
		importPath := module
		if rel != "." {
			importPath = module + "/" + rel
		}
		mp := &modulePackage{dir: rel, name: p.Name, imports: p.Imports}
		if importPath != harness && slices.Contains(p.TestImports, harness) {
			for _, f := range p.TestGoFiles {
				if fileImports(t, filepath.Join(path, f), harness) {
					mp.testsThroughHarness = filepath.ToSlash(filepath.Join(rel, f))
					break
				}
			}
		}
		plain := map[string]bool{}
		if q, err := without.ImportDir(path, 0); err == nil {
			for _, f := range slices.Concat(q.GoFiles, q.TestGoFiles, q.XTestGoFiles) {
				plain[f] = true
			}
		}
		for _, f := range slices.Concat(p.GoFiles, p.TestGoFiles, p.XTestGoFiles) {
			if !plain[f] {
				mp.integrationOnly = append(mp.integrationOnly, f)
			}
		}
		out[importPath] = mp
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fileImports reports whether the Go file imports path.
func fileImports(t *testing.T, file, path string) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range f.Imports {
		if p, _ := strconv.Unquote(im.Path.Value); p == path {
			return true
		}
	}
	return false
}

// neverNilClients are the server's clients that code dereferences from
// goroutines nobody waits for (go mqttBusiness.Publish…, go
// domain.Update…InOpenSearch, graph reads), mostly without a nil check.
// The integration harness keeps each one non-nil from the binary's start, and
// between tests it leaves one that reaches nothing.
// redisInit.RedisClient isn't here: nil is production's "no Redis", and
// every use of it checks for nil first.
var neverNilClients = map[string]string{
	"initializers/mqttInit":       "MqttClient",
	"initializers/opensearchInit": "OpenSearchClient",
	"initializers/dgraphInit":     "DgraphClient",
}

// TestNoIntegrationCleanupSetsAClientBackToNil: no file behind the integration
// tag sets the server's MQTT, search or graph client to nil. On 18 Oct 2026
// a test's cleanup did (mqttInit.MqttClient = nil), and a
// `go mqttBusiness.PublishTaskComment(...)` the test had started ran after it.
// It dereferenced nil, and the panic ended the binary with twelve tests unrun.
// Use integration.UseMqttClient or integration.UseOpenSearchClient, which put
// back a client that reaches nothing, or save the old client and put it back.
func TestNoIntegrationCleanupSetsAClientBackToNil(t *testing.T) {
	module := goModulePath(t, "../go.mod")
	var files, sets int
	var found []string
	for _, p := range integrationPackages(t, module) {
		for _, name := range p.integrationOnly {
			path := filepath.Join("..", p.dir, name)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files++
			alias := map[string]string{} // the name an import is used by -> the client it holds
			for _, im := range f.Imports {
				ip, _ := strconv.Unquote(im.Path.Value)
				v, ok := neverNilClients[strings.TrimPrefix(ip, module+"/")]
				if !ok {
					continue
				}
				n := ip[strings.LastIndex(ip, "/")+1:]
				if im.Name != nil {
					n = im.Name.Name
				}
				alias[n] = v
			}
			if len(alias) == 0 {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != len(as.Rhs) {
					return true
				}
				for i, l := range as.Lhs {
					sel, ok := l.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					x, ok := sel.X.(*ast.Ident)
					if !ok || alias[x.Name] != sel.Sel.Name {
						continue
					}
					sets++
					if r, ok := as.Rhs[i].(*ast.Ident); ok && r.Name == "nil" {
						found = append(found, fmt.Sprintf("%s: %s.%s = nil", strings.TrimPrefix(fset.Position(as.Pos()).String(), "../"), x.Name, sel.Sel.Name))
					}
				}
				return true
			})
		}
	}
	if files == 0 || sets == 0 {
		t.Fatalf("read %d integration files and %d assignments to the clients: this check stopped looking", files, sets)
	}
	sort.Strings(found)
	for _, f := range found {
		t.Errorf("%s: work a test started and didn't wait for can run after this and dereference nil, which ends the "+
			"binary with every later test unrun. Put back a client that reaches nothing (integration.UseMqttClient, "+
			"integration.UseOpenSearchClient), or the client the test found.", f)
	}
}
