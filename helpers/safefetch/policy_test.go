package safefetch

import (
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

// An endpoint a member added for an MCP server or a remote agent is held to
// the same address rule as a link preview, on any port: public addresses only.
func TestAMembersEndpointIsRefusedInsideTheNetwork(t *testing.T) {
	p := EndpointPolicy(false)
	for _, in := range []string{
		"http://127.0.0.1:8080/mcp", "http://[::1]/", "http://10.0.0.5:3000/", "http://172.17.0.1/",
		"http://192.168.1.10/", "http://169.254.169.254/latest/meta-data/", "http://[fd00:ec2::254]/",
		"http://100.100.100.200/", "http://0.0.0.0:8000/",
	} {
		if _, err := CheckEndpoint(in, p); !errors.Is(err, ErrBlocked) {
			t.Errorf("CheckEndpoint(%q) = %v; want ErrBlocked", in, err)
		}
	}
	for _, in := range []string{"http://go-service:8000/", "http://localhost:6379/", "http://metadata.google.internal/", "http://postgres/"} {
		if _, err := CheckEndpoint(in, p); !errors.Is(err, ErrHost) {
			t.Errorf("CheckEndpoint(%q) = %v; want ErrHost", in, err)
		}
	}
	if _, err := CheckEndpoint("https://api.githubcopilot.com/mcp/", p); err != nil {
		t.Errorf("a public MCP server: %v", err)
	}
	if _, err := CheckEndpoint("https://agent.example.com:9999/a2a", p); err != nil {
		t.Errorf("any port on a public host: %v", err)
	}
	if _, err := CheckEndpoint("https://agent.example.com:9999/a2a", Policy{}); !errors.Is(err, ErrPort) {
		t.Errorf("the default keeps to the web's ports: %v", err)
	}
	if _, err := CheckEndpoint("https://user:pw@agent.example.com/", p); !errors.Is(err, ErrUserInfo) {
		t.Errorf("a credential in the address: %v", err)
	}
}

// An admin who runs MCP servers beside OneCamp may add them on this server's
// own network; the metadata and link-local addresses stay refused even then.
func TestAnAdminsEndpointMayBeInsideButNeverMetadata(t *testing.T) {
	p := EndpointPolicy(true)
	for _, in := range []string{"http://10.0.0.5:3000/", "http://127.0.0.1:8080/", "http://github-mcp:8082/mcp", "http://[fd12::1]/", "http://100.101.102.103:8080/mcp"} {
		if _, err := CheckEndpoint(in, p); err != nil {
			t.Errorf("CheckEndpoint(%q) = %v; an admin added it", in, err)
		}
	}
	// A Tailscale address is a member's no more than any other inside one.
	if _, err := CheckEndpoint("http://100.101.102.103:8080/mcp", EndpointPolicy(false)); !errors.Is(err, ErrBlocked) {
		t.Errorf("a member's Tailscale address: %v; want ErrBlocked", err)
	}
	for _, in := range []string{"http://169.254.169.254/", "http://[fd00:ec2::254]/", "http://100.100.100.200/", "http://192.0.0.192/", "http://[fe80::1]/", "http://metadata/"} {
		if _, err := CheckEndpoint(in, p); err == nil {
			t.Errorf("CheckEndpoint(%q) passed; a metadata address never may", in)
		}
	}
	f := useFakeNet(t)
	f.name("metadata.google.internal", addrs("169.254.169.254"))
	_, err := NewClient(p, 0).Get("http://metadata.google.internal/computeMetadata/v1/")
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("a name resolving to the metadata address: %v; want ErrBlocked", err)
	}
	if len(f.dialled) != 0 {
		t.Errorf("dialled %v", f.dialled)
	}
}

// The client carries a credential, so it never follows a redirect off the
// origin it was given: the secret stays with its address.
func TestACredentialNeverFollowsARedirectElsewhere(t *testing.T) {
	f := useFakeNet(t)
	const other = "93.184.216.35"
	f.name("mcp.test", addrs(pub))
	f.name("elsewhere.test", addrs(other))
	reached := 0
	f.serve(t, other, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Errorf("the credential reached another host: %v", r.Header)
		}
	}))
	f.serve(t, pub, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/same" {
			http.Redirect(w, r, "/ok", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path == "/ok" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		http.Redirect(w, r, "http://elsewhere.test/steal", http.StatusTemporaryRedirect)
	}))
	c := NewClient(Policy{AnyPort: true}, 0)
	for _, h := range []string{"Authorization", "X-Api-Key"} {
		req, _ := http.NewRequest(http.MethodPost, "http://mcp.test/rpc", strings.NewReader("{}"))
		req.Header.Set(h, "Bearer s3cret")
		if _, err := c.Do(req); !errors.Is(err, ErrOffOrigin) || !Refused(err) {
			t.Errorf("%s: a redirect to another host: %v; want ErrOffOrigin", h, err)
		}
	}
	if reached != 0 {
		t.Errorf("the other host was reached %d times", reached)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://mcp.test/same", nil)
	res, err := c.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("a redirect on the same origin is followed: %v", err)
	}
	res.Body.Close()
}

// A caller's own rule sees every address before it is dialled.
func TestACallersRuleCanRefuseAnAddress(t *testing.T) {
	f := useFakeNet(t)
	f.name("agent.test", addrs(pub))
	refuse := errors.New("not on the local network")
	p := Policy{AnyPort: true, Also: func(host string, a netip.Addr) error {
		if host != "agent.test" || a != netip.MustParseAddr(pub) {
			t.Errorf("Also saw %s %s", host, a)
		}
		return refuse
	}}
	if _, err := NewClient(p, 0).Get("http://agent.test/"); !errors.Is(err, refuse) {
		t.Errorf("got %v; want the caller's refusal", err)
	}
	if len(f.dialled) != 0 {
		t.Errorf("dialled %v", f.dialled)
	}
}
