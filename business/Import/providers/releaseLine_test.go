package providers

import (
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// RELEASE-LINE ONLY (v2.75.0 / v1.60.0). The Basecamp importer signs in through
// Basecamp's Launchpad, and this release's web has no step for that (B-1's web,
// b9094d680, isn't in it), so an admin who picked Basecamp in Import would reach a
// dead end. The release lines leave it unregistered; beta registers it, and the
// release that carries B-1's web drops this test and registers it again.
func TestTheReleaseLineDoesNotOfferBasecamp(t *testing.T) {
	if importProvider.Get("basecamp") != nil {
		t.Fatal("the Basecamp importer is offered, but this release's web has no way to sign in to Basecamp")
	}
	for _, name := range []string{"asana", "clickup", "jira", "linear", "monday", "notion", "todoist", "trello"} {
		if importProvider.Get(name) == nil {
			t.Errorf("%s isn't offered: the release line left out more than Basecamp", name)
		}
	}
}
