package helpers

// API tokens act as members, and stay so: a guard over the code a token's call
// reaches, with no exceptions (a zero baseline).
//
// A token carries none of its owner's admin powers (ActsThroughToken). The ways
// that went wrong were all one shape: code a token reaches built an actor or an
// Access subject straight from the account's admin flag, or decided a write by
// a role, with nothing asking whether a token made the call. MCP's tables and
// data sources, the public API's tables, the AI executors MCP runs, and the
// shared send rule all did. This walks that code and fails on:
//
//   - an actor whose IsAdmin, or a subject whose Admin, is taken from an
//     account's IsAdmin without asking ActsThroughToken in the same expression;
//   - accessBusiness.SubjectOf, which keeps the admin flag: SubjectFor asks;
//   - a decision by an admin role (the workspace's, a channel's, a team's, a
//     project's: an Is…Admin compared, negated or tested) in a function that
//     never asks ActsThroughToken; or in a helper that cannot ask (it has no
//     context), called by one that doesn't;
//   - a case of MCP's reach (business/MCPServer/reach.go) decided by an admin
//     role that never asks viaToken (the reach asks once, at its top);
//   - the shared send rule's announcement-channel check without ActsThroughToken.

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

// tokenReachableRoots is the code a token's call runs: the public API, the MCP
// surface, the AI executors MCP runs (and agent runs a token asks for), the
// shared send rule the public API posts through, and who a message reaches,
// which that rule asks (chat P3: a channel's managers broadcast in the app,
// never through a token).
// business/App and business/PublicAPI (platform P-API, 17 Oct 2026): every app's
// call is an API token's call, and the public API's one shape is what every
// /v1 route runs through, so both act as a member at most, with no exceptions.
// business/Hooks (P-API part 2, 18 Oct): an app's own webhook subscriptions
// arrive there, so it decides nothing by an admin role either.
// Who knows (DG9): a token asks it through the public API.
//
// Without AI there is no MCP surface and no AI executor: a token reaches the
// rest alone (the AI edition also reads business/MCPServer and business/AI,
// and MCP's reach case by case).
// Person Preferences (calendar A3): a person reads and sets only their own,
// whoever asks for them.
var tokenReachableRoots = []string{"../controllers/V1", "../business/Send", "../business/Reach", "../business/App", "../business/PublicAPI", "../business/Hooks", "../business/WhoKnows", "../controllers/WhoKnows", "../business/Preferences", "../controllers/Preferences"}

// reachFunc is MCP's reach, which the AI edition checks case by case; this
// edition has no MCP, so no function of the roots is named so.
const reachFunc = "PrincipalCanReach"

