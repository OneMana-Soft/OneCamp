package domain

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	readStateModels "github.com/akashc777/OneCamp/models/postgres/ReadState"
)

// The two unread-count queries must ask the same question.
//
// A channel's unread count and a conversation's unread count are the same idea
// over different tables, and they were written months apart by the same hand
// with different care. The channel one excluded your own posts, excluded deleted
// posts, and compared strictly after the marker. The chat one did none of those.
//
// The consequence was a badge that could not be cleared by reading, because
// nothing you could do would make the count fall:
//
//   - your own message counted against you, and CreateChat stamps the row with
//     Postgres NOW() AFTER the Go timestamp it stores as your last_seen, so with
//     >= every message you sent registered as one unread to yourself
//   - a deleted message counted forever, and there was nothing left to open
//
// Since 20 Oct 2026 the rule is written once (models/postgres/ReadState
// Table.Unread) and both queries join on it, as the first unread and Mark
// unread do. The guard reads each query as it runs: its literals, and each
// shared fragment it joins in, replaced by the fragment's own text. This pins
// the parity rather than the text, so either query may be rewritten as long as
// it still answers the question the same way.
func TestUnreadCountQueriesAgree(t *testing.T) {
	fragments := map[string]string{
		"readStateModels.Chat.Unread()":    readStateModels.Chat.Unread(),
		"readStateModels.Channel.Unread()": readStateModels.Channel.Unread(),
	}
	chatQ := extractQuery(t, "chatDomain.go", "GetLatestChatMessageCountByUserID", fragments)
	channelQ := extractQuery(t, "../Post/postDomain.go", "GetLatestPostInChannelCountByUserID", fragments)

	// Both count by the read state's one rule.
	if !strings.Contains(chatQ, readStateModels.Chat.Unread()) {
		t.Errorf("the chat unread query no longer joins on the read state's rule (readStateModels.Chat.Unread()):\n%s", chatQ)
	}
	if !strings.Contains(channelQ, readStateModels.Channel.Unread()) {
		t.Errorf("the channel unread query no longer joins on the read state's rule (readStateModels.Channel.Unread()):\n%s", channelQ)
	}

	for _, c := range []struct {
		name string
		chat string
		chnl string
	}{
		{"excludes messages you sent yourself", "created_by <> lsc.user_id", "created_by <> lsc.user_id"},
		{"excludes deleted messages", "deleted_at IS NULL", "deleted_at IS NULL"},
	} {
		if !strings.Contains(chatQ, c.chat) {
			t.Errorf("the chat unread query no longer %s: missing %q", c.name, c.chat)
		}
		if !strings.Contains(channelQ, c.chnl) {
			t.Errorf("the channel unread query no longer %s: missing %q", c.name, c.chnl)
		}
	}

	// Strictly after the marker, never at it. The marker is written at the moment
	// of reading, so a row stamped at that instant is one the reader just saw.
	for name, q := range map[string]string{"chat": chatQ, "channel": channelQ} {
		if strings.Contains(q, ">= lsc.user_last_seen") {
			t.Errorf("the %s unread query uses >= against the last-seen marker; a message stamped at "+
				"the instant of reading is one the reader has seen, and >= counts it as unread forever", name)
		}
		if !strings.Contains(q, "> lsc.user_last_seen") {
			t.Errorf("the %s unread query no longer compares against the last-seen marker at all", name)
		}
	}
}

// The guard reads a query built from a shared fragment as the query that runs,
// and refuses one it can't read rather than passing it unchecked.
func TestTheGuardReadsAQueryBuiltFromFragments(t *testing.T) {
	src := []byte("package p\n\nfunc Count() {\n\tquery := `SELECT 1 FROM a m ON ` + rule.Of() + `\n\t\tWHERE x`\n\t_ = query\n}\n")
	got, err := queryOf(src, "Count", map[string]string{"rule.Of()": "m.k = lsc.k"})
	if err != nil || got != "SELECT 1 FROM a m ON m.k = lsc.k\n\t\tWHERE x" {
		t.Fatalf("a query with a fragment: %q %v", got, err)
	}
	if _, err := queryOf(src, "Count", nil); err == nil || !strings.Contains(err.Error(), "rule.Of()") {
		t.Errorf("a fragment the guard doesn't know must fail, naming it: %v", err)
	}
	if _, err := queryOf(src, "Gone", nil); err == nil || !strings.Contains(err.Error(), "Gone") {
		t.Errorf("a function that isn't there must fail, naming it: %v", err)
	}
	plain := []byte("package p\n\nfunc Count() {\n\tquery := `SELECT 1`\n\t_ = query\n}\n")
	if got, err := queryOf(plain, "Count", nil); err != nil || got != "SELECT 1" {
		t.Errorf("a query of one literal: %q %v", got, err)
	}
}

// extractQuery is the SQL the named function in the file at path runs
// (queryOf), failing the test where it can't be read.
func extractQuery(t *testing.T, path, fn string, fragments map[string]string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s: %v", path, err)
	}
	q, err := queryOf(src, fn, fragments)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return q
}

// queryOf is the SQL function fn in src assigns to its query variable: its
// string literals, joined with +, and each other operand replaced by its text
// in fragments (keyed by the operand's source, "readStateModels.Chat.Unread()").
// An operand not in fragments is an error naming it, so a part of the query the
// guard can't see never passes for checked.
func queryOf(src []byte, fn string, fragments map[string]string) (string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		return "", err
	}
	var body *ast.BlockStmt
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == fn && f.Body != nil {
			body = f.Body
			break
		}
	}
	if body == nil {
		return "", fmt.Errorf("function %s not found; if it was renamed, this guard needs the new name", fn)
	}
	var expr ast.Expr
	ast.Inspect(body, func(n ast.Node) bool {
		if expr != nil {
			return false
		}
		if a, ok := n.(*ast.AssignStmt); ok && len(a.Lhs) == 1 && len(a.Rhs) == 1 {
			if id, ok := a.Lhs[0].(*ast.Ident); ok && id.Name == "query" {
				expr = a.Rhs[0]
				return false
			}
		}
		return true
	})
	if expr == nil {
		return "", fmt.Errorf("%s builds no query variable", fn)
	}
	var out strings.Builder
	var walk func(ast.Expr) error
	walk = func(e ast.Expr) error {
		switch v := e.(type) {
		case *ast.ParenExpr:
			return walk(v.X)
		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				if err := walk(v.X); err != nil {
					return err
				}
				return walk(v.Y)
			}
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				s, err := strconv.Unquote(v.Value)
				if err != nil {
					return err
				}
				out.WriteString(s)
				return nil
			}
		}
		text := string(src[fset.Position(e.Pos()).Offset:fset.Position(e.End()).Offset])
		frag, ok := fragments[text]
		if !ok {
			return fmt.Errorf("%s joins %s into its query, which this guard can't read: add its text to the fragments", fn, text)
		}
		out.WriteString(frag)
		return nil
	}
	if err := walk(expr); err != nil {
		return "", err
	}
	return out.String(), nil
}
