package linear

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Every query the import sends is checked against Linear's public schema,
// recorded in testdata/schema-fields.json (from github.com/linear/linear,
// packages/sdk/src/schema.graphql; regenerate it with
// testdata/extract_schema.py, naming the types the queries reach). A field
// Linear has deprecated, or a query over Linear's complexity cap, fails
// here, before Linear retires the field and every import stops in silence,
// as Jira's /search and Todoist's Sync v9 did (22 Oct 2026).
func TestEveryQueryReadsLiveFieldsUnderTheCap(t *testing.T) {
	raw, err := os.ReadFile("testdata/schema-fields.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Types map[string]map[string]struct {
			Type       string `json:"type"`
			Deprecated bool   `json:"deprecated"`
		} `json:"types"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	for name, q := range sentQueries(t) {
		sel, err := parseSelection(q)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var problems []string
		var cost func(typ string, fields []gqlField, path string) float64
		cost = func(typ string, fields []gqlField, path string) float64 {
			known, ok := schema.Types[typ]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: type %s isn't recorded (add it to testdata)", path, typ))
				return 0
			}
			sum := 0.0
			for _, f := range fields {
				at := path + "." + f.name
				def, ok := known[f.name]
				switch {
				case !ok:
					problems = append(problems, at+": no such field on "+typ)
					continue
				case def.Deprecated:
					problems = append(problems, at+": deprecated by Linear")
				}
				switch {
				case len(f.children) == 0:
					sum += 0.1 // a property
				case strings.HasSuffix(def.Type, "Connection"):
					// A connection multiplies what it holds by its page size
					// (first), or 50 when none is given.
					n := 50.0
					if f.first > 0 {
						n = float64(f.first)
					}
					sum++
					for _, c := range f.children {
						inner, ok := schema.Types[def.Type][c.name]
						if !ok {
							problems = append(problems, at+"."+c.name+": no such field on "+def.Type)
							continue
						}
						if c.name == "nodes" {
							sum += n * (1 + cost(inner.Type, c.children, at+".nodes"))
						} else {
							sum += 1 + cost(inner.Type, c.children, at+"."+c.name)
						}
					}
				default:
					sum += 1 + cost(def.Type, f.children, at) // an object
				}
			}
			return sum
		}
		total := cost("Query", sel, name)
		for _, p := range problems {
			t.Error(p)
		}
		// Linear rejects any query above 10,000 points, always.
		if total > 10000 {
			t.Errorf("%s: about %.0f points, over Linear's cap of 10,000 (a page would always be refused)", name, total)
		}
	}
}

// sentQueries is every query the import sends, by its constant's name, read
// from the package's code instead of a list kept by hand: the calls of gql,
// and of each function that hands one of its own parameters to such a call
// as the query (issuePages, rootList). A query sent any other way (a literal
// at the call, a string made at run time) fails, since the guard couldn't
// read it, and so does a *Query constant that nothing sends.
func sentQueries(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	consts := map[string]string{} // each string constant's value, by name
	funcs := map[string]*ast.FuncDecl{}
	for _, e := range entries {
		if n := e.Name(); !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, s := range d.Specs {
					vs, ok := s.(*ast.ValueSpec)
					if !ok || d.Tok != token.CONST {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, err := strconv.Unquote(lit.Value); err == nil {
								consts[name.Name] = v
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Body != nil {
					funcs[d.Name.Name] = d
				}
			}
		}
	}
	senders := map[string]int{"gql": 2} // which argument is the query
	for grew := true; grew; {
		grew = false
		for name, fn := range funcs {
			if _, ok := senders[name]; ok {
				continue
			}
			params := paramNames(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if _, arg, ok := queryArg(n, senders); ok {
					if id, ok := arg.(*ast.Ident); ok && slices.Contains(params, id.Name) {
						senders[name], grew = slices.Index(params, id.Name), true
					}
				}
				return true
			})
		}
	}
	sent := map[string]string{}
	for name, fn := range funcs {
		params := paramNames(fn)
		_, handsOn := senders[name]
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, arg, ok := queryArg(n, senders)
			if !ok {
				return true
			}
			id, _ := arg.(*ast.Ident)
			switch {
			case id != nil && consts[id.Name] != "":
				sent[id.Name] = consts[id.Name]
			case id != nil && handsOn && slices.Contains(params, id.Name):
				// Its callers' constants are what it sends.
			default:
				t.Errorf("%s: a query the guard can't read: send a string constant", fset.Position(call.Pos()))
			}
			return true
		})
	}
	for name := range consts {
		if strings.HasSuffix(name, "Query") && sent[name] == "" {
			t.Errorf("%s is never sent through gql", name)
		}
	}
	if len(sent) == 0 {
		t.Fatal("found no query sent through gql")
	}
	return sent
}

// queryArg is the query a call sends, when n calls one of the senders (by
// name, as a function, a method or a generic function's instance).
func queryArg(n ast.Node, senders map[string]int) (*ast.CallExpr, ast.Expr, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return nil, nil, false
	}
	fun := call.Fun
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	var name string
	switch f := fun.(type) {
	case *ast.Ident:
		name = f.Name
	case *ast.SelectorExpr:
		name = f.Sel.Name
	}
	i, ok := senders[name]
	if !ok || i >= len(call.Args) {
		return nil, nil, false
	}
	return call, call.Args[i], true
}

// paramNames is fn's parameters in order, its receiver aside.
func paramNames(fn *ast.FuncDecl) []string {
	var names []string
	for _, f := range fn.Type.Params.List {
		if len(f.Names) == 0 {
			names = append(names, "_")
		}
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
	}
	return names
}

// gqlField is one field of a selection set: its name, its page size when it
// says `first: N`, and what it selects.
type gqlField struct {
	name     string
	first    int
	children []gqlField
}

// parseSelection reads a query's top selection set: enough GraphQL for the
// import's queries (fields, arguments, nested sets; no fragments). Pure.
func parseSelection(q string) ([]gqlField, error) {
	toks := tokenize(q)
	i := 0
	for i < len(toks) && toks[i] != "{" {
		if toks[i] == "(" {
			i = skipParens(toks, i)
			continue
		}
		i++
	}
	if i == len(toks) {
		return nil, fmt.Errorf("no selection set")
	}
	fields, next, err := parseSet(toks, i)
	if err == nil && next != len(toks) {
		err = fmt.Errorf("text after the selection set")
	}
	return fields, err
}

func parseSet(toks []string, i int) ([]gqlField, int, error) {
	if toks[i] != "{" {
		return nil, i, fmt.Errorf("want {, got %q", toks[i])
	}
	i++
	var out []gqlField
	for i < len(toks) && toks[i] != "}" {
		f := gqlField{name: toks[i]}
		i++
		if i < len(toks) && toks[i] == "(" {
			end := skipParens(toks, i)
			for j := i; j < end-2; j++ {
				if toks[j] == "first" && toks[j+1] == ":" {
					f.first, _ = strconv.Atoi(toks[j+2])
				}
			}
			i = end
		}
		if i < len(toks) && toks[i] == "{" {
			children, next, err := parseSet(toks, i)
			if err != nil {
				return nil, next, err
			}
			f.children, i = children, next
		}
		out = append(out, f)
	}
	if i == len(toks) {
		return nil, i, fmt.Errorf("unclosed {")
	}
	return out, i + 1, nil
}

func skipParens(toks []string, i int) int {
	depth := 0
	for ; i < len(toks); i++ {
		switch toks[i] {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return i
}

func tokenize(q string) []string {
	var out []string
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == ' ' || c == '\n' || c == '\t' || c == '\r' || c == ',':
			i++
		case strings.ContainsRune("{}():$!", rune(c)):
			out = append(out, string(c))
			i++
		case c == '"':
			j := i + 1
			for j < len(q) && q[j] != '"' {
				j++
			}
			out = append(out, q[i:j+1])
			i = j + 1
		default:
			j := i
			for j < len(q) && !strings.ContainsRune(" \n\t\r,{}():$!\"", rune(q[j])) {
				j++
			}
			out = append(out, q[i:j])
			i = j
		}
	}
	return out
}