func TestTokenReachableCodeActsAsAMember(t *testing.T) {
	var findings []string
	var funcs []*roleFunc
	parsed := 0
	for _, root := range tokenReachableRoots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("%s is missing; this guard would pass by reading nothing: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			parsed++
			findings = append(findings, adminReadsWithoutTheTokenTest(fset, file)...)
			funcs = append(funcs, roleFuncsOf(fset, file)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if parsed == 0 {
		t.Fatal("parsed no file; this guard has gone stale")
	}
	findings = append(findings, roleDecisionsWithoutTheToken(funcs)...)
	findings = append(findings, announcementPostsWithoutTheToken(t)...)
	for _, f := range findings {
		t.Error(f)
	}
}

// roleFunc is one function of the token-reachable code: whether it decides
// anything by an admin role, whether it asks if a token made the call, whether
// it could (a context or a request to ask), and what it calls.
type roleFunc struct {
	name, at        string
	decides, asks   bool
	canAsk          bool
	calls           map[string]bool
	firstDecisionAt string
}

// roleFuncsOf reads every function in file, declared or assigned to a
// package-level variable (a seam).
func roleFuncsOf(fset *token.FileSet, file *ast.File) []*roleFunc {
	var out []*roleFunc
	add := func(name string, typ *ast.FuncType, body *ast.BlockStmt, pos token.Pos) {
		if body == nil {
			return
		}
		f := &roleFunc{name: name, at: fset.Position(pos).String(), canAsk: takesAContext(typ), calls: map[string]bool{}}
		ast.Inspect(body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if name := funcName(x.Fun); name != "" {
					f.calls[name] = true
					if name == "ActsThroughToken" {
						f.asks = true
					}
				}
			case *ast.Ident:
				if x.Name == "viaToken" {
					f.asks = true
				}
			}
			if pos, ok := roleDecision(n); ok && !f.decides {
				f.decides, f.firstDecisionAt = true, fset.Position(pos).String()
			}
			return true
		})
		out = append(out, f)
	}
	for _, d := range file.Decls {
		switch x := d.(type) {
		case *ast.FuncDecl:
			if x.Name.Name == reachFunc && x.Recv == nil {
				continue
			}
			add(x.Name.Name, x.Type, x.Body, x.Pos())
		case *ast.GenDecl:
			for _, s := range x.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, v := range vs.Values {
					if lit, ok := v.(*ast.FuncLit); ok && i < len(vs.Names) {
						add(vs.Names[i].Name, lit.Type, lit.Body, lit.Pos())
					}
				}
			}
		}
	}
	return out
}

// roleDecisionsWithoutTheToken finds the functions that decide by an admin
// role and never ask whether a token made the call. One that cannot ask (no
// context, no request) is a helper: every function in this code that calls it
// has to ask instead.
func roleDecisionsWithoutTheToken(funcs []*roleFunc) []string {
	var out []string
	for _, f := range funcs {
		if !f.decides || f.asks {
			continue
		}
		if f.canAsk {
			out = append(out, f.firstDecisionAt+": "+f.name+" decides by an admin role and never asks helpers.ActsThroughToken: "+
				"a token's call would carry its owner's role")
			continue
		}
		for _, caller := range funcs {
			if caller.calls[f.name] && !caller.asks {
				out = append(out, caller.at+": "+caller.name+" calls "+f.name+", which decides by an admin role ("+f.firstDecisionAt+
					"), and never asks helpers.ActsThroughToken")
			}
		}
	}
	return out
}

// roleDecision is whether n decides by an admin role: one compared or joined
// (a binary expression), negated, or tested by an if or a switch.
func roleDecision(n ast.Node) (token.Pos, bool) {
	switch x := n.(type) {
	case *ast.BinaryExpr:
		if readsRole(x.X) || readsRole(x.Y) {
			return x.Pos(), true
		}
	case *ast.UnaryExpr:
		if x.Op == token.NOT && readsRole(x.X) {
			return x.Pos(), true
		}
	case *ast.IfStmt:
		if readsRole(x.Cond) {
			return x.Cond.Pos(), true
		}
	case *ast.SwitchStmt:
		if x.Tag != nil && readsRole(x.Tag) {
			return x.Tag.Pos(), true
		}
	case *ast.CaseClause:
		for _, e := range x.List {
			if readsRole(e) {
				return e.Pos(), true
			}
		}
	}
	return token.NoPos, false
}

// readsRole is whether e is itself a read of an admin role: a field, or a call
// that answers one (userDomain.IsActiveAdmin).
func readsRole(e ast.Expr) bool {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			break
		}
		e = p.X
	}
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return roleName(x.Sel.Name)
	case *ast.CallExpr:
		return roleName(funcName(x.Fun))
	}
	return false
}

// roleName is whether name reads someone's admin role: the workspace's (an
// account's IsAdmin), a channel's or a team's (IsAdmin on its read for them),
// a project's (IsProjectAdmin), a moderator's.
func roleName(name string) bool {
	return name == "IsModerator" || (strings.HasPrefix(name, "Is") && strings.HasSuffix(name, "Admin"))
}

