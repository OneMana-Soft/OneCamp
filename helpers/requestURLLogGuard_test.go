package helpers

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A log line names a request by its route (RouteOf), never by its URL. A path
// carries ids and, on the guest and public routes, a link's token; a query or
// a Referer can carry one too; and otel_logs keeps every line for 30 days. So
// no log call in the server's code takes a URL, a request's RequestURI or its
// Referer (the CSRF check logs the Referer's origin). The router's
// TestARequestIsTracedAndLoggedByItsRouteAlone asks every route through the
// base middleware; this reads the lines a route's own code writes.
func TestNoLogLineTakesARequestsURL(t *testing.T) {
	fset := token.NewFileSet()
	var hits []string
	for _, path := range productionGoFiles(t) {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		hits = append(hits, urlsLogged(fset, file)...)
	}
	if len(hits) > 0 {
		t.Errorf("%d log calls take a request's URL; name the request by helpers.RouteOf(r):\n%s", len(hits), strings.Join(hits, "\n"))
	}
}

func TestUrlsLoggedFindsEachWayToLogAURL(t *testing.T) {
	const src = `package p
func f(r *http.Request, hook webhook) {
	helpers.LogWarnWithContext(r.Context(), "for %s %s", r.Method, r.URL.Path)
	helpers.LogErrorWithContext(ctx, "asked %s", r.URL.RawQuery)
	LogWithContext(ctx, "warn", "asked %s", r.URL)
	helpers.LogInfoWithContext(ctx, "asked %s", r.RequestURI)
	helpers.LogInfoWithContext(ctx, "from %s", r.Referer())
	helpers.LogInfoWithContext(ctx, "from %s", r.Header.Get("Referer"))
	log.Printf("asked %s", r.URL.String())
	helpers.LogInfoWithContext(ctx, "days %s", r.URL.Query().Get("days"))
	helpers.LogWarnWithContext(r.Context(), "%s %s refused", r.Method, helpers.RouteOf(r))
	helpers.LogWarnWithContext(r.Context(), "origin %s", r.Header.Get("Origin"))
	authBusiness.Login(ctx, r.URL.Path)
	helpers.WriteJSON(w, 200, r.URL.Path)
	helpers.LogWarnWithContext(ctx, "fetch %s failed", meta.URL)
}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	got := urlsLogged(fset, file)
	want := []string{
		"p.go:3: r.URL.Path",
		"p.go:4: r.URL.RawQuery",
		"p.go:5: r.URL",
		"p.go:6: r.RequestURI",
		"p.go:7: r.Referer()",
		"p.go:8: r.Header.Get(\"Referer\")",
		"p.go:9: r.URL.String()",
		"p.go:10: r.URL.Query().Get(\"days\")",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("found\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// logFunc is a logging function's name: helpers' Log* family (LogWithContext,
// LogWarnWithContext, LogReadFailure...), and not Login or Logout.
var logFunc = regexp.MustCompile(`^Log([A-Z]|$)`)

// urlsLogged lists the arguments of file's log calls (helpers' Log*, and the
// log and slog packages') that read a request's URL, RequestURI or Referer,
// one "file:line: expression" each.
func urlsLogged(fset *token.FileSet, file *ast.File) []string {
	var hits []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isLogCall(call) {
			return true
		}
		for _, arg := range call.Args {
			if part := urlPartOf(arg); part != nil {
				at := fset.Position(part.Pos())
				hits = append(hits, fmt.Sprintf("%s:%d: %s", at.Filename, at.Line, types.ExprString(part)))
			}
		}
		return true
	})
	return hits
}

func isLogCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return logFunc.MatchString(fn.Name)
	case *ast.SelectorExpr:
		if pkg, ok := fn.X.(*ast.Ident); ok && (pkg.Name == "log" || pkg.Name == "slog") {
			return true
		}
		return logFunc.MatchString(fn.Sel.Name)
	}
	return false
}

// urlPartOf is the outermost expression in e that reads a request's URL
// (whole, or its path, query or text), its RequestURI, its Referer() or a
// Referer header; nil when e reads none.
func urlPartOf(e ast.Expr) ast.Expr {
	var found ast.Expr
	ast.Inspect(e, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Referer" || isRefererHeader(x, sel)) {
				found = x
			} else if readsRequestURL(x) {
				found = x
			}
		case *ast.SelectorExpr:
			if readsRequestURL(x) {
				found = x
			}
		}
		return found == nil
	})
	return found
}

// readsRequestURL: e is a request's URL or RequestURI, or a field or call
// down from one (r.URL.Path, r.URL.Query().Get(...), r.URL.String()). A
// request is r, req or request, or a response's .Request, as everywhere in
// this code; another .URL (an import's source file, say) isn't one.
func readsRequestURL(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		if x.Sel.Name == "RequestURI" || (x.Sel.Name == "URL" && isRequest(x.X)) {
			return true
		}
		return readsRequestURL(x.X)
	case *ast.CallExpr:
		return readsRequestURL(x.Fun)
	}
	return false
}

func isRequest(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "r" || x.Name == "req" || x.Name == "request"
	case *ast.SelectorExpr:
		return x.Sel.Name == "Request"
	}
	return false
}

// isRefererHeader: call is X.Header.Get("Referer").
func isRefererHeader(call *ast.CallExpr, sel *ast.SelectorExpr) bool {
	header, ok := sel.X.(*ast.SelectorExpr)
	if sel.Sel.Name != "Get" || !ok || header.Sel.Name != "Header" || len(call.Args) != 1 {
		return false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	name, err := strconv.Unquote(lit.Value)
	return err == nil && strings.EqualFold(name, "Referer")
}
