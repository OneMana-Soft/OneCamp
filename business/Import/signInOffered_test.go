package business

import (
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	_ "github.com/akashc777/OneCamp/business/Import/providers"
)

// Jira takes an API token as well as Atlassian's sign-in, so its
// sign-in is offered only where the server has the app; the web shows the
// sign-in alone when it is offered, which would have hidden the token form
// everywhere. Basecamp has no tokens, so its sign-in is always offered, with
// how to set it up.
func TestAnOptionalSignInIsOfferedOnlyWhereItWorks(t *testing.T) {
	t.Setenv("JIRA_OAUTH_CLIENT_ID", "")
	t.Setenv("JIRA_OAUTH_CLIENT_SECRET", "")
	if c := OfferedSignIn(importProvider.Get("jira")); c != nil {
		t.Errorf("jira with no app: sign-in offered (%+v), want the token form only", *c)
	}
	t.Setenv("JIRA_OAUTH_CLIENT_ID", "id")
	t.Setenv("JIRA_OAUTH_CLIENT_SECRET", "secret")
	if c := OfferedSignIn(importProvider.Get("jira")); c == nil || !c.Configured() {
		t.Errorf("jira with its app: %v, want sign-in offered", c)
	}
	t.Setenv("BASECAMP_OAUTH_CLIENT_ID", "")
	if c := OfferedSignIn(importProvider.Get("basecamp")); c == nil || c.Configured() {
		t.Errorf("basecamp with no app: %v, want sign-in offered, not configured", c)
	}
}
