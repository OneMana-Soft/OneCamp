package email

import (
	"context"
	"errors"
	"net"
	"net/mail"
	"strings"
	"sync"
	"time"
)

// ErrUndeliverable is returned for a recipient whose domain cannot receive
// mail: one reserved for examples and tests, or one that does not exist at
// all. Sending would only bounce, and bounces cost twice: the day's allowance
// (a free Resend account is 100 a day, shared) and the sender's standing with
// mailbox providers, which a high bounce rate can lose for every message,
// receipts and password resets included. The public demo's people all have
// such addresses, and every booking and message there once sent one.
var ErrUndeliverable = &SendError{Message: "the recipient's email domain cannot receive mail", Terminal: true,
	ForPeople: "that address's domain can't receive email"}

// Names that never receive mail (RFC 2606, RFC 6761, and mDNS's .local).
var reservedTLDs = []string{".test", ".example", ".invalid", ".localhost", ".local"}
var reservedDomains = map[string]bool{"example.com": true, "example.net": true, "example.org": true, "localhost": true}

func reserved(domain string) bool {
	if reservedDomains[domain] {
		return true
	}
	for _, tld := range reservedTLDs {
		if strings.HasSuffix(domain, tld) {
			return true
		}
	}
	for d := range reservedDomains {
		if strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

// Swapped in tests.
var (
	lookupMX   = net.DefaultResolver.LookupMX
	lookupHost = net.DefaultResolver.LookupHost
)

const domainCheckTTL = time.Hour

var domainChecks = struct {
	sync.Mutex
	m map[string]domainCheck
}{m: map[string]domainCheck{}}

type domainCheck struct {
	ok bool
	at time.Time
}

func notFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// domainReceivesMail asks the DNS whether a domain can take mail: an MX
// record that is not the "null MX" (RFC 7505), or failing that any address
// for the name itself (RFC 5321's implicit MX). Only a definite "no such
// name" counts against it; a lookup that times out or fails otherwise counts
// for it, so a DNS hiccup never loses real mail.
func domainReceivesMail(ctx context.Context, domain string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	mx, err := lookupMX(ctx, domain)
	if err == nil {
		for _, m := range mx {
			if strings.TrimSuffix(m.Host, ".") != "" {
				return true
			}
		}
		return len(mx) == 0
	}
	if !notFound(err) {
		return true
	}
	if _, err := lookupHost(ctx, domain); err != nil && notFound(err) {
		return false
	}
	return true
}

// Deliverable reports whether mail to addr can arrive at all: its domain is
// not reserved and exists in the DNS. Answers are remembered for an hour.
func Deliverable(ctx context.Context, addr string) bool {
	parsed, err := mail.ParseAddress(addr)
	if err != nil {
		return false
	}
	at := strings.LastIndex(parsed.Address, "@")
	domain := strings.ToLower(strings.TrimSuffix(parsed.Address[at+1:], "."))
	if domain == "" || reserved(domain) {
		return false
	}
	now := time.Now()
	domainChecks.Lock()
	c, seen := domainChecks.m[domain]
	domainChecks.Unlock()
	if seen && now.Sub(c.at) < domainCheckTTL {
		return c.ok
	}
	ok := domainReceivesMail(ctx, domain)
	domainChecks.Lock()
	domainChecks.m[domain] = domainCheck{ok: ok, at: now}
	domainChecks.Unlock()
	return ok
}
