package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every task GitHub route checks the task before it touches anything: a read
// needs someone who may see the task (projectaccess.RequireTask with
// NeedView), a change someone who may change it (with NeedEditTask: Work
// Access decides), and a bulk change checks each task it names
// (changeRefusal). Eleven of them checked nothing
// but sign-in, so anyone could link a private task to any issue (and have its
// comments posted there), unlink, branch, open pull requests and search a
// linked private repository. The routes are read from the router, so a new one
// is held to the same rule; a guard that runs after the first read or write is
// as good as none.
func TestTaskGitHubRoutesCheckTheTaskFirst(t *testing.T) {
	src, err := os.ReadFile("../../router/router.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`r\.(Get|Post|Put|Patch|Delete)\("(/(?:github-[a-z-]+|create-branch)[^"]*)",\s*taskController\.(\w+)\)`).
		FindAllStringSubmatch(string(src), -1)
	if len(routes) < 13 {
		t.Fatalf("found %d task GitHub routes in the router, want at least 13: has the route list moved?", len(routes))
	}

	funcs := map[string]*ast.FuncDecl{}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "taskController.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
			funcs[fn.Name.Name] = fn
		}
	}

	// What reads or writes a task, its GitHub records or GitHub itself.
	effects := map[string]bool{"business": true, "taskDomain": true, "githubBusiness": true, "integrationDomain": true,
		"githubTaskActivityDomain": true, "githubPRReviewDomain": true, "syncBusiness": true}
	for _, rt := range routes {
		method, path, handler := rt[1], rt[2], rt[3]
		guard := "projectaccess.RequireTask(workAccess.NeedEditTask)"
		switch {
		case method == "Get":
			guard = "projectaccess.RequireTask(workAccess.NeedView)"
		case !strings.Contains(path, "{task_uuid}"):
			guard = "changeRefusal"
		}
		fn := funcs[handler]
		if fn == nil {
			t.Errorf("%s %s: no handler %s in taskController.go", method, path, handler)
			continue
		}
		calls := callsInOrder(fn.Body)
		at, first := -1, -1
		for i, c := range calls {
			if c == guard && at < 0 {
				at = i
			}
			if pkg, _, ok := strings.Cut(c, "."); ok && effects[pkg] && first < 0 {
				first = i
			}
		}
		switch {
		case at < 0:
			t.Errorf("%s %s (%s) never calls %s", method, path, handler, guard)
		case first >= 0 && first < at:
			t.Errorf("%s %s (%s) calls %s before %s", method, path, handler, calls[first], guard)
		}
	}

	// A bulk change asks the same question of each task: may this person change it?
	refusal := funcs["changeRefusal"]
	if refusal == nil {
		t.Fatal("changeRefusal is gone")
	}
	asksForChange := false
	ast.Inspect(refusal.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && callName(call) == "business.TaskFor" && needOf(call, 3) == "workAccess.NeedEditTask" {
			asksForChange = true
		}
		return true
	})
	if !asksForChange {
		t.Error("changeRefusal doesn't ask business.TaskFor whether the person may change the task")
	}
}

// callsInOrder is every call in body, in source order, by name: pkg.Func for a
// selector, the bare name otherwise; a task check with the need it asks,
// projectaccess.RequireTask(workAccess.NeedView).
func callsInOrder(body *ast.BlockStmt) []string {
	var calls []string
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if name := callName(call); name != "" {
				if name == "projectaccess.RequireTask" {
					name += "(" + needOf(call, 3) + ")"
				}
				calls = append(calls, name)
			}
		}
		return true
	})
	return calls
}

// needOf is the call's argument at i as pkg.Name, or "" when it isn't one.
func needOf(call *ast.CallExpr, i int) string {
	if len(call.Args) <= i {
		return ""
	}
	if sel, ok := call.Args[i].(*ast.SelectorExpr); ok {
		if x, ok := sel.X.(*ast.Ident); ok {
			return x.Name + "." + sel.Sel.Name
		}
	}
	return ""
}

func callName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			return x.Name + "." + f.Sel.Name
		}
	case *ast.Ident:
		return f.Name
	}
	return ""
}
