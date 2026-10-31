package business

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/akashc777/OneCamp/business/Import/providers/jira"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// A Jira site the admin types is checked by the outbound rules before the
// connection is tried or saved: an address on this server's own network is
// refused in words, and is never sent the token.
func TestConnectingJiraAtAnAddressInsideIsRefused(t *testing.T) {
	var reached atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	for _, site := range []string{srv.URL, "http://10.0.0.7", "http://169.254.169.254", "http://[::1]:8080"} {
		md, _ := json.Marshal(map[string]string{"site_url": site, "email": "admin@example.com"})
		tok := &importModels.Token{Provider: importModels.ProviderJira, AccessToken: "api-token", Metadata: md}
		_, problem := SaveConnection(context.Background(), jira.New(), tok, site)
		if problem == nil || problem.Code != "site_refused" || problem.Status != http.StatusBadRequest {
			t.Errorf("%s: %+v", site, problem)
		}
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the site was reached %d times", n)
	}
}

// A name that only resolves inside, refused as the connection is made, is
// said the same way, not as a site that can't be reached.
func TestASiteRefusedAtConnectionIsSaidAsRefused(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://jira.corp.example/rest/api/3/project/search", Err: safefetch.ErrResolvesInside}
	p := DescribeProviderError(importModels.ProviderJira, "https://jira.corp.example", err)
	if p.Code != "site_refused" || !strings.Contains(p.Msg, "https://jira.corp.example is on a private or reserved network") {
		t.Errorf("%+v", p)
	}
	p = DescribeProviderError(importModels.ProviderJira, "https://acme.example", &url.Error{Op: "Get", URL: "https://acme.example/x", Err: safefetch.ErrOffOrigin})
	if p.Code != "site_refused" || !strings.Contains(p.Msg, "another address") {
		t.Errorf("%+v", p)
	}
}
