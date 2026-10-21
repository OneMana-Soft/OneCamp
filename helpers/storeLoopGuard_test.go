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
	"strings"
	"testing"
)

// storeLoopBaseline is every store call inside a loop that was there when the
// guard landed (the backend audit's loop inventory, 18 Oct 2026), one per line
// as "file :: function :: call". A package that batches one deletes its line.
const storeLoopBaseline = "testdata/store-loop-baseline.txt"

// storeLoopHeader opens the baseline file.
const storeLoopHeader = "# Store calls inside loops in business/, controllers/ and domain/, as they were when\n" +
	"# helpers/storeLoopGuard_test.go landed (backend audit, 18 Oct 2026): file :: function :: call.\n" +
	"# Batch a loop, then delete its line. A new line needs a reason in its commit.\n"

const modulePath = "github.com/akashc777/OneCamp"

// storeMethods are calls that reach a store whatever they are called on: SQL,
// Dgraph, Redis pipelines, MQTT.
var storeMethods = map[string]bool{
	"QueryContext": true, "ExecContext": true, "QueryRowContext": true, "PrepareContext": true, "BeginTx": true,
	"QueryWithVars": true, "Mutate": true, "Do": true, "DoCommitNow": true, "ReadInto": true, "ReadIntoTxn": true,
	"InTxn": true, "InSnapshot": true, "InTx": true, "Pipelined": true, "TxPipelined": true, "Publish": true,
}

// storePackage reports whether a call into the package at path reaches a store
// by itself: the models, the initializers (the clients) and MQTT publishing.
func storePackage(path string) bool {
	return strings.HasPrefix(path, modulePath+"/models/") || strings.HasPrefix(path, modulePath+"/initializers/") ||
		path == modulePath+"/business/Mqtt" || strings.Contains(path, "go-redis") || strings.Contains(path, "opensearch-go")
}

type loopFile struct {
	path, pkg string
	imports   map[string]string
	ast       *ast.File
}

// storeLoopScan finds, in business/, controllers/ and domain/, every call made
// inside a for or range loop that reaches a store: a store method, a call into
// a store package, or a call to a function of the module that does either (up
// to six calls deep). Keyed by file, the function holding the loop and the
// call, so moving lines doesn't change a key.
func storeLoopScan(t *testing.T) (map[string]bool, int) {
	t.Helper()
	fset := token.NewFileSet()
	var files []*loopFile
	type fnInfo struct {
		file  *loopFile
		body  *ast.BlockStmt
		calls []string
		store bool
	}
	fns := map[string]*fnInfo{}
	type alias struct {
		key  string
		file *loopFile
		to   ast.Expr
	}
	var aliases []alias
	for _, root := range []string{"business", "controllers", "domain", "models", "initializers", "services"} {
		err := filepath.WalkDir("../"+root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(strings.TrimPrefix(path, "../"))
			lf := &loopFile{path: rel, pkg: modulePath + "/" + filepath.ToSlash(filepath.Dir(rel)), imports: map[string]string{}, ast: f}
			for _, im := range f.Imports {
				p := strings.Trim(im.Path.Value, `"`)
				name := filepath.Base(p)
				if im.Name != nil {
					name = im.Name.Name
				} else if strings.Contains(p, "go-redis") {
					name = "redis"
				} else if strings.HasPrefix(p, "github.com/dgraph-io/dgo") {
					name = "dgo"
				}
				lf.imports[name] = p
			}
			files = append(files, lf)
			for _, decl := range f.Decls {
				// A package variable holding a function (a seam tests replace:
				// var readDocFor = func(...) {...}, editWho = otherPkg.Func) is
				// called like one.
				if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
					for _, spec := range gd.Specs {
						vs := spec.(*ast.ValueSpec)
						for i, name := range vs.Names {
							if i >= len(vs.Values) {
								break
							}
							key := lf.pkg + "." + name.Name
							switch v := vs.Values[i].(type) {
							case *ast.FuncLit:
								fns[key] = &fnInfo{file: lf, body: v.Body}
							case *ast.SelectorExpr, *ast.Ident:
								aliases = append(aliases, alias{key, lf, v})
							}
						}
					}
					continue
				}
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				key := lf.pkg + "." + fd.Name.Name
				if fd.Recv != nil {
					key = lf.pkg + "#" + fd.Name.Name
				}
				fns[key] = &fnInfo{file: lf, body: fd.Body}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// resolve names a call: the function it reaches in the module (by key),
	// and whether the call reaches a store by itself.
	resolve := func(lf *loopFile, c *ast.CallExpr) (key, name string, store bool) {
		switch fn := c.Fun.(type) {
		case *ast.Ident:
			return lf.pkg + "." + fn.Name, fn.Name, false
		case *ast.SelectorExpr:
			if id, ok := fn.X.(*ast.Ident); ok {
				if p, ok := lf.imports[id.Name]; ok {
					return p + "." + fn.Sel.Name, id.Name + "." + fn.Sel.Name, storePackage(p)
				}
			}
			if storeMethods[fn.Sel.Name] {
				return "", "." + fn.Sel.Name, true
			}
			// A method of another package's variable: pkg.Var.Method(...).
			if inner, ok := fn.X.(*ast.SelectorExpr); ok {
				if id, ok := inner.X.(*ast.Ident); ok {
					if p, ok := lf.imports[id.Name]; ok {
						return p + "#" + fn.Sel.Name, id.Name + "." + inner.Sel.Name + "." + fn.Sel.Name, storePackage(p)
					}
				}
			}
			return lf.pkg + "#" + fn.Sel.Name, "." + fn.Sel.Name, false
		}
		return "", "", false
	}
	for _, a := range aliases {
		if _, ok := fns[a.key]; ok {
			continue
		}
		key, _, store := resolve(a.file, &ast.CallExpr{Fun: a.to})
		f := &fnInfo{file: a.file, body: &ast.BlockStmt{}, store: store}
		if key != "" {
			f.calls = []string{key}
		}
		fns[a.key] = f
	}
	for _, f := range fns {
		ast.Inspect(f.body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			key, _, store := resolve(f.file, c)
			if store {
				f.store = true
			} else if key != "" {
				f.calls = append(f.calls, key)
			}
			return true
		})
	}
	reaches := map[string]bool{}
	for k, f := range fns {
		if f.store {
			reaches[k] = true
		}
	}
	for depth := 0; depth < 6; depth++ {
		for k, f := range fns {
			if reaches[k] {
				continue
			}
			for _, c := range f.calls {
				if reaches[c] {
					reaches[k] = true
					break
				}
			}
		}
	}

	found := map[string]bool{}
	scanned := 0
	for _, lf := range files {
		if !strings.HasPrefix(lf.path, "business/") && !strings.HasPrefix(lf.path, "controllers/") && !strings.HasPrefix(lf.path, "domain/") {
			continue
		}
		scanned++
		for _, decl := range lf.ast.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				var body *ast.BlockStmt
				switch l := n.(type) {
				case *ast.ForStmt:
					body = l.Body
				case *ast.RangeStmt:
					body = l.Body
				default:
					return true
				}
				ast.Inspect(body, func(m ast.Node) bool {
					c, ok := m.(*ast.CallExpr)
					if !ok {
						return true
					}
					key, name, store := resolve(lf, c)
					if store || reaches[key] {
						found[fmt.Sprintf("%s :: %s :: %s", lf.path, fd.Name.Name, name)] = true
					}
					return true
				})
				return false // the inner walk covered nested loops
			})
		}
	}
	return found, scanned
}

