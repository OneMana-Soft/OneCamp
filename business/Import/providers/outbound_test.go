package providers_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// outbound is where each provider's requests go, and who chose the address.
// A provider's own hosts are written in its source; an address an answer names
// is read only on those hosts, or without the credential; and the one host an
// admin types, a Jira site, is reached only through SiteAPIClient, under the
// outbound rules. A new provider is added here with its hosts.
var outbound = []struct {
	dir, path string
	hosts     []string
}{
	{"asana", "fixed hosts; next_page.path is appended to the fixed base", []string{"app.asana.com"}},
	{"basecamp", "pinned: an account's href, a Link next and a dock url are read only on apiOrigins", []string{"launchpad.37signals.com", "3.basecampapi.com"}},
	{"clickup", "fixed hosts", []string{"api.clickup.com"}},
	{"jira", "typed: the admin's site, through SiteAPIClient", nil},
	{"linear", "fixed hosts", []string{"api.linear.app"}},
	{"monday", "fixed hosts", []string{"api.monday.com"}},
	{"notion", "fixed hosts", []string{"api.notion.com"}},
	{"todoist", "fixed hosts", []string{"api.todoist.com"}},
	{"trello", "fixed hosts", []string{"api.trello.com"}},
}

// source is a provider's non-test Go source, joined.
func source(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	return b.String()
}

func TestEveryProvidersRequestsGoWhereTheyShould(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, o := range outbound {
		listed[o.dir] = true
	}
	for _, e := range entries {
		if e.IsDir() && !listed[e.Name()] {
			t.Errorf("provider %s isn't in outbound: say where its requests go", e.Name())
		}
	}
	// An address read from the token's metadata is one the admin typed.
	typedAddress := regexp.MustCompile(`json:"(site_url|[a-z_]*base_url|[a-z_]*api_url)"`)
	for _, o := range outbound {
		src := source(t, o.dir)
		for _, h := range o.hosts {
			if !strings.Contains(src, "https://"+h) {
				t.Errorf("%s (%s): https://%s isn't in its source", o.dir, o.path, h)
			}
		}
		typed := typedAddress.MatchString(src)
		switch o.dir {
		case "jira":
			if !typed || !strings.Contains(src, "importProvider.SiteAPIClient") || strings.Contains(src, "SharedAPIClient") {
				t.Errorf("jira (%s): its calls must go through SiteAPIClient alone", o.path)
			}
		case "basecamp":
			if !strings.Contains(src, "if !onAPI(url) {") {
				t.Errorf("basecamp (%s): get must check onAPI before it sends the token", o.path)
			}
		}
		if typed && o.dir != "jira" {
			t.Errorf("%s (%s) reads an address from the token: route it through SiteAPIClient and list it as typed", o.dir, o.path)
		}
	}
}
