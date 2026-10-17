package helpers

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// What the server says to a person names what failed in their words, never
// which of the systems behind the product failed.
//
// WHY THIS EXISTS. A team's page answered "Failed to get dgraph team member
// list", and the web app shows a response's msg to the person (lib/
// axiosInstance.ts falls back through data?.msg), so a teammate was told the
// name of a database. Nearly a hundred messages read like that: "Failed to
// get task dgraph info", "Failed to remove refresh key in redis", "minio
// upload failed". To the reader they say nothing they can act on, and to
// anyone probing the server they say what it runs on. The log line beside
// each one keeps the detail; only what goes into the response changed.
//
// WHAT IT READS. Every string literal in a response's message: the message
// keys of an envelope (msg, message, error, err and the like, never a data
// field), the message of http.Error and helpers.ErrorJSON, and every literal
// passed to any call that writes to a handler's http.ResponseWriter (w) —
// which covers the controllers' own helpers, fail(w, status, "..."), without
// listing them. Log calls take a context or a format, never w, and are not
// read: they are where the detail belongs.
//
// WHAT IT CANNOT READ. A message built elsewhere and passed in a variable,
// and an error's own text put in msg ("msg": err.Error()); those belong to
// review (see TestErrorStringsInResponsesDoNotGrow for the second).
var internalSystemName = regexp.MustCompile(`(?i)\b(dgraph|postgres(ql)?|pgx?|pq|sqlx|redis|opensearch|elastic(search)?|minio|s3|clickhouse|emqx|mqtt|hocuspocus|livekit|clamav|traefik)\b`)

// responseMessageKeys are the envelope keys (and struct fields, compared
// without case) whose values are words for the reader. Data fields are not
// read: a payload may well hold an id or a setting that names a system.
var responseMessageKeys = map[string]bool{
	"msg": true, "message": true, "error": true, "err": true, "detail": true, "details": true,
	"reason": true, "hint": true, "note": true, "warning": true,
}

// internalNameAllowed are messages that name a system because it is the
// reader's own: keyed "path|literal", each with why.
var internalNameAllowed = map[string]string{}

// internalNameScanRoots are the source trees whose handlers answer people.
var internalNameScanRoots = []string{
	"../adapter", "../business", "../cmd", "../controllers", "../domain", ".",
	"../initializers", "../middleware", "../models", "../router", "../services",
}

