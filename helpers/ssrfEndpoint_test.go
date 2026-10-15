package helpers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An MCP server or remote agent a member added reaches public addresses only;
// one an admin added may also be on this server's own network; metadata and
// link-local addresses are refused for everyone.
func TestOnlyAnAdminsEndpointReachesThisServersNetwork(t *testing.T) {
	member, admin := EndpointPolicyFor(false), EndpointPolicyFor(true)
	for _, in := range []string{"http://127.0.0.1:8080/mcp", "http://[::1]/", "http://10.0.0.5:3000/", "http://172.17.0.1/", "http://192.168.1.10/", "http://100.101.102.103:8080/mcp"} {
		if err := CheckEndpoint(in, member); !errors.Is(err, ErrEndpointBlocked) {
			t.Errorf("a member's %s: %v; want ErrEndpointBlocked", in, err)
		}
		if err := CheckEndpoint(in, admin); err != nil {
			t.Errorf("an admin's %s: %v", in, err)
		}
	}
	for _, in := range []string{"http://169.254.169.254/latest/meta-data/", "http://[fd00:ec2::254]/", "http://100.100.100.200/", "http://[fe80::1]/", "http://0.0.0.0:8000/", "http://metadata/"} {
		for _, p := range []EndpointPolicy{member, admin} {
			if err := CheckEndpoint(in, p); !errors.Is(err, ErrEndpointBlocked) {
				t.Errorf("%s (admin %v): %v; a metadata or special address is refused for everyone", in, p.AddedByAdmin, err)
			}
		}
	}
	if err := CheckEndpoint("https://agent.example.com:9999/a2a", member); err != nil {
		t.Errorf("any port on a public host: %v", err)
	}
	if err := CheckEndpoint("https://user:pw@agent.example.com/", admin); !errors.Is(err, ErrEndpointUserInfo) {
		t.Errorf("a credential in the address: %v", err)
	}
	if err := CheckEndpoint("ftp://agent.example.com/", admin); !errors.Is(err, ErrEndpointScheme) {
		t.Errorf("another scheme: %v", err)
	}

	// The same rule as each connection is made: a member's client never
	// reaches a server on loopback; an admin's does.
	reached := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++ }))
	defer srv.Close()
	if _, err := EndpointClient(member, 0).Get(srv.URL); !errors.Is(err, ErrEndpointBlocked) {
		t.Errorf("a member's client on loopback: %v; want ErrEndpointBlocked", err)
	}
	if reached != 0 {
		t.Fatal("a member's client reached loopback")
	}
	res, err := EndpointClient(admin, 0).Get(srv.URL)
	if err != nil || reached != 1 {
		t.Fatalf("an admin's client on loopback: %v (reached %d)", err, reached)
	}
	res.Body.Close()
	if !strings.Contains(EndpointRefusal(ErrEndpointBlocked, false), "Only an admin can add") {
		t.Errorf("a member is told who can: %q", EndpointRefusal(ErrEndpointBlocked, false))
	}
}

// A client carrying a credential never follows a redirect to another origin,
// so the credential never reaches it, whatever header it rides in.
func TestAnEndpointCredentialNeverFollowsARedirectElsewhere(t *testing.T) {
	elsewhereHit := 0
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhereHit++ }))
	defer elsewhere.Close()
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/same" {
			http.Redirect(w, r, "/ok", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path == "/ok" {
			return
		}
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer home.Close()
	c := EndpointClient(EndpointPolicyFor(true), 0)
	for _, h := range []string{"Authorization", "X-Api-Key"} {
		req, _ := http.NewRequest(http.MethodPost, home.URL+"/rpc", strings.NewReader("{}"))
		req.Header.Set(h, "Bearer s3cret")
		if _, err := c.Do(req); !errors.Is(err, ErrOffOrigin) {
			t.Errorf("%s: a redirect to another origin: %v; want ErrOffOrigin", h, err)
		}
	}
	if elsewhereHit != 0 {
		t.Errorf("the other origin was reached %d times", elsewhereHit)
	}
	res, err := c.Get(home.URL + "/same")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("a redirect on the same origin is followed: %v", err)
	}
	res.Body.Close()
}
