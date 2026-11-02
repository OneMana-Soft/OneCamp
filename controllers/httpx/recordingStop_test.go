package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
)

// A stop that found nothing recording is answered as the asker's state: 400
// with the plain message. A stop that worked, and any other failure, are left
// to the route.
func TestNothingRecordingIsTheAsksStateNotTheServers(t *testing.T) {
	rec := httptest.NewRecorder()
	if !WriteNothingRecording(rec, fmt.Errorf("stopping ch-1: %w", liveKitBusiness.ErrNothingRecording)) {
		t.Fatal("a stop with nothing recording was left unanswered")
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusBadRequest || body["msg"] != NothingRecording {
		t.Errorf("nothing recording: %d %q, want 400 %q", rec.Code, body["msg"], NothingRecording)
	}
	for name, err := range map[string]error{"a stop that worked": nil, "a call server that failed": errors.New("egress is down")} {
		rec := httptest.NewRecorder()
		if WriteNothingRecording(rec, err) || rec.Body.Len() != 0 {
			t.Errorf("%s was answered as nothing recording: %q", name, rec.Body.String())
		}
	}
}

// Every route that stops a call's recording shares the ONE stop
// (business/LiveKit StopRecording), so a stop with nothing recording gets ONE
// answer on all of them (main, 30 Oct): a function in controllers that calls
// the stop, or a seam bound to it, answers through WriteNothingRecording.
func TestEveryRouteThatStopsARecordingSaysNothingIsRecordingTheSameWay(t *testing.T) {
	const stopPath = "github.com/akashc777/OneCamp/business/LiveKit"
	const answerPath = "github.com/akashc777/OneCamp/controllers/httpx"
	fset := token.NewFileSet()
	byDir := map[string][]*ast.File{}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err == nil {
			byDir[filepath.Dir(path)] = append(byDir[filepath.Dir(path)], f)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// The name a file gives an import: business/LiveKit is package business,
	// so it is always named; httpx may not be.
	named := func(f *ast.File, path, unnamed string) string {
		for _, s := range f.Imports {
			if p, _ := strconv.Unquote(s.Path.Value); p == path {
				if s.Name != nil {
					return s.Name.Name
				}
				return unnamed
			}
		}
		return ""
	}
	is := func(e ast.Expr, pkg, name string) bool {
		s, ok := e.(*ast.SelectorExpr)
		if !ok || pkg == "" {
			return false
		}
		x, ok := s.X.(*ast.Ident)
		return ok && x.Name == pkg && s.Sel.Name == name
	}
	var stoppers, own []string
	for dir, files := range byDir {
		seams := map[string]bool{} // package-level vars bound to the stop
		for _, f := range files {
			lk := named(f, stopPath, "")
			for _, decl := range f.Decls {
				if g, ok := decl.(*ast.GenDecl); ok && g.Tok == token.VAR {
					for _, spec := range g.Specs {
						vs := spec.(*ast.ValueSpec)
						for i, v := range vs.Values {
							if is(v, lk, "StopRecording") && i < len(vs.Names) {
								seams[vs.Names[i].Name] = true
							}
						}
					}
				}
			}
		}
		for _, f := range files {
			lk, hx := named(f, stopPath, ""), named(f, answerPath, "httpx")
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				stops, answers := false, false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok {
						id, isIdent := c.Fun.(*ast.Ident)
						stops = stops || is(c.Fun, lk, "StopRecording") || (isIdent && seams[id.Name])
						answers = answers || is(c.Fun, hx, "WriteNothingRecording")
					}
					return true
				})
				if !stops {
					continue
				}
				where := filepath.Join(dir, fn.Name.Name)
				stoppers = append(stoppers, where)
				if !answers {
					own = append(own, where)
				}
			}
		}
	}
	if len(stoppers) < 4 {
		t.Fatalf("found %d routes that stop a recording (%v), want the channel's, the group's, the DM's and the meeting's", len(stoppers), stoppers)
	}
	for _, where := range own {
		t.Errorf("%s stops a call's recording but answers 'nothing recording' its own way: answer it with httpx.WriteNothingRecording", where)
	}
}
