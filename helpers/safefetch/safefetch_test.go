package safefetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNet stands in for DNS and the network: names resolve to the addresses
// given (in turn, when there are several answers in sequence), and a dial to
// a vetted address goes to a test server on loopback instead. It records what
// was looked up and what was dialled, so a test can say the connection went
// to the address that was checked.
type fakeNet struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr // name -> answers, one per lookup (the last repeats)
	routes  map[string]string         // "ip:port" -> real listener address
	lookups map[string]int
	dialled []string
}

func useFakeNet(t *testing.T) *fakeNet {
	t.Helper()
	f := &fakeNet{answers: map[string][][]netip.Addr{}, routes: map[string]string{}, lookups: map[string]int{}}
	oldLookup, oldDial := lookup, dial
	lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		seq, ok := f.answers[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		i := f.lookups[host]
		f.lookups[host]++
		if i >= len(seq) {
			i = len(seq) - 1
		}
		return seq[i], nil
	}
	dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		f.mu.Lock()
		f.dialled = append(f.dialled, address)
		real, ok := f.routes[address]
		f.mu.Unlock()
		if !ok {
			return nil, errors.New("fakeNet: nothing listens at " + address)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, real)
	}
	t.Cleanup(func() { lookup, dial = oldLookup, oldDial })
	return f
}

func (f *fakeNet) name(host string, answers ...[]netip.Addr) { f.answers[host] = answers }

// serve puts a test server behind a public address on port 80.
func (f *fakeNet) serve(t *testing.T, public string, h http.Handler) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f.routes[public+":80"] = srv.Listener.Addr().String()
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

const pub = "93.184.216.34" // a public address (example.com's, once)

func TestPublicAddrRefusesEveryAddressInsideTheNetwork(t *testing.T) {
	inside := []string{
		"127.0.0.1", "127.255.255.254", "0.0.0.0", "0.1.2.3", "10.0.0.1", "172.16.0.1", "172.31.255.255",
		"192.168.1.1", "169.254.169.254", "169.254.0.1", "100.64.0.1", "100.100.100.200", "192.0.0.192",
		"192.0.2.1", "198.18.0.1", "198.51.100.7", "203.0.113.9", "224.0.0.1", "239.255.255.250",
		"240.0.0.1", "255.255.255.255",
		"::1", "::", "::ffff:127.0.0.1", "::ffff:7f00:1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
		"::127.0.0.1", "64:ff9b::7f00:1", "64:ff9b::a00:1", "64:ff9b::a9fe:a9fe", "64:ff9b:1::1", "2002:7f00:1::1", "2001::1", "2001:db8::1",
		"fe80::1", "fc00::1", "fd00:ec2::254", "fec0::1", "ff02::1", "100::1", "3fff::1",
	}
	for _, s := range inside {
		if PublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s is inside the network, but PublicAddr let it through", s)
		}
	}
	// 64:ff9b::808:808 is 8.8.8.8 through a NAT64 gateway (an IPv6-only box behind DNS64).
	outside := []string{pub, "8.8.8.8", "1.1.1.1", "::ffff:8.8.8.8", "64:ff9b::808:808", "2606:4700:4700::1111", "2001:4860:4860::8888"}
	for _, s := range outside {
		if !PublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s is public, but PublicAddr refused it", s)
		}
	}
	if PublicAddr(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("an address with a zone names an interface here; it must be refused")
	}
}

