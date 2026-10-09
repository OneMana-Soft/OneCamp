package provider

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A provider's credential goes to a URL only when the URL's host is the
// provider's. The check was a substring search on the whole URL, so each of
// the "someone else's" cases below received the importing admin's token.
func TestCredentialAllowed(t *testing.T) {
	const linear = "uploads.linear.app"
	for _, c := range []struct {
		url  string
		want bool
	}{
		{"https://uploads.linear.app/x.png", true},
		{"https://UPLOADS.Linear.App/x.png", true},
		{"https://uploads.linear.app./x.png", true}, // the same host, fully qualified
		{"https://uploads.linear.app:443/x.png", true},
		{"https://eu.uploads.linear.app/x.png", true}, // under the provider's host

		// Someone else's.
		{"https://uploads.linear.app.evil.example/x", false},
		{"https://evil.example/?u=uploads.linear.app", false},
		{"https://evil.example/uploads.linear.app/x", false},
		{"https://evil.example/#uploads.linear.app", false},
		{"https://uploads.linear.app@evil.example/x", false}, // the provider's name as a user name
		{"https://eviluploads.linear.app/x", false},          // a different name ending the same way
		{`https://evil.example\@uploads.linear.app/x`, false},
		{"https://[::1]/uploads.linear.app", false},
		// Not https: a credential never goes in the clear.
		{"http://uploads.linear.app/x.png", false},
		{"//uploads.linear.app/x.png", false},
		{"uploads.linear.app/x.png", false},
		{"", false},
	} {
		if got := CredentialAllowed(c.url, linear); got != c.want {
			t.Errorf("CredentialAllowed(%q) = %v, want %v", c.url, got, c.want)
		}
	}
	if CredentialAllowed("https://uploads.linear.app/x") {
		t.Error("a URL was allowed a credential with no provider host to match")
	}
}

func TestWithAuthorization(t *testing.T) {
	stored := map[string]string{"authorization": "Bearer from-an-earlier-run", "Accept": "*/*"}

	theirs := WithAuthorization(SourceAttachment{URL: "https://evil.example/?u=uploads.linear.app", Headers: stored},
		"Bearer secret", "uploads.linear.app")
	for k := range theirs.Headers {
		if strings.EqualFold(k, "Authorization") {
			t.Errorf("a URL that isn't the provider's carries %s: %q", k, theirs.Headers[k])
		}
	}
	if theirs.Headers["Accept"] != "*/*" {
		t.Error("the attachment's other headers were dropped")
	}

	ours := WithAuthorization(SourceAttachment{URL: "https://uploads.linear.app/x.png", Headers: stored},
		"Bearer secret", "uploads.linear.app")
	if ours.Headers["Authorization"] != "Bearer secret" {
		t.Errorf("a Linear upload went without the token: %v", ours.Headers)
	}
	if stored["authorization"] != "Bearer from-an-earlier-run" || len(stored) != 2 {
		t.Errorf("the caller's header map was changed: %v", stored)
	}
}

// Downloads go through the SSRF-safe client: an attachment URL naming this
// server, its networks or the metadata service is refused before anything is
// sent, and so is one that isn't https.
func TestDownloadsRefuseInternalAddresses(t *testing.T) {
	var reached atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte("internal"))
	})
	plain := httptest.NewServer(handler)
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(handler)
	defer tlsSrv.Close()

	for _, u := range []string{
		plain.URL + "/file",
		tlsSrv.URL + "/file",
		"https://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"https://[::ffff:10.0.0.1]/file",
		"https://100.100.100.200/latest/meta-data/",
		"http://8.8.8.8/file.pdf",
	} {
		var dest bytes.Buffer
		_, _, err := DefaultFetchAttachment(context.Background(), SourceAttachment{URL: u}, &dest)
		if err == nil {
			t.Errorf("%s was downloaded: %q", u, dest.String())
		} else if !strings.Contains(err.Error(), "attachment URL refused") {
			t.Errorf("%s: failed, but not because it was refused: %v", u, err)
		}
	}

	// The client checks again as it connects, so a name that resolves to this
	// machine by then (DNS rebinding) is refused too.
	_, port, _ := strings.Cut(strings.TrimPrefix(tlsSrv.URL, "https://"), ":")
	resp, err := downloadClient.Get("https://localhost:" + port + "/file")
	if err == nil {
		_ = resp.Body.Close()
		t.Error("the download client connected to this machine by name")
	} else if !strings.Contains(err.Error(), "SSRF dial blocked") {
		t.Errorf("the download client failed, but not at its dial check: %v", err)
	}

	if n := reached.Load(); n != 0 {
		t.Errorf("a server on this machine was reached %d times", n)
	}
}

// A redirect may lead anywhere public, but the credential stays with the host
// it was checked for, and a redirect to an internal or plain-http address is
// refused outright.
func TestRedirectsKeepTheCredentialOnItsHost(t *testing.T) {
	hop := func(raw string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, raw, nil)
		req.Header.Set("Authorization", "Bearer secret")
		return req
	}
	first := hop("https://1.1.1.1/download")

	other := hop("https://8.8.8.8/signed-copy")
	if err := downloadClient.CheckRedirect(other, []*http.Request{first}); err != nil {
		t.Fatalf("a redirect to a public host was refused: %v", err)
	}
	if got := other.Header.Get("Authorization"); got != "" {
		t.Errorf("the credential followed a redirect to another host: %q", got)
	}

	same := hop("https://1.1.1.1/elsewhere")
	if err := downloadClient.CheckRedirect(same, []*http.Request{first}); err != nil {
		t.Fatalf("a redirect on the same host was refused: %v", err)
	}
	if same.Header.Get("Authorization") == "" {
		t.Error("the credential was dropped on a redirect to the host it was checked for")
	}

	for _, raw := range []string{"http://1.1.1.1/download", "https://169.254.169.254/", "https://127.0.0.1/"} {
		if err := downloadClient.CheckRedirect(hop(raw), []*http.Request{first}); err == nil {
			t.Errorf("a redirect to %s was followed", raw)
		}
	}
}