// funcName is a called function's name, package or receiver dropped.
func funcName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// takesAContext is whether a function can ask about the call: it has a
// context.Context, or an *http.Request.
func takesAContext(typ *ast.FuncType) bool {
	if typ == nil || typ.Params == nil {
		return false
	}
	for _, p := range typ.Params.List {
		t := p.Type
		if star, ok := t.(*ast.StarExpr); ok {
			t = star.X
		}
		if sel, ok := t.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok &&
				((pkg.Name == "context" && sel.Sel.Name == "Context") || (pkg.Name == "http" && sel.Sel.Name == "Request")) {
				return true
			}
		}
	}
	return false
}

// adminReadsWithoutTheTokenTest finds actors and subjects built from an
// account's admin flag with no ActsThroughToken beside it, and SubjectOf calls.
func adminReadsWithoutTheTokenTest(fset *token.FileSet, file *ast.File) []string {
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			kind := typeName(x.Type)
			if !strings.HasSuffix(kind, "Actor") && kind != "Subject" {
				return true
			}
			for _, el := range x.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || (key.Name != "IsAdmin" && key.Name != "Admin") {
					continue
				}
				if mentions(kv.Value, "IsAdmin") && !calls(kv.Value, "ActsThroughToken") {
					out = append(out, fset.Position(kv.Pos()).String()+": a "+kind+"'s "+key.Name+
						" is the account's admin flag, with no helpers.ActsThroughToken: a token's call would carry its owner's admin powers")
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SubjectOf" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "accessBusiness" {
					out = append(out, fset.Position(x.Pos()).String()+
						": accessBusiness.SubjectOf keeps the admin flag through a token; use accessBusiness.SubjectFor(ctx, …)")
				}
			}
		}
		return true
	})
	return out
}

// announcementPostsWithoutTheToken checks the shared send rule refuses a token
// posting as an announcement channel's admin.
func announcementPostsWithoutTheToken(t *testing.T) []string {
	t.Helper()
	src := strippedSource(t, "../business/Send/send.go")
	body, ok := funcBody(src, "PrepareChannelPost")
	if !ok {
		return []string{"business/Send has no PrepareChannelPost; this guard has gone stale"}
	}
	// Since chat P3 the rule lives in prepareChannelPost, which
	// PrepareChannelPost and PrepareReplyCopy both run: follow it there.
	if !strings.Contains(body, "ActsThroughToken(") && strings.Contains(body, "prepareChannelPost(") {
		body, _ = funcBody(src, "prepareChannelPost")
	}
	if !strings.Contains(body, "ActsThroughToken(") {
		return []string{"business/Send's PrepareChannelPost lets an announcement channel's admin post through a token: ask helpers.ActsThroughToken"}
	}
	return nil
}

// funcBody is the source of the function name declares in src, up to the next
// function. Pure.
func funcBody(src, name string) (string, bool) {
	start := strings.Index(src, "func "+name+"(")
	if start < 0 {
		return "", false
	}
	body := src[start:]
	if end := strings.Index(body[1:], "\nfunc "); end > 0 {
		body = body[:end+1]
	}
	return body, true
}

// typeName is a composite literal's type name, package dropped.
func typeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.StarExpr:
		return typeName(x.X)
	}
	return ""
}

// mentions is whether e names the field or identifier name.
func mentions(e ast.Expr, name string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			found = found || x.Sel.Name == name
		case *ast.Ident:
			found = found || x.Name == name
		}
		return !found
	})
	return found
}

// calls is whether e calls a function named name, in any package.
func calls(e ast.Expr, name string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			switch f := c.Fun.(type) {
			case *ast.Ident:
				found = found || f.Name == name
			case *ast.SelectorExpr:
				found = found || f.Sel.Name == name
			}
		}
		return !found
	})
	return found
}

// strippedSource is a file with its // comments dropped, so prose cannot
// satisfy a check.
func strippedSource(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")
}