func TestNumbersAreReadAsInetAtonReadsThem(t *testing.T) {
	cases := map[string]string{
		"2130706433":          "127.0.0.1",
		"0177.0.0.1":          "127.0.0.1",
		"0x7f.0.0.1":          "127.0.0.1",
		"0x7f000001":          "127.0.0.1",
		"0X7F.1":              "127.0.0.1",
		"127.1":               "127.0.0.1",
		"127.0.1":             "127.0.0.1",
		"017700000001":        "127.0.0.1",
		"0":                   "0.0.0.0",
		"0x.0x.0x.0x":         "0.0.0.0",
		"3232235777":          "192.168.1.1",
		"0xa9.0xfe.0xa9.0xfe": "169.254.169.254",
		"93.184.216.34":       pub,
	}
	for in, want := range cases {
		got, ok := legacyIPv4(in)
		if !ok || got.String() != want {
			t.Errorf("legacyIPv4(%q) = %v, %v; want %s", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "1.2.3.4.5", "256.1.1.1", "1.2.3.256", "08.1.1.1", "0x", "a.b.c.d", "4294967296", "1.16777216"} {
		if in == "0x" {
			continue // inet_aton reads a bare 0x as 0
		}
		if a, ok := legacyIPv4(in); ok {
			t.Errorf("legacyIPv4(%q) = %v; it isn't an address", in, a)
		}
	}
}

func TestCheckURLRefusesWhatCanReachInside(t *testing.T) {
	cases := map[string]error{
		"http://127.0.0.1/":                        ErrBlocked,
		"http://[::1]/":                            ErrBlocked,
		"http://169.254.169.254/latest/meta-data/": ErrBlocked,
		"http://0.0.0.0/":                          ErrBlocked,
		"http://[::]/":                             ErrBlocked,
		"http://10.0.0.1/":                         ErrBlocked,
		"http://172.16.0.1/":                       ErrBlocked,
		"http://192.168.1.1/":                      ErrBlocked,
		"http://100.64.0.1/":                       ErrBlocked,
		"http://198.18.0.1/":                       ErrBlocked,
		"http://2130706433/":                       ErrBlocked,
		"http://0177.0.0.1/":                       ErrBlocked,
		"http://0x7f.0.0.1/":                       ErrBlocked,
		"http://0x7f000001/":                       ErrBlocked,
		"http://127.1/":                            ErrBlocked,
		"http://127.0.0.1./":                       ErrBlocked,
		"http://[::ffff:127.0.0.1]/":               ErrBlocked,
		"http://[::ffff:7f00:1]/":                  ErrBlocked,
		"http://[64:ff9b::7f00:1]/":                ErrBlocked,
		"http://[2002:7f00:1::1]/":                 ErrBlocked,
		"http://[fe80::1]/":                        ErrBlocked,
		"http://[fc00::1]/":                        ErrBlocked,
		"http://[fd00:ec2::254]/":                  ErrBlocked,
		"http://[fe80::1%25eth0]/":                 ErrHost,
		"http://localhost/":                        ErrHost,
		"http://LOCALHOST./":                       ErrHost,
		"http://a.localhost/":                      ErrHost,
		"http://metadata.google.internal/":         ErrHost,
		"http://printer.local/":                    ErrHost,
		"http://router.home.arpa/":                 ErrHost,
		"http://intranet/":                         ErrHost,
		"http://999999999999/":                     ErrHost,
		// What IDNA maps: fullwidth digits, the ideographic full stop and
		// fullwidth letters are an address or a refused name once mapped.
		"http://\uff11\uff12\uff17.\uff10.\uff10.\uff11/":                ErrBlocked,
		"http://127\u30020\u30020\u30021/":                               ErrBlocked,
		"http://\uff4c\uff4f\uff43\uff41\uff4c\uff48\uff4f\uff53\uff54/": ErrHost,
		"http://a..example.com/":                                         ErrHost,
		"http://[64:ff9b::a9fe:a9fe]/":                                   ErrBlocked,
		"http://[::ffff:0:7f00:1]/":                                      ErrBlocked,
		"http://user:pass@example.com/":                                  ErrUserInfo,
		"http://example.com@127.0.0.1/":                                  ErrUserInfo,
		"ftp://example.com/":                                             ErrScheme,
		"file:///etc/passwd":                                             ErrScheme,
		"gopher://example.com/":                                          ErrScheme,
		"javascript:alert(1)":                                            ErrScheme,
		"http://example.com:22/":                                         ErrPort,
		"http://example.com:6379/":                                       ErrPort,
		"https://example.com:0/":                                         ErrPort,
	}
	for in, want := range cases {
		if _, err := CheckURL(in); !errors.Is(err, want) {
			t.Errorf("CheckURL(%q) = %v; want %v", in, err, want)
		}
	}
	for _, ok := range []string{"https://example.com/a?b=c#d", "http://example.com:8080/", "https://xn--bcher-kva.example/", "https://bücher.example/", "http://93.184.216.34/", "https://[2606:4700:4700::1111]/", "http://[64:ff9b::5db8:d822]/"} {
		if _, err := CheckURL(ok); err != nil {
			t.Errorf("CheckURL(%q) = %v; it is a public link", ok, err)
		}
	}
}

func TestAFetchConnectsToTheAddressItChecked(t *testing.T) {
	f := useFakeNet(t)
	f.name("public.test", addrs(pub))
	f.serve(t, pub, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "public.test" {
			t.Errorf("the request named %q; it must still name the site", r.Host)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<title>Hi</title>"))
	}))
	res, err := Fetch(context.Background(), "http://public.test/page", Options{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(res.Body) != "<title>Hi</title>" || res.ContentType != "text/html" || res.Status != 200 {
		t.Errorf("got %d %q %q", res.Status, res.ContentType, res.Body)
	}
	if strings.Join(f.dialled, ",") != pub+":80" || f.lookups["public.test"] != 1 {
		t.Errorf("dialled %v after %d lookups; want the one vetted address after one lookup", f.dialled, f.lookups["public.test"])
	}
}

// DNS rebinding: the name answers a public address when it is checked, then a
// private one. The connection goes to the address that was checked, so the
// second answer is never used for that fetch; the next fetch looks again and
// is refused.
func TestDNSRebindingCannotSwapTheAddressAfterTheCheck(t *testing.T) {
	f := useFakeNet(t)
	f.name("rebind.test", addrs(pub), addrs("10.0.0.1"))
	f.serve(t, pub, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))

	if _, err := Fetch(context.Background(), "http://rebind.test/", Options{}); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if f.lookups["rebind.test"] != 1 || len(f.dialled) != 1 || f.dialled[0] != pub+":80" {
		t.Fatalf("lookups %d, dialled %v: one lookup, and the vetted address dialled", f.lookups["rebind.test"], f.dialled)
	}
	_, err := Fetch(context.Background(), "http://rebind.test/", Options{})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("second fetch after the name moved inside: %v; want ErrBlocked", err)
	}
	if len(f.dialled) != 1 {
		t.Errorf("dialled %v: nothing may be dialled once the answer is private", f.dialled)
	}
}

func TestANameAnsweringAnyPrivateAddressIsRefusedWhole(t *testing.T) {
	f := useFakeNet(t)
	f.name("mixed.test", addrs(pub, "127.0.0.1"))
	f.name("mapped.test", addrs("::ffff:169.254.169.254"))
	for _, u := range []string{"http://mixed.test/", "http://mapped.test/"} {
		_, err := Fetch(context.Background(), u, Options{})
		if !errors.Is(err, ErrBlocked) || !Refused(err) {
			t.Errorf("Fetch(%s) = %v; want ErrBlocked", u, err)
		}
		// A name says so apart from an address, so a preview can say
		// "couldn't reach it" without telling which names the DNS knows.
		if !errors.Is(err, ErrResolvesInside) {
			t.Errorf("Fetch(%s) = %v; want ErrResolvesInside", u, err)
		}
	}
	if _, err := Fetch(context.Background(), "http://127.0.0.1/", Options{}); errors.Is(err, ErrResolvesInside) {
		t.Errorf("an address is no name: %v", err)
	}
	if len(f.dialled) != 0 {
		t.Errorf("dialled %v; nothing should have been", f.dialled)
	}
}

func TestARedirectInsideTheNetworkIsRefused(t *testing.T) {
	f := useFakeNet(t)
	f.name("public.test", addrs(pub))
	f.name("private.test", addrs("10.0.0.5"))
	to := map[string]string{
		"/meta":     "http://169.254.169.254/latest/meta-data/",
		"/loopback": "http://127.0.0.1/admin",
		"/decimal":  "http://2130706433/",
		"/v6":       "http://[::1]/",
		"/name":     "http://private.test/",
		"/file":     "file:///etc/passwd",
		"/port":     "http://public.test:6379/",
		"/mapped":   "http://[::ffff:127.0.0.1]/",
		"/nat64":    "http://[64:ff9b::a9fe:a9fe]/latest/meta-data/",
		"/wide":     "http://\uff11\uff12\uff17.\uff10.\uff10.\uff11/",
	}
	f.serve(t, pub, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, to[r.URL.Path], http.StatusFound)
	}))
	want := map[string]error{"/meta": ErrBlocked, "/loopback": ErrBlocked, "/decimal": ErrBlocked, "/v6": ErrBlocked, "/name": ErrResolvesInside, "/file": ErrScheme, "/port": ErrPort, "/mapped": ErrBlocked, "/nat64": ErrBlocked, "/wide": ErrBlocked}
	for path, wantErr := range want {
		_, err := Fetch(context.Background(), "http://public.test"+path, Options{})
		if !errors.Is(err, wantErr) {
			t.Errorf("redirect %s → %s: %v; want %v", path, to[path], err, wantErr)
		}
	}
	for _, d := range f.dialled {
		if d != pub+":80" {
			t.Errorf("dialled %s; only the public address may be", d)
		}
	}
}

