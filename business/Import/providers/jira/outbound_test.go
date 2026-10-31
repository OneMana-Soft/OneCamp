package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers/safefetch"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// apiTokenAt is a Jira API-token connection to site, as the connect form
// saves it.
func apiTokenAt(site string) *importModels.Token {
	md, _ := json.Marshal(tokenMeta{SiteURL: site, Email: "admin@example.com"})
	return &importModels.Token{Provider: providerName, AccessToken: "api-token", Metadata: md}
}

// countingSite answers every request with an empty project list and counts
// the requests that reached it, and the ones that carried a credential.
func countingSite(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var withCredential atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			withCredential.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"isLast":true,"values":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &withCredential
}

// The site an admin types for a Jira API token is sent the token, so the
// server reaches it under the outbound rules: loopback, the private ranges and
// link-local (the clouds' metadata service) are refused before anything is
// sent.
func TestAJiraSiteOnTheServersOwnNetworkIsRefused(t *testing.T) {
	useProductionClient(t)
	local, credentials := countingSite(t)
	for _, site := range []string{
		local.URL,
		"http://10.20.30.40",
		"https://192.168.1.10:8443",
		"http://169.254.169.254",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := New().Discover(ctx, "u", apiTokenAt(site))
		cancel()
		if !safefetch.Refused(err) {
			t.Errorf("%s: want the outbound rules' refusal, got %v", site, err)
		}
	}
	if n := credentials.Load(); n != 0 {
		t.Errorf("a site on this server's network was sent the credential %d times", n)
	}
}

// A site that answers with a redirect to another address is not followed
// there: the credential is bound to the address the admin gave.
func TestAJiraSiteRedirectingElsewhereIsNotFollowed(t *testing.T) {
	inside, credentials := countingSite(t)
	for _, target := range []string{inside.URL, "http://169.254.169.254"} {
		front := httptest.NewServer(http.RedirectHandler(target+"/rest/api/3/project/search", http.StatusFound))
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := New().Discover(ctx, "u", apiTokenAt(front.URL))
		cancel()
		front.Close()
		if !safefetch.Refused(err) {
			t.Errorf("redirect to %s: want the outbound rules' refusal, got %v", target, err)
		}
	}
	if n := credentials.Load(); n != 0 {
		t.Errorf("a redirect took the credential to another address %d times", n)
	}
}
