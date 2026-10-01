package ai

import (
	"net"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"169.254.169.254", // AWS/GCP/Azure IMDS
		"169.254.0.1",     // link-local
		"fe80::1",         // IPv6 link-local
		"0.0.0.0",         // unspecified
		"::",              // IPv6 unspecified
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if !isBlockedIP(ip) {
			t.Errorf("expected %s to be blocked", s)
		}
	}

	// Allowed: public AND private (self-hosted model servers live on
	// private/cluster networks, so those must NOT be blocked).
	allowed := []string{
		"8.8.8.8",      // public
		"104.18.0.1",   // public
		"10.0.0.5",     // private (k8s pod / LAN)
		"172.16.3.4",   // private
		"192.168.1.10", // private LAN (Ollama box)
		"127.0.0.1",    // loopback (local Ollama) — allowed by design
		"fd12:3456::1", // IPv6 ULA (private)
	}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if isBlockedIP(ip) {
			t.Errorf("did NOT expect %s to be blocked (breaks self-hosting)", s)
		}
	}
}

func TestIsBlockedIP_StrictPolicy(t *testing.T) {
	t.Setenv("AI_BLOCK_PRIVATE_IPS", "true")

	// Under the strict policy, private + loopback are blocked.
	strictBlocked := []string{
		"10.0.0.5",
		"172.16.3.4",
		"192.168.1.10",
		"127.0.0.1",
		"fd12:3456::1", // IPv6 ULA
		"169.254.169.254",
	}
	for _, s := range strictBlocked {
		ip := net.ParseIP(s)
		if !isBlockedIP(ip) {
			t.Errorf("strict policy: expected %s to be blocked", s)
		}
	}

	// Public addresses still allowed.
	for _, s := range []string{"8.8.8.8", "1.1.1.1"} {
		ip := net.ParseIP(s)
		if isBlockedIP(ip) {
			t.Errorf("strict policy: public %s must remain allowed", s)
		}
	}
}
