package helpers

import (
	"bytes"
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestALinkSecretIsUnguessableAndKeptOnlyAsItsHash(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		raw, hash, err := NewLinkSecret()
		if err != nil {
			t.Fatal(err)
		}
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || len(b) != 32 {
			t.Fatalf("%q is not 32 bytes, URL-safe: %v", raw, err)
		}
		if seen[raw] {
			t.Fatal("the same secret twice")
		}
		seen[raw] = true
		if !bytes.Equal(hash, LinkSecretHash(raw)) || len(hash) != 32 || bytes.Contains(hash, []byte(raw)) {
			t.Fatal("the hash is the secret's SHA-256, and isn't the secret")
		}
	}
}

func TestASealedLinkSecretOpensOnlyAsTheSecretItsHashFinds(t *testing.T) {
	t.Setenv("APP_SECRET_KEK", "the server's key")
	raw, hash, sealed, err := NewSealedLinkSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hash, LinkSecretHash(raw)) || sealed == "" || strings.Contains(sealed, raw) {
		t.Fatalf("hash %x, sealed %q: the hash finds the secret, and the seal doesn't show it", hash, sealed)
	}
	if got := OpenLinkSecret(sealed, hash); got != raw {
		t.Errorf("opened %q, want %q", got, raw)
	}
	_, _, another, err := NewSealedLinkSecret()
	if err != nil {
		t.Fatal(err)
	}
	if got := OpenLinkSecret(another, hash); got != "" {
		t.Errorf("another link's seal opened as this one's: %q", got)
	}
	if got := OpenLinkSecret("", hash); got != "" {
		t.Errorf("no seal opened as %q", got)
	}
	t.Setenv("APP_SECRET_KEK", "the server's key, changed since")
	if got := OpenLinkSecret(sealed, hash); got != "" {
		t.Errorf("a seal opened under another key: %q", got)
	}
}

// A link's secret matches only the hash it was kept as, and is compared in
// constant time: LinkSecretMatches goes through crypto/subtle, never a plain
// comparison, so how long a wrong guess takes says nothing of how close it
// was.
func TestALinkSecretMatchesOnlyItsHashInConstantTime(t *testing.T) {
	raw, hash, err := NewLinkSecret()
	if err != nil {
		t.Fatal(err)
	}
	other, _, _ := NewLinkSecret()
	if !LinkSecretMatches(raw, hash) || LinkSecretMatches(other, hash) || LinkSecretMatches(raw, hash[:16]) || LinkSecretMatches(raw, nil) || LinkSecretMatches("", hash) {
		t.Fatal("a link secret matched another's hash, or not its own")
	}
	file, err := parser.ParseFile(token.NewFileSet(), "linkSecret.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	constantTime, plain := false, false
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "LinkSecretMatches" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "subtle" && x.Sel.Name == "ConstantTimeCompare" {
					constantTime = true
				}
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "bytes" {
					plain = true
				}
			case *ast.BinaryExpr:
				// Two computed values compared with == or != (len aside), or
				// a string conversion's.
				if (x.Op == token.EQL || x.Op == token.NEQ) && (comparedCall(x.X) && comparedCall(x.Y) || conversion(x.X) || conversion(x.Y)) {
					plain = true
				}
			}
			return true
		})
	}
	if !constantTime || plain {
		t.Fatalf("LinkSecretMatches compares without crypto/subtle (constant time %v, plain comparison %v)", constantTime, plain)
	}
}

// comparedCall is whether e is a call other than len's.
func comparedCall(e ast.Expr) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := c.Fun.(*ast.Ident)
	return !ok || id.Name != "len"
}

// conversion is whether e is string(...).
func conversion(e ast.Expr) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := c.Fun.(*ast.Ident)
	return ok && id.Name == "string"
}
