package helpers

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// The addresses a webhook, an import's download or an app's handler must never
// reach, and some it must. Every range in blockedPrefixes has a case here, and
// so does each way of writing an address that a plain prefix check would miss.
func TestBlockedAddresses(t *testing.T) {
	for _, c := range []struct {
		addr    string
		blocked bool
	}{
		// This host. On Linux a connection to 0.0.0.0 or :: is a connection to it.
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"127.0.0.1", true},
		{"127.255.255.254", true},
		{"::", true},
		{"::1", true},
		// Private networks.
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"fc00::1", true},
		{"fd12:3456::1", true},
		// Carrier-grade NAT: internal on many clouds, and Alibaba's metadata.
		{"100.64.0.1", true},
		{"100.100.100.200", true},
		{"100.127.255.255", true},
		// Link-local, and the metadata service in it.
		{"169.254.169.254", true},
		{"169.254.0.1", true},
		{"fe80::1", true},
		{"fe80::1%eth0", true}, // a prefix never contains a zoned address
		{"fd00:ec2::254", true},
		// Not the public internet.
		{"192.0.0.1", true},
		{"192.0.0.170", true},
		{"192.0.2.1", true},
		{"192.88.99.1", true},
		{"198.18.0.1", true},
		{"198.19.255.255", true},
		{"198.51.100.1", true},
		{"203.0.113.1", true},
		{"240.0.0.1", true},
		{"255.255.255.255", true},
		{"100::1", true},
		{"2001:db8::1", true},
		{"fec0::1", true},
		// Multicast.
		{"224.0.0.1", true},
		{"239.255.255.250", true},
		{"ff02::1", true},
		{"ff05::1:3", true},
		// An IPv4 address written as IPv6 is that IPv4 address.
		{"::ffff:127.0.0.1", true},
		{"::ffff:10.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"::ffff:0.0.0.0", true},
		{"::127.0.0.1", true}, // IPv4-compatible, deprecated
		// NAT64 and 6to4 send their packets on to the IPv4 address inside.
		{"64:ff9b::127.0.0.1", true},
		{"64:ff9b::a9fe:a9fe", true}, // 169.254.169.254
		{"64:ff9b::10.0.0.1", true},
		{"64:ff9b:1::1", true},      // NAT64 for local use, any address
		{"2002:7f00:1::1", true},    // 127.0.0.1
		{"2002:a00:1::", true},      // 10.0.0.1
		{"2002:a9fe:a9fe::1", true}, // 169.254.169.254

		// The public internet, including the edges of the ranges above.
		{"1.1.1.1", false},
		{"8.8.8.8", false},
		{"100.63.255.255", false},
		{"100.128.0.0", false},
		{"172.15.255.255", false},
		{"172.32.0.0", false},
		{"192.169.0.1", false},
		{"198.17.255.255", false},
		{"198.20.0.0", false},
		{"223.255.255.255", false},
		{"2606:4700:4700::1111", false},
		{"2001:4860:4860::8888", false},
		{"::ffff:8.8.8.8", false},
		{"64:ff9b::808:808", false}, // NAT64 of 8.8.8.8: how an IPv6-only host reaches it
		{"2002:808:808::1", false},  // 6to4 of 8.8.8.8
	} {
		a, err := netip.ParseAddr(c.addr)
		if err != nil {
			t.Fatalf("bad test address %q: %v", c.addr, err)
		}
		if got := isBlockedAddr(a); got != c.blocked {
			t.Errorf("isBlockedAddr(%s) = %v, want %v", c.addr, got, c.blocked)
		}
		// The resolver's form: a 16-byte net.IP, IPv4 ones mapped.
		if c.addr != "fe80::1%eth0" {
			if got := isBlockedIP(net.ParseIP(c.addr)); got != c.blocked {
				t.Errorf("isBlockedIP(%s) = %v, want %v", c.addr, got, c.blocked)
			}
		}
	}
	if !isBlockedAddr(netip.Addr{}) || !isBlockedIP(nil) {
		t.Error("something that isn't an address must be blocked")
	}
}

// The dial check sees the address actually being connected to, so it refuses
// it however the URL named it, a name that resolves to this host included.
func TestSafeClientRefusesBlockedAddressesWhenItConnects(t *testing.T) {
	var reached atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer srv.Close()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	client := SSRFSafeClient(true)
	for _, u := range []string{
		srv.URL,
		"http://localhost:" + port + "/",
		"http://0.0.0.0:" + port + "/",
		"http://[::ffff:127.0.0.1]:" + port + "/",
	} {
		resp, err := client.Get(u)
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s: connected", u)
			continue
		}
		if !strings.Contains(err.Error(), "SSRF dial blocked") {
			t.Errorf("%s: refused, but not by the dial check: %v", u, err)
		}
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the server on this host was reached %d times", n)
	}
}

func TestDialCheck(t *testing.T) {
	for _, c := range []struct {
		address string
		refused bool
	}{
		{"8.8.8.8:443", false},
		{"[2606:4700:4700::1111]:443", false},
		{"127.0.0.1:80", true},
		{"[fe80::1%eth0]:80", true},
		{"[::ffff:169.254.169.254]:80", true},
		{"not-an-address:80", true},
		{"8.8.8.8", true}, // no port: not something the dialler hands over
	} {
		if err := refuseBlockedAddr("tcp", c.address, nil); (err != nil) != c.refused {
			t.Errorf("refuseBlockedAddr(%s) = %v, want refused=%v", c.address, err, c.refused)
		}
	}
}

func TestValidateOutboundURL(t *testing.T) {
	for _, c := range []struct {
		url string
		ok  bool
	}{
		{"https://8.8.8.8/hook", true},
		{"https://[2606:4700:4700::1111]/hook", true},
		{"http://8.8.8.8/hook", false}, // https only
		{"https://127.0.0.1/", false},
		{"https://localhost/", false}, // a name that resolves to this host
		{"https://0.0.0.0/", false},
		{"https://100.100.100.200/latest/meta-data/", false},
		{"https://[::]/", false},
		{"https://[::ffff:169.254.169.254]/", false},
		{"https://[fe80::1%25eth0]/", false},
		{"https://[64:ff9b::a9fe:a9fe]/", false},
	} {
		_, err := ValidateOutboundURL(c.url, false)
		if (err == nil) != c.ok {
			t.Errorf("ValidateOutboundURL(%s) = %v, want ok=%v", c.url, err, c.ok)
		}
	}

	// A redirect is checked the same way before it's followed.
	client := SSRFSafeClient(false)
	req := httptest.NewRequest(http.MethodGet, "https://[::ffff:127.0.0.1]/admin", nil)
	if err := client.CheckRedirect(req, []*http.Request{req}); err == nil {
		t.Error("a redirect to this host was followed")
	}
}
