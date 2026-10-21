package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rawGoBaseline is how many go statements business/, controllers/, domain/,
// initializers/ and services/ still start themselves rather than through
// GoSafeNamed or GoSafeLoop (427 before F4, 165 after). It only goes down.
// without-ai's own count, without the AI edition's packages (159 on main).
const rawGoBaseline = 147

// unrecoveredGoBaseline is how many of those have no recover at all: a
// panic on one ends the process (355 before F4). It only goes down.
// without-ai's own count (87 on main).
const unrecoveredGoBaseline = 83

// goStatements counts the go statements under the trees, and those with no
// recover in sight: a go func() {...}() whose body never calls recover (or a
// Recover* helper), or a go f(...) of any function.
func goStatements(t *testing.T) (raw, unrecovered int) {
	t.Helper()
	for _, root := range []string{"../business", "../controllers", "../domain", "../initializers", "../services"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				g, ok := n.(*ast.GoStmt)
				if !ok {
					return true
				}
				raw++
				recovered := false
				if lit, ok := g.Call.Fun.(*ast.FuncLit); ok {
					ast.Inspect(lit.Body, func(m ast.Node) bool {
						if c, ok := m.(*ast.CallExpr); ok {
							switch fn := c.Fun.(type) {
							case *ast.Ident:
								recovered = recovered || fn.Name == "recover"
							case *ast.SelectorExpr:
								recovered = recovered || strings.Contains(fn.Sel.Name, "ecover")
							}
						}
						return true
					})
				}
				if !recovered {
					unrecovered++
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return raw, unrecovered
}

// Goroutines are started through GoSafeNamed or GoSafeLoop, and the ones
// that aren't only get fewer.
//
// WHY. A panic on a goroutine nobody recovers ends the whole process, every
// user's requests with it, and recover in the caller cannot help: it only
// catches its own goroutine's. The audit found 355 such goroutines; one of
// them, a task's attachments going to search, crashed the integration test
// written for this package the moment OpenSearch wasn't there. GoSafeNamed
// recovers and logs a one-shot goroutine and counts it as work in flight;
// GoSafeLoop does the same for one that lasts as long as its context. This
// counts the go statements still written by hand, and those with no recover
// at all, and fails when either rises above its baseline, or falls below it
// (lower the baseline with the change).
func TestGoroutinesStartThroughGoSafe(t *testing.T) {
	raw, unrecovered := goStatements(t)
	if raw == 0 {
		t.Fatal("found no go statements; the guard is no longer looking at the code")
	}
	switch {
	case raw > rawGoBaseline:
		t.Errorf("%d go statements are written by hand, more than %d: start the new one with helpers.GoSafeNamed (or GoSafeLoop for one that lasts as long as its context)", raw, rawGoBaseline)
	case raw < rawGoBaseline:
		t.Errorf("%d go statements are written by hand now, fewer than %d: lower rawGoBaseline to %d", raw, rawGoBaseline, raw)
	}
	switch {
	case unrecovered > unrecoveredGoBaseline:
		t.Errorf("%d goroutines have no recover, more than %d: a panic on one ends the process; use helpers.GoSafeNamed", unrecovered, unrecoveredGoBaseline)
	case unrecovered < unrecoveredGoBaseline:
		t.Errorf("%d goroutines have no recover now, fewer than %d: lower unrecoveredGoBaseline to %d", unrecovered, unrecoveredGoBaseline, unrecovered)
	}
}

// bareTokenWait is an MQTT token waited on with no bound.
var bareTokenWait = regexp.MustCompile(`\b(token|tok|res|mqttClientRes)\.Wait\(\)`)

// An MQTT publish waits for the broker for at most mqttInit.PublishWait.
//
// WHY. Every publish waited on its token with a bare Wait(): with the broker
// slow or gone, each one's goroutine stayed parked, one per message, until it
// came back. This reads the files that use the MQTT client for a token waited
// on without WaitTimeout. It baselines nothing.
func TestMqttWaitsAreBounded(t *testing.T) {
	read := 0
	for _, root := range []string{"../business", "../initializers", "../controllers", "../services"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			src := string(b)
			if !strings.Contains(src, "mqttInit") && !strings.Contains(src, "paho.mqtt") {
				return nil
			}
			read++
			for i, line := range strings.Split(src, "\n") {
				if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
					continue
				}
				if bareTokenWait.MatchString(line) {
					t.Errorf("%s:%d waits on an MQTT token with no bound; use WaitTimeout(mqttInit.PublishWait)", path, i+1)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if read < 5 {
		t.Fatalf("read %d MQTT files; the guard is no longer looking at them", read)
	}
}

// namedLoopAllowed are GoSafeNamed goroutines that loop on a context but end
// with a piece of work, by file and why.
var namedLoopAllowed = map[string]string{
	"business/Import/pages_worker.go": "keepClaimed heartbeats one page's claim and ends with the page",
	"domain/Doc/searchEntry.go":       "a doc's search write loops only while asks for that doc keep coming",
}

// A goroutine that lasts as long as its context starts with GoSafeLoop.
//
// WHY. GoSafeNamed counts its goroutine as work in flight, which
// SettleBackground (every integration test's teardown, a shutdown) waits for.
// Three loops filed under it (a ticker, a stream read, a context) held every
// test's teardown for three minutes until the suite timed out. This reads every
// GoSafeNamed whose function, or the one function it calls, loops at its top
// level on a context, a ticker or a subscription. It baselines nothing.
func TestLongLivedGoroutinesAreLoops(t *testing.T) {
	type body struct {
		b   *ast.BlockStmt
		src []byte
	}
	funcs := map[string]body{}
	type use struct {
		path, key string
		lit       *ast.FuncLit
		src       []byte
		line      int
	}
	var uses []use
	fset := token.NewFileSet()
	for _, root := range []string{"../business", "../controllers", "../domain", "../initializers", "../services"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			dir := filepath.Dir(path)
			for _, decl := range f.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
					sep := "."
					if fd.Recv != nil {
						sep = "#"
					}
					funcs[dir+sep+fd.Name.Name] = body{fd.Body, src}
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				s, ok := c.Fun.(*ast.SelectorExpr)
				if !ok || s.Sel.Name != "GoSafeNamed" || len(c.Args) != 2 {
					return true
				}
				lit, ok := c.Args[1].(*ast.FuncLit)
				if !ok {
					return true
				}
				u := use{path: path, lit: lit, src: src, line: fset.Position(c.Pos()).Line}
				if len(lit.Body.List) == 1 {
					if es, ok := lit.Body.List[0].(*ast.ExprStmt); ok {
						if ce, ok := es.X.(*ast.CallExpr); ok {
							switch fn := ce.Fun.(type) {
							case *ast.Ident:
								u.key = dir + "." + fn.Name
							case *ast.SelectorExpr:
								u.key = dir + "#" + fn.Sel.Name
							}
						}
					}
				}
				uses = append(uses, u)
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	text := func(n ast.Node, src []byte) string {
		return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
	}
	longLived := func(b *ast.BlockStmt, src []byte) bool {
		if b == nil {
			return false
		}
		for _, st := range b.List {
			switch l := st.(type) {
			case *ast.ForStmt:
				if l.Cond == nil {
					return true
				}
				if c := text(l.Cond, src); strings.Contains(c, ".Err()") || strings.Contains(c, "Done()") {
					return true
				}
			case *ast.RangeStmt:
				if x := text(l.X, src); strings.HasSuffix(x, ".C") || strings.HasSuffix(x, "Channel()") || strings.Contains(x, "Tick") {
					return true
				}
			}
		}
		return false
	}
	for _, u := range uses {
		rel, _ := filepath.Rel("..", u.path)
		if _, ok := namedLoopAllowed[filepath.ToSlash(rel)]; ok {
			continue
		}
		ll := longLived(u.lit.Body, u.src)
		if !ll && u.key != "" {
			if f, ok := funcs[u.key]; ok {
				ll = longLived(f.b, f.src)
			}
		}
		if ll {
			t.Errorf("%s:%d: a goroutine that lasts as long as its context is started with GoSafeNamed, which counts it as work in flight; use GoSafeLoop", u.path, u.line)
		}
	}
	for rel := range namedLoopAllowed {
		if _, err := os.Stat("../" + rel); err != nil {
			t.Errorf("%s is allowed but gone; take it off namedLoopAllowed", rel)
		}
	}
}
