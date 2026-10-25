package jira

import (
	"os"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers/safefetch"
)

// The tests' fake Jira sites listen on loopback, which the production client
// refuses. They run through a client that also allows this machine's own
// network, with the production client's every other rule (redirects stay on
// their origin); useProductionClient puts the real one back for a test.
func TestMain(m *testing.M) {
	siteClient = safefetch.NewClient(safefetch.Policy{AnyPort: true, PrivateNetwork: true}, 60*time.Second)
	os.Exit(m.Run())
}

// useProductionClient sends this test's calls through SiteAPIClient, as a
// running server does.
func useProductionClient(t *testing.T) {
	prev := siteClient
	siteClient = importProvider.SiteAPIClient
	t.Cleanup(func() { siteClient = prev })
}