func TestRedirectsAreCapped(t *testing.T) {
	f := useFakeNet(t)
	f.name("loop.test", addrs(pub))
	f.serve(t, pub, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	if _, err := Fetch(context.Background(), "http://loop.test/", Options{MaxRedirects: 3}); !errors.Is(err, ErrRedirects) {
		t.Fatalf("a redirect loop: %v; want ErrRedirects", err)
	}
	if len(f.dialled) != 4 {
		t.Errorf("dialled %d times; want the first request and 3 redirects", len(f.dialled))
	}
}

func TestTheBodyIsCappedAndSaysSo(t *testing.T) {
	f := useFakeNet(t)
	f.name("big.test", addrs(pub))
	f.serve(t, pub, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 3<<20)))
	}))
	res, err := Fetch(context.Background(), "http://big.test/", Options{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !res.Truncated || len(res.Body) != DefaultMaxBytes {
		t.Errorf("read %d bytes, truncated %v; want %d and true", len(res.Body), res.Truncated, DefaultMaxBytes)
	}
}

func TestASlowSiteRunsOutOfTime(t *testing.T) {
	f := useFakeNet(t)
	f.name("slow.test", addrs(pub))
	f.serve(t, pub, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	start := time.Now()
	_, err := Fetch(context.Background(), "http://slow.test/", Options{Timeout: 150 * time.Millisecond})
	if err == nil || Refused(err) {
		t.Fatalf("a slow site: %v; want a timeout, not a refusal", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "timeout") {
		t.Errorf("a slow site: %v; want the deadline", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("took %v; the timeout is 150 ms", time.Since(start))
	}
}

// A proxy in the environment would resolve the name itself, after the check.
func TestAProxyInTheEnvironmentIsNeverUsed(t *testing.T) {
	f := useFakeNet(t)
	f.name("public.test", addrs(pub))
	f.serve(t, pub, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("direct")) }))
	proxied := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied = true
		_, _ = w.Write([]byte("proxy"))
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	res, err := Fetch(context.Background(), "http://public.test/", Options{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(res.Body) != "direct" || proxied {
		t.Fatalf("got %q, proxied %v; the proxy must never be used", res.Body, proxied)
	}
}

func TestNumbersNeverReachTheResolver(t *testing.T) {
	f := useFakeNet(t)
	for _, u := range []string{"http://2130706433/", "http://0x7f.1/", "http://017700000001/", "http://999999999999/"} {
		if _, err := Fetch(context.Background(), u, Options{}); err == nil {
			t.Errorf("Fetch(%s) succeeded", u)
		}
	}
	if len(f.lookups) != 0 || len(f.dialled) != 0 {
		t.Errorf("looked up %v and dialled %v; a numeric host is never resolved", f.lookups, f.dialled)
	}
}
