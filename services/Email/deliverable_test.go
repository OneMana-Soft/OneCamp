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
