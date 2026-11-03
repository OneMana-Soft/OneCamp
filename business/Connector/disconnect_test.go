package business

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestDisconnectAnswersUnknownForAProviderNotRegistered: the connectors page
// disconnects only what it lists. Any other name, such as "github" (the
// person's GitHub identity, which githubSync reads to credit their work, not
// the connectors page's "github_connector"), is unknown, and nothing is
// written or revoked for it (V-281).
func TestDisconnectAnswersUnknownForAProviderNotRegistered(t *testing.T) {
	for _, provider := range []string{"github", "", "nope", "GMAIL", ProviderGitHub + " "} {
		if _, ok := providerRegistry[provider]; ok {
			t.Fatalf("%q is registered; the test needs a name that isn't", provider)
		}
		err := Disconnect(context.Background(), uuid.New(), provider)
		if !errors.Is(err, ErrUnknownConnector) {
			t.Errorf("Disconnect(%q) = %v; want ErrUnknownConnector", provider, err)
		}
	}
}