// No new store call inside a loop in business/, controllers/ or domain/.
//
// WHY. The backend audit (18 Oct 2026) found forty-odd request paths that
// make one store round trip per item: a Redis SETNX per channel member on
// every post, a SQL read per project on every project list, up to 400 graph
// reads per AskAI question, a Dgraph read per recipient of a forwarded
// message. Each was written one call at a time and each looked harmless at the
// size it was written for. This reads every loop and every call in it that
// reaches a store (directly, or through a function of the module that does,
// six calls deep), and compares them with the baseline: a new one fails, so a
// loop is batched (a read by ids, = ANY, unnest, a pipeline, _bulk) before it
// lands; an entry nothing matches any more fails too, so the line goes when
// its package batches the loop.
//
// When the call really must run per item (an import's vendor pages, a send
// that is per recipient by nature), add its line to the baseline with the
// change that needs it, and say why in the commit.
func TestNoNewStoreCallsInLoops(t *testing.T) {
	found, scanned := storeLoopScan(t)
	if scanned < 1000 {
		t.Fatalf("scanned %d files; the guard is no longer looking at business, controllers and domain", scanned)
	}
	if os.Getenv("WRITE_STORE_LOOP_BASELINE") != "" {
		keys := make([]string, 0, len(found))
		for k := range found {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if err := os.WriteFile(storeLoopBaseline, []byte(storeLoopHeader+strings.Join(keys, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d entries", len(keys))
		return
	}
	f, err := os.Open(storeLoopBaseline)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	baseline := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			baseline[line] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	var added, gone []string
	for k := range found {
		if !baseline[k] {
			added = append(added, k)
		}
	}
	for k := range baseline {
		if !found[k] {
			gone = append(gone, k)
		}
	}
	sort.Strings(added)
	sort.Strings(gone)
	for _, k := range added {
		t.Errorf("a store call in a loop: %s. Batch it (read by ids, = ANY, unnest, a pipeline, _bulk), or add the line to %s with the reason in the commit", k, storeLoopBaseline)
	}
	for _, k := range gone {
		t.Errorf("%s is in %s but no loop makes that call now; delete the line", k, storeLoopBaseline)
	}
}
