package email

import (
	"context"
	"errors"
	"net"
	"testing"
)

func withDNS(t *testing.T, mx map[string][]*net.MX, hosts map[string]bool, flaky map[string]bool) {
	t.Helper()
	oldMX, oldHost := lookupMX, lookupHost
	t.Cleanup(func() {
		lookupMX, lookupHost = oldMX, oldHost
		domainChecks.Lock()
		domainChecks.m = map[string]domainCheck{}
		domainChecks.Unlock()
	})
	lookupMX = func(_ context.Context, name string) ([]*net.MX, error) {
		if flaky[name] {
			return nil, &net.DNSError{Err: "i/o timeout", Name: name, IsTimeout: true}
		}
		if r, ok := mx[name]; ok {
			return r, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	lookupHost = func(_ context.Context, name string) ([]string, error) {
		if hosts[name] {
			return []string{"192.0.2.1"}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
}

func TestDeliverable(t *testing.T) {
	withDNS(t,
		map[string][]*net.MX{"onemana.dev": {{Host: "mx.example.net.", Pref: 10}}, "nullmx.org": {{Host: ".", Pref: 0}}},
		map[string]bool{"a-only.io": true},
		map[string]bool{"slow-dns.com": true},
	)
	ctx := context.Background()
	for addr, want := range map[string]bool{
		"hi@onemana.dev":                  true,  // MX
		"Sam <sam@a-only.io>":             true,  // no MX, but an address: implicit MX
		"x@slow-dns.com":                  true,  // a DNS hiccup never loses mail
		"visitor@demo.onemana.dev":        false, // no such name
		"maya.chen@cast.demo.onemana.dev": false,
		"someone@nullmx.org":              false, // "I take no mail"
		"user@example.com":                false, // reserved
		"user@mail.example.org":           false,
		"dev@box.local":                   false,
		"qa@anything.test":                false,
		"not an address":                  false,
	} {
		if got := Deliverable(ctx, addr); got != want {
			t.Errorf("Deliverable(%q) = %v, want %v", addr, got, want)
		}
	}
}

// An address on a name reserved for examples is told without the DNS, by the
// same rule Deliverable refuses with: a real domain is not one, whatever its
// records say.
func TestOnReservedDomain(t *testing.T) {
	oldMX, oldHost := lookupMX, lookupHost
	t.Cleanup(func() { lookupMX, lookupHost = oldMX, oldHost })
	lookupMX = func(_ context.Context, name string) ([]*net.MX, error) {
		t.Errorf("asked the DNS for %s's MX", name)
		return nil, errors.New("no DNS here")
	}
	lookupHost = func(_ context.Context, name string) ([]string, error) {
		t.Errorf("asked the DNS for %s", name)
		return nil, errors.New("no DNS here")
	}
	for addr, want := range map[string]bool{
		"alex@example.com":                true,
		"Alex Kim <ALEX@Example.ORG>":     true,
		"lee@mail.example.net":            true,
		"qa@anything.test":                true,
		"x@nowhere.invalid":               true,
		"hello@northwind.example":         true,
		"dev@box.localhost":               true,
		"alex@outlook.com":                false,
		"lee.real@proton.me":              false,
		"maya.chen@cast.demo.onemana.dev": false,
		"alex@example.com.evil.io":        false,
		"alex@notexample.com":             false,
		"not an address":                  false,
		"":                                false,
	} {
		if got := OnReservedDomain(addr); got != want {
			t.Errorf("OnReservedDomain(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestDeliverableRemembersAnswers(t *testing.T) {
	calls := 0
	withDNS(t, nil, nil, nil)
	inner := lookupMX
	lookupMX = func(ctx context.Context, name string) ([]*net.MX, error) {
		calls++
		return inner(ctx, name)
	}
	for range 3 {
		Deliverable(context.Background(), "a@gone.example-not-real.com")
	}
	if calls != 1 {
		t.Fatalf("looked the domain up %d times, want once", calls)
	}
}

func TestUndeliverableStopsRetries(t *testing.T) {
	if !IsTerminal(ErrUndeliverable) || !errors.Is(error(ErrUndeliverable), ErrUndeliverable) {
		t.Fatal("an undeliverable address must not be retried")
	}
}

// The shipped build mails no name reserved for examples: only the integration
// build's LetReservedNamesThroughForTest sets reservedReachTestTransport, so
// Deliverable refuses each of them before any DNS lookup.
func TestReservedNamesAreRefusedWithoutTheIntegrationBuild(t *testing.T) {
	if reservedReachTestTransport.Load() {
		t.Fatal("reserved names reach the transport, and no test opted in")
	}
	looked := 0
	old := lookupMX
	t.Cleanup(func() { lookupMX = old })
	lookupMX = func(context.Context, string) ([]*net.MX, error) {
		looked++
		return []*net.MX{{Host: "mx.mail.test.", Pref: 1}}, nil
	}
	for _, addr := range []string{"alex@example.com", "lee@mail.example.org", "x@example.net", "qa@anything.test",
		"hello@northwind.example", "x@nowhere.invalid", "dev@box.localhost", "dev@box.local", "Alex <ALEX@EXAMPLE.COM>"} {
		if Deliverable(context.Background(), addr) {
			t.Errorf("Deliverable(%q) = true, want false", addr)
		}
	}
	if looked != 0 {
		t.Errorf("%d MX lookups for reserved names, want none", looked)
	}
}
