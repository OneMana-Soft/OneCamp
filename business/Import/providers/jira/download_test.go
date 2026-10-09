package jira

import (
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// The Jira credential (an admin's email and API token, or an OAuth token)
// went with every attachment URL an issue named. It now goes only to the site
// it was connected for.
func TestJiraSendsItsCredentialOnlyToItsSite(t *testing.T) {
	const site = "https://acme.atlassian.net"
	for _, c := range []struct {
		name, url, tok string
		wantURL        string
		wantAuth       string
	}{
		{"the site's own content URL", site + "/rest/api/3/attachment/content/10001", "ZW1haWw6dG9r",
			site + "/rest/api/3/attachment/content/10001", "Basic ZW1haWw6dG9r"},
		{"no URL: derived from the site", "", "ZW1haWw6dG9r",
			site + "/rest/api/3/attachment/content/10001", "Basic ZW1haWw6dG9r"},
		{"an OAuth token is sent as Bearer, not Basic Bearer", site + "/rest/api/3/attachment/content/10001", "Bearer oauth-tok",
			site + "/rest/api/3/attachment/content/10001", "Bearer oauth-tok"},
		{"another host", "https://evil.example/rest/api/3/attachment/content/10001", "ZW1haWw6dG9r",
			"https://evil.example/rest/api/3/attachment/content/10001", ""},
		{"the site's name inside another host", "https://acme.atlassian.net.evil.example/x", "ZW1haWw6dG9r",
			"https://acme.atlassian.net.evil.example/x", ""},
		{"another Atlassian site", "https://other.atlassian.net/rest/api/3/attachment/content/1", "ZW1haWw6dG9r",
			"https://other.atlassian.net/rest/api/3/attachment/content/1", ""},
	} {
		got := jiraDownload(importProvider.SourceAttachment{SourceID: "10001", URL: c.url}, site, c.tok)
		if got.URL != c.wantURL {
			t.Errorf("%s: fetched %s, want %s", c.name, got.URL, c.wantURL)
		}
		if got.Headers["Authorization"] != c.wantAuth {
			t.Errorf("%s: Authorization %q, want %q", c.name, got.Headers["Authorization"], c.wantAuth)
		}
	}

	// A site that doesn't parse names no host, so nothing gets the credential.
	got := jiraDownload(importProvider.SourceAttachment{URL: site + "/x"}, "://bad", "ZW1haWw6dG9r")
	if got.Headers["Authorization"] != "" {
		t.Errorf("with an unreadable site the credential still went: %q", got.Headers["Authorization"])
	}
}
