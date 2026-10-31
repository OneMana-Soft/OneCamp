package jira

import (
	"context"

	"encoding/json"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// A token from Atlassian's sign-in (3LO) works only through
// api.atlassian.com/ex/jira/{cloud id}; the site's own address answers it
// 401. Every call went to the site, so a signed-in Jira read nothing, and
// only API tokens worked. Recorded flow: the site refuses a bearer token,
// and the gateway serves the same API under the site's cloud id.
func TestASignedInJiraTokenReadsThroughTheGateway(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer site.Close()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/oauth/token/accessible-resources":
			_, _ = w.Write([]byte(`[{"id":"cloud-1","url":"` + site.URL + `","name":"acme","scopes":["read:jira-work"]},
				{"id":"cloud-2","url":"https://other.atlassian.net","name":"other","scopes":["read:jira-work"]}]`))
		case r.URL.Path == "/ex/jira/cloud-1/rest/api/3/project/search":
			_, _ = w.Write([]byte(`{"values":[{"id":"1","key":"ENG","name":"Engineering"}],"isLast":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer gateway.Close()
	prev := atlassianAPI
	atlassianAPI = gateway.URL
	defer func() { atlassianAPI = prev }()

	p := New()
	// The sign-in's last step finds the site the admin named among those
	// the token reaches, and keeps its cloud id beside the token.
	md, err := p.CompleteOAuth(context.Background(), "at-1", site.URL+"/")
	if err != nil {
		t.Fatalf("CompleteOAuth: %v", err)
	}
	var meta tokenMeta
	if err := json.Unmarshal(md, &meta); err != nil || meta.CloudID != "cloud-1" || meta.Bearer != "true" || meta.SiteURL != site.URL {
		t.Fatalf("metadata %s, want cloud-1, bearer and the site", md)
	}
	// Two sites and none named: the admin is asked which, never guessed.
	if _, err := p.CompleteOAuth(context.Background(), "at-1", ""); err == nil || !strings.Contains(err.Error(), "several") {
		t.Fatalf("two sites, none named: %v, want a question", err)
	}
	// The other site, named as an admin may type it: without its scheme, in
	// capitals, or with its default port.
	for _, named := range []string{"OTHER.atlassian.net", "https://other.atlassian.net:443/"} {
		md, err := p.CompleteOAuth(context.Background(), "at-1", named)
		if err != nil || json.Unmarshal(md, &meta) != nil || meta.CloudID != "cloud-2" {
			t.Fatalf("named %q: %s %v, want cloud-2", named, md, err)
		}
	}

	items, err := p.Discover(context.Background(), "owner", &importModels.Token{AccessToken: "at-1", Metadata: md})
	if err != nil {
		t.Fatalf("Discover with a signed-in token: %v", err)
	}
	if len(items) != 1 || items[0].ID != "ENG" || items[0].URL != site.URL+"/browse/ENG" {
		t.Fatalf("items %+v, want ENG, browsed at the site", items)
	}
}

// A signed-in token fetches a file the issue names on its own site
// through the gateway, which takes the token; a file on another host is left
// alone and gets no credential (TestJiraSendsItsCredentialOnlyToItsSite).
func TestASignedInJiraFileComesThroughTheGateway(t *testing.T) {
	gw := "https://api.atlassian.com/ex/jira/cloud-1"
	got := jiraDownload(importProvider.SourceAttachment{SourceID: "10001", URL: "https://acme.atlassian.net/rest/api/3/attachment/content/10001"}, gw, "Bearer at-1", "https://acme.atlassian.net")
	if got.URL != gw+"/rest/api/3/attachment/content/10001" {
		t.Fatalf("fetched %s, want the gateway's copy", got.URL)
	}
	if got.Headers["Authorization"] != "Bearer at-1" {
		t.Fatalf("Authorization %q, want the token for the gateway", got.Headers["Authorization"])
	}
	// The site's address with its default port is the same site.
	ported := jiraDownload(importProvider.SourceAttachment{SourceID: "10003", URL: "https://acme.atlassian.net:443/rest/api/3/attachment/content/10003"}, gw, "Bearer at-1", "https://acme.atlassian.net")
	if ported.URL != gw+"/rest/api/3/attachment/content/10003" {
		t.Fatalf("fetched %s, want the gateway's copy", ported.URL)
	}
	other := jiraDownload(importProvider.SourceAttachment{SourceID: "10002", URL: "https://evil.example/x"}, gw, "Bearer at-1", "https://acme.atlassian.net")
	if other.URL != "https://evil.example/x" || other.Headers["Authorization"] != "" {
		t.Fatalf("another host: %s with %q, want it untouched and no credential", other.URL, other.Headers["Authorization"])
	}
}
