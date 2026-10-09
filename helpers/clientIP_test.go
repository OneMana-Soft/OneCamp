package helpers

import (
	"net/http"
	"testing"
)

func TestTheClientAddressIsTheOneTheProxySaw(t *testing.T) {
	req := func(remote string, header ...string) *http.Request {
		r, _ := http.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for i := 0; i+1 < len(header); i += 2 {
			r.Header.Add(header[i], header[i+1])
		}
		return r
	}
	const traefik = "172.18.0.2:5000"
	for _, c := range []struct {
		name string
		r    *http.Request
		want string
	}{
		{"behind Traefik, which replaces the header", req(traefik, "X-Forwarded-For", "203.0.113.9"), "203.0.113.9"},
		{"a client writing its own entries can't choose", req(traefik, "X-Forwarded-For", "6.6.6.6, 7.7.7.7, 203.0.113.9"), "203.0.113.9"},
		{"the header on two lines", req(traefik, "X-Forwarded-For", "6.6.6.6", "X-Forwarded-For", "203.0.113.9"), "203.0.113.9"},
		{"through Cloudflare", req(traefik, "X-Forwarded-For", "104.16.5.5", "CF-Connecting-IP", "198.51.100.1"), "198.51.100.1"},
		{"through Cloudflare over IPv6", req(traefik, "X-Forwarded-For", "2606:4700::1", "CF-Connecting-IP", "2001:db8::5"), "2001:db8::5"},
		{"a Cloudflare header not from Cloudflare", req(traefik, "X-Forwarded-For", "203.0.113.9", "CF-Connecting-IP", "1.2.3.4"), "203.0.113.9"},
		{"a nonsense Cloudflare header", req(traefik, "X-Forwarded-For", "104.16.5.5", "CF-Connecting-IP", "nobody"), "104.16.5.5"},
		{"only X-Real-IP", req(traefik, "X-Real-IP", "203.0.113.9"), "203.0.113.9"},
		{"a proxy entry that isn't an address", req(traefik, "X-Forwarded-For", "203.0.113.9, unknown"), "172.18.0.2"},
		{"no headers", req(traefik), "172.18.0.2"},
		{"exposed directly, headers are the sender's", req("198.51.100.7:5000", "X-Forwarded-For", "203.0.113.9"), "198.51.100.7"},
		{"an IPv6 peer, no headers", req("[2001:db8::1]:443"), "2001:db8::1"},
		{"a mapped IPv4 entry", req(traefik, "X-Forwarded-For", "::ffff:203.0.113.9"), "203.0.113.9"},
	} {
		if got := ClientIP(c.r); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	t.Setenv("TRUST_PROXY_HEADERS", "false")
	if got := ClientIP(req(traefik, "X-Forwarded-For", "203.0.113.9")); got != "172.18.0.2" {
		t.Errorf("headers turned off: %q", got)
	}
	t.Setenv("TRUST_PROXY_HEADERS", "true")
	if got := ClientIP(req("198.51.100.7:5000", "X-Forwarded-For", "203.0.113.9")); got != "203.0.113.9" {
		t.Errorf("headers believed from any peer: %q", got)
	}
}
