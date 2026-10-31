package models

import "testing"

// A system line and a call's post are the server's; an ordinary message, and
// no message at all, are not (calls CALLS-3b).
func TestWhatTheServerWrote(t *testing.T) {
	for _, c := range []struct {
		kind string
		want bool
	}{{"", false}, {"system", true}, {"huddle", true}} {
		if got := (&DgraphPost{Kind: c.kind}).ServerWritten(); got != c.want {
			t.Errorf("a post of kind %q: %v", c.kind, got)
		}
		if got := (&DgraphChat{Kind: c.kind}).ServerWritten(); got != c.want {
			t.Errorf("a message of kind %q: %v", c.kind, got)
		}
	}
	var p *DgraphPost
	var c *DgraphChat
	if p.ServerWritten() || c.ServerWritten() {
		t.Error("no message is the server's")
	}
}
