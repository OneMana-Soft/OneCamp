package business

import (
	"errors"
	"strings"
	"testing"
)

// A data source's destination is judged by the one address rule
// (helpers/safefetch) plus its own allowlist: what is neither public nor this
// server's own network is refused whatever is allowlisted; this server's own
// network needs an allowlist entry; a public address needs none unless an
// allowlist is set, and then it must be on it.
func TestADataSourceDestinationFollowsTheOneRule(t *testing.T) {
	refusedAlways := map[string]string{
		"169.254.169.254":    "link-local",
		"fe80::1":            "link-local",
		"fd00:ec2::254":      "metadata",
		"100.100.100.200":    "metadata",
		"192.0.0.192":        "special-purpose",
		"224.0.0.1":          "multicast",
		"0.0.0.0":            "unspecified",
		"255.255.255.255":    "broadcast",
		"198.51.100.7":       "special-purpose",
		"::ffff:169.254.1.1": "link-local",
	}
	internal := []string{"10.0.0.5", "127.0.0.1", "192.168.1.10", "fd12::1", "100.101.102.103"}

	t.Setenv(hostAllowlistEnv, "")
	for host, word := range refusedAlways {
		err := checkHostDestination(host)
		var de *destinationError
		if !errors.As(err, &de) || !strings.Contains(err.Error(), word) {
			t.Errorf("%s: %v; want a refusal naming %q", host, err, word)
		}
	}
	for _, host := range internal {
		if err := checkHostDestination(host); err == nil || !strings.Contains(err.Error(), hostAllowlistEnv) {
			t.Errorf("%s with no allowlist: %v; want the allowlist hint", host, err)
		}
	}
	if err := checkHostDestination("93.184.216.34"); err != nil {
		t.Errorf("a public address with no allowlist: %v", err)
	}

	// An operator's allowlist opens their own network, and only what it names.
	t.Setenv(hostAllowlistEnv, "10.0.0.0/8, 127.0.0.1, 100.64.0.0/10, fd12::/16")
	for _, host := range internal[:2] {
		if err := checkHostDestination(host); err != nil {
			t.Errorf("%s allowlisted: %v", host, err)
		}
	}
	if err := checkHostDestination("100.101.102.103"); err != nil {
		t.Errorf("a Tailscale address allowlisted: %v", err)
	}
	if err := checkHostDestination("192.168.1.10"); err == nil {
		t.Error("an internal address the allowlist doesn't name was allowed")
	}
	if err := checkHostDestination("93.184.216.34"); err == nil {
		t.Error("a public address off a configured allowlist was allowed")
	}
	// Nothing the allowlist says reaches a metadata address.
	t.Setenv(hostAllowlistEnv, "169.254.0.0/16, 100.64.0.0/10")
	for _, host := range []string{"169.254.169.254", "100.100.100.200"} {
		if err := checkHostDestination(host); err == nil {
			t.Errorf("%s was allowed by an allowlist", host)
		}
	}
}
