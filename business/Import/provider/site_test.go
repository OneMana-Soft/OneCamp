package provider

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers/safefetch"
)

// listing puts the hosts listed in env in force for one test, as a server
// that starts with IMPORT_PRIVATE_HOSTS=env does.
func listing(t *testing.T, env string) {
	t.Helper()
	old := sitePolicy
	t.Cleanup(func() { useSitePolicy(old) })
	t.Setenv(privateHostsEnv, env)
	if _, err := LoadPrivateHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestASitesPrivateAddressIsRefusedUnlessListed(t *testing.T) {
	if err := CheckSite("https://10.20.30.40"); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("nothing listed: a private site is %v", err)
	}
	listing(t, "10.20.0.0/16, 192.168.7.9")
	for _, ok := range []string{"https://10.20.30.40", "https://10.20.30.40:8443", "http://192.168.7.9:8080", "https://jira.example.com"} {
		if err := CheckSite(ok); err != nil {
			t.Errorf("listed: %s is %v", ok, err)
		}
	}
	for _, refused := range []string{"https://10.21.0.1", "https://192.168.7.10", "https://127.0.0.1", "https://[::1]", "http://169.254.169.254"} {
		if err := CheckSite(refused); !safefetch.Refused(err) {
			t.Errorf("%s isn't listed: %v", refused, err)
		}
	}
}

// Loopback, link-local and the metadata services stay refused when listed.
func TestLoopbackAndMetadataStayRefusedWhenListed(t *testing.T) {
	listing(t, "127.0.0.1, 127.0.0.0/8, ::1, 169.254.169.254, 169.254.0.0/16")
	for _, refused := range []string{"https://127.0.0.1", "https://127.0.0.2:8443", "https://[::1]", "http://169.254.169.254", "http://169.254.10.10"} {
		if err := CheckSite(refused); !safefetch.Refused(err) {
			t.Errorf("listed %s: %v", refused, err)
		}
	}
	// What the dialler asks of each address it resolved.
	for _, a := range []string{"127.0.0.1", "::1", "169.254.169.254"} {
		addr := netip.MustParseAddr(a)
		if sitePolicy.Allows(addr) && sitePolicy.Also("x", addr) == nil {
			t.Errorf("the dialler lets listed %s through", a)
		}
	}
}

// A listed name lets through the addresses it had at start-up, and only for
// that name.
func TestAListedNameLetsThroughOnlyItsOwnAddresses(t *testing.T) {
	h := privateHosts{names: map[string][]netip.Addr{"jira.corp.example": {netip.MustParseAddr("10.9.9.9")}}}
	if err := h.allows("jira.corp.example.", netip.MustParseAddr("10.9.9.9")); err != nil {
		t.Errorf("its own address: %v", err)
	}
	if err := h.allows("jira.corp.example", netip.MustParseAddr("10.9.9.8")); !errors.Is(err, safefetch.ErrBlocked) {
		t.Errorf("an address it didn't have: %v", err)
	}
	if err := h.allows("wiki.corp.example", netip.MustParseAddr("10.9.9.9")); !errors.Is(err, safefetch.ErrBlocked) {
		t.Errorf("another name at its address: %v", err)
	}
}

func TestAListThatCantBeReadIsRefusedWhole(t *testing.T) {
	for _, bad := range []string{"10.0.0.0/33", "jira corp", "https://jira.corp.example", "10.0.0.1, user@host", "jira.corp.example:8443"} {
		if _, _, err := parsePrivateHosts(bad); err == nil {
			t.Errorf("%q was read", bad)
		}
	}
	nets, names, err := parsePrivateHosts(" 10.20.0.0/16 , Jira.Corp.Example. ,, fd00:1::/64 ")
	if err != nil || len(nets) != 2 || len(names) != 1 || names[0] != "jira.corp.example" {
		t.Fatalf("%v %v %v", nets, names, err)
	}
}

// privateAddr is an address on one of this machine's interfaces in a private
// range (a LAN or a container bridge), or the test is skipped.
func privateAddr(t *testing.T) netip.Addr {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() && p.Addr().IsPrivate() {
			return p.Addr()
		}
	}
	t.Skip("no private IPv4 address on this machine")
	return netip.Addr{}
}

// The client holds every connection to the list, and a redirect off the
// listed site's origin is still not followed.
func TestTheSiteClientConnectsOnlyWhereListed(t *testing.T) {
	addr := privateAddr(t)
	serve := func(h http.Handler) string {
		l, err := net.Listen("tcp", netip.AddrPortFrom(addr, 0).String())
		if err != nil {
			t.Skip(err)
		}
		srv := httptest.NewUnstartedServer(h)
		srv.Listener.Close()
		srv.Listener = l
		srv.Start()
		t.Cleanup(srv.Close)
		return srv.URL
	}
	var elsewhere int
	other := serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere++ }))
	site := serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/away" {
			http.Redirect(w, r, other+"/", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	get := func(u string) error {
		resp, err := SiteAPIClient.Get(u)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(site + "/"); !safefetch.Refused(err) {
		t.Fatalf("nothing listed: %v", err)
	}
	listing(t, addr.String())
	if err := get(site + "/"); err != nil {
		t.Fatalf("listed: %v", err)
	}
	if err := get(site + "/away"); !errors.Is(err, safefetch.ErrOffOrigin) {
		t.Errorf("a redirect to another origin on the listed host: %v", err)
	}
	if elsewhere != 0 {
		t.Errorf("the redirect was followed %d times", elsewhere)
	}
}

// A well-formed name that doesn't resolve as the server starts is a warning
// and lets nothing through; the rest of the list still applies.
func TestANameThatDoesntResolveAtStartIsLeftOut(t *testing.T) {
	old := sitePolicy
	t.Cleanup(func() { useSitePolicy(old) })
	t.Setenv(privateHostsEnv, "jira.nowhere.invalid, 10.20.0.0/16")
	warnings, err := LoadPrivateHosts(context.Background())
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "jira.nowhere.invalid") {
		t.Fatalf("%v %v", warnings, err)
	}
	if err := CheckSite("https://10.20.30.40"); err != nil {
		t.Errorf("the listed range: %v", err)
	}
	if err := sitePolicy.Also("jira.nowhere.invalid", netip.MustParseAddr("10.30.0.1")); !errors.Is(err, safefetch.ErrBlocked) {
		t.Errorf("the name that didn't resolve: %v", err)
	}
	t.Setenv(privateHostsEnv, "10.20.0.0/33")
	if _, err := LoadPrivateHosts(context.Background()); err == nil || !strings.Contains(err.Error(), "10.20.0.0/33") {
		t.Errorf("a list that can't be read: %v", err)
	}
}

// AllowLocalSitesForTest widens the site rules, so it is reached only from
// tests: a _test.go file, or a file built only with the integration tag (a
// file under any other tag, such as linux, ships).
func TestOnlyTestsAllowLocalSites(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "node_modules" || n == "vendor" || (strings.HasPrefix(n, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(src, []byte("AllowLocalSitesForTest(")) && !bytes.HasPrefix(src, []byte("//go:build integration\n")) {
			t.Errorf("%s reaches AllowLocalSitesForTest outside the integration build", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