func TestResponsesNameNoInternalSystems(t *testing.T) {
	var offenders []string
	files := 0
	for _, root := range internalNameScanRoots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("scan root %s missing, so this check would pass on nothing: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || (path != root && strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files++
			found, perr := internalNamesInResponses(path)
			if perr != nil {
				return perr
			}
			offenders = append(offenders, found...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if files < 100 {
		t.Fatalf("read only %d files: the scan roots no longer hold the handlers", files)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d response message(s) name a system behind the product. Say what failed in the reader's "+
			"words (\"Couldn't load the team's members\"), and keep the detail in the log line:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// The check reads every shape a message reaches a person in, and nothing
// that only reaches the logs or a payload's data: otherwise a pass means
// nothing (it read none of them) or the check gets deleted (it flagged logs).
func TestInternalNamesGuardReadsMessagesNotLogs(t *testing.T) {
	const fixture = `package fixture

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, 500, helpers.Envolope{"msg": "Failed to get dgraph team"})                        // 1 flagged
	http.Error(w, "redis is down", 500)                                                                      // 2 flagged
	fail(w, 500, "minio upload failed")                                                                      // 3 flagged
	helpers.WriteJSON(w, 500, helpers.Envolope{"error": fmt.Sprintf("couldn't reach %s", "OpenSearch")})    // 4 flagged
	helpers.ErrorJSON(w, errors.New("pq: duplicate key"), 400)                                               // 5 flagged
	w.Write([]byte("clickhouse said no"))                                                                    // 6 flagged
	json.NewEncoder(w).Encode(map[string]string{"message": "LiveKit unreachable"})                           // 7 flagged
	answer := helpers.Envolope{"msg": "the mqtt broker refused"}                                             // 8 flagged
	helpers.WriteJSON(w, 500, answer)
	http.Redirect(w, r, signInError("session_failed", "Couldn't reach Postgres"), 302)                       // 9 flagged

	helpers.LogErrorWithContext(r.Context(), "controllers/Handler dgraph read err: %v", nil)                  // a log: not read
	helpers.WriteJSON(w, 200, helpers.Envolope{"data": map[string]string{"provider": "s3"}})               // data: not read
	helpers.WriteJSON(w, 200, helpers.Envolope{"msg": "Couldn't load the team's members."})                 // plain words
	notAWriter("dgraph")                                                                                     // no writer
}

func NotAHandler(w *bufio.Writer) {
	fmt.Fprintf(w, "postgres dump header")                                                                   // not a response writer
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.go")
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := internalNamesInResponses(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(fixture, "\n")
	var want []int
	for i, l := range lines {
		if strings.Contains(l, "flagged") {
			want = append(want, i+1)
		}
	}
	var got []int
	for _, f := range found {
		parts := strings.SplitN(f, ":", 3)
		if len(parts) < 3 {
			t.Fatalf("finding without a file and line: %q", f)
		}
		n, _ := strconv.Atoi(parts[1])
		got = append(got, n)
	}
	sort.Ints(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("flagged lines %v, want %v:\n  %s", got, want, strings.Join(found, "\n  "))
	}
}

// internalNamesInResponses is every response message literal in one file
// that names an internal system and isn't allowed, as "path:line: text".
func internalNamesInResponses(path string) ([]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	rel := filepath.ToSlash(strings.TrimPrefix(path, "../"))

	// The bodies of functions that hold a response writer named w.
	type span struct{ from, to token.Pos }
	var writers []span
	ast.Inspect(file, func(n ast.Node) bool {
		var ft *ast.FuncType
		var body *ast.BlockStmt
		switch fn := n.(type) {
		case *ast.FuncDecl:
			ft, body = fn.Type, fn.Body
		case *ast.FuncLit:
			ft, body = fn.Type, fn.Body
		}
		if ft != nil && body != nil && hasResponseWriterW(ft) {
			writers = append(writers, span{body.Pos(), body.End()})
		}
		return true
	})
	inWriter := func(p token.Pos) bool {
		for _, s := range writers {
			if p >= s.from && p < s.to {
				return true
			}
		}
		return false
	}

	seen := map[token.Pos]bool{}
	var out []string
	check := func(lit *ast.BasicLit) {
		if seen[lit.Pos()] {
			return
		}
		seen[lit.Pos()] = true
		text, err := strconv.Unquote(lit.Value)
		if err != nil || !internalSystemName.MatchString(text) {
			return
		}
		if _, ok := internalNameAllowed[rel+"|"+text]; ok {
			return
		}
		out = append(out, fmt.Sprintf("%s:%d: %q", rel, fset.Position(lit.Pos()).Line, text))
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			// An envelope is a response body wherever it is built.
			if isEnvelopeType(x.Type) {
				messageLiterals(x, check)
			}
		case *ast.CallExpr:
			name := calleeName(x.Fun)
			switch {
			case name == "WriteJSON" && len(x.Args) >= 3:
				messageLiterals(x.Args[2], check)
			case (name == "http.Error" || name == "ErrorJSON") && len(x.Args) >= 2:
				messageLiterals(x.Args[1], check)
			case len(x.Args) > 0 && isIdent(x.Args[0], "w") && inWriter(x.Pos()):
				// Anything handed the response writer writes the rest of
				// its arguments into the response.
				for _, a := range x.Args[1:] {
					messageLiterals(a, check)
				}
			case isWriterMethod(x.Fun) && inWriter(x.Pos()):
				// w.Write([]byte("...")), json.NewEncoder(w).Encode(...).
				for _, a := range x.Args {
					messageLiterals(a, check)
				}
			}
		}
		return true
	})
	return out, nil
}

// messageLiterals visits the string literals in a response value that are
// words for the reader: everything but the values of data fields.
func messageLiterals(e ast.Expr, visit func(*ast.BasicLit)) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			visit(x)
		}
	case *ast.CompositeLit:
		for _, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if isMessageKey(kv.Key) {
					messageLiterals(kv.Value, visit)
				}
				continue
			}
			messageLiterals(el, visit)
		}
	case *ast.CallExpr: // fmt.Sprintf("...", ...), errors.New("..."), []byte("...")
		for _, a := range x.Args {
			messageLiterals(a, visit)
		}
	case *ast.BinaryExpr:
		messageLiterals(x.X, visit)
		messageLiterals(x.Y, visit)
	case *ast.ParenExpr:
		messageLiterals(x.X, visit)
	case *ast.UnaryExpr:
		messageLiterals(x.X, visit)
	}
}

func isMessageKey(k ast.Expr) bool {
	switch key := k.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(key.Value)
		return err == nil && responseMessageKeys[strings.ToLower(s)]
	case *ast.Ident:
		return responseMessageKeys[strings.ToLower(key.Name)]
	}
	return false
}

func isEnvelopeType(t ast.Expr) bool {
	switch x := t.(type) {
	case *ast.Ident:
		return x.Name == "Envolope"
	case *ast.SelectorExpr:
		return x.Sel.Name == "Envolope"
	}
	return false
}

// calleeName is "WriteJSON" for helpers.WriteJSON or WriteJSON, "http.Error"
// for http.Error, and the bare name otherwise.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if pkg, ok := f.X.(*ast.Ident); ok && pkg.Name == "http" {
			return "http." + f.Sel.Name
		}
		return f.Sel.Name
	}
	return ""
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// isWriterMethod is w.Write / w.WriteString, or an encoder made on w.
func isWriterMethod(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if isIdent(sel.X, "w") && (sel.Sel.Name == "Write" || sel.Sel.Name == "WriteString") {
		return true
	}
	if inner, ok := sel.X.(*ast.CallExpr); ok && sel.Sel.Name == "Encode" && len(inner.Args) > 0 && isIdent(inner.Args[0], "w") {
		return true
	}
	return false
}

// hasResponseWriterW reports a parameter "w http.ResponseWriter".
func hasResponseWriterW(ft *ast.FuncType) bool {
	if ft.Params == nil {
		return false
	}
	for _, field := range ft.Params.List {
		sel, ok := field.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ResponseWriter" {
			continue
		}
		for _, n := range field.Names {
			if n.Name == "w" {
				return true
			}
		}
	}
	return false
}
