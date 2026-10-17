package controllers

import (
	"encoding/json"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// QA SH-07 (14 Oct 2026): the demo's visitor was offered Set a password, two-step
// sign-in, Add a passkey, Calendar's Connect and the connectors, all of which
// the server refuses them. Their own profile now says they are the visitor, so
// the app leaves those out; anyone else's says nothing.
func TestTheOwnProfileSaysWhetherItIsTheDemoVisitor(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example.test")
	for _, c := range []struct {
		email string
		want  bool
	}{
		{"visitor@demo.example.test", true},
		{"Visitor@Demo.example.test", true},
		{"maya@demo.example.test", false},
	} {
		var p dgraphStruct.DgraphUser
		markDemoVisitor(&p, c.email)
		raw, _ := json.Marshal(p)
		if got := strings.Contains(string(raw), `"user_demo_visitor":true`); got != c.want {
			t.Errorf("%s: says visitor %v, want %v (%s)", c.email, got, c.want, raw)
		}
	}
	t.Setenv("DEMO_MODE", "false")
	var p dgraphStruct.DgraphUser
	markDemoVisitor(&p, "visitor@demo.example.test")
	if p.DemoVisitor {
		t.Error("a server that is not the demo has no visitor")
	}
}
