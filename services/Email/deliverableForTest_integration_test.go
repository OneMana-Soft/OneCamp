//go:build integration

package email

import (
	"context"
	"net"
	"testing"
)

// LetReservedNamesThroughForTest lets reserved names through for the test that
// asks, with no DNS lookup; a real domain is still checked, and once that test
// ends a reserved name is refused again.
func TestLetReservedNamesThroughForTest(t *testing.T) {
	withDNS(t, map[string][]*net.MX{"onemana.dev": {{Host: "mx.example.net.", Pref: 10}}}, nil, nil)
	looked := map[string]int{}
	checked := lookupMX
	lookupMX = func(ctx context.Context, name string) ([]*net.MX, error) {
		looked[name]++
		return checked(ctx, name)
	}
	ctx := context.Background()
	t.Run("opted in", func(t *testing.T) {
		LetReservedNamesThroughForTest(t)
		for addr, want := range map[string]bool{
			"alex@example.com":     true,
			"lee@mail.example.org": true,
			"qa@anything.test":     true,
			"hi@onemana.dev":       true,  // MX
			"x@gone.onemana.dev":   false, // a real name with no mail is still refused
		} {
			if got := Deliverable(ctx, addr); got != want {
				t.Errorf("Deliverable(%q) = %v, want %v", addr, got, want)
			}
		}
		if n := looked["example.com"] + looked["mail.example.org"] + looked["anything.test"]; n != 0 {
			t.Errorf("%d MX lookups for reserved names, want none", n)
		}
		if looked["onemana.dev"] != 1 || looked["gone.onemana.dev"] != 1 {
			t.Errorf("real names looked up %v, want each once", looked)
		}
	})
	for _, addr := range []string{"alex@example.com", "qa@anything.test"} {
		if Deliverable(ctx, addr) {
			t.Errorf("after the test that opted in ended, Deliverable(%q) = true", addr)
		}
	}
}
