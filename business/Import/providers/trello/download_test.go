package trello

import (
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// A Trello file is fetched with the importing admin's key and token in an
// OAuth Authorization header: Trello stopped taking them in the query of a
// /download/ URL on 25 Jan 2021, so every file failed (I1). They go to Trello
// and to nothing else; with a substring check each of the "someone else's"
// URLs below got both.
func TestTrelloSendsItsKeyOnlyToTrello(t *testing.T) {
	tok := &trelloToken{APIKey: "key1", Token: "tok1"}
	want := `OAuth oauth_consumer_key="key1", oauth_token="tok1"`
	for _, c := range []struct {
		url  string
		ours bool
	}{
		{"https://trello.com/1/cards/c1/attachments/a1/download/plan.pdf", true},
		{"https://api.trello.com/1/x", true},

		// Someone else's.
		{"https://evil.example/trello.com", false},
		{"https://evil.example/?u=trello.com", false},
		{"https://trello.com.evil.example/x", false},
		{"https://nottrello.com/x", false},
		{"http://trello.com/1/x", false},
	} {
		got := trelloFile(importProvider.SourceAttachment{URL: c.url}, tok)
		if got.URL != c.url {
			t.Errorf("%s: the address changed to %s", c.url, got.URL)
		}
		if auth := got.Headers["Authorization"]; (auth == want) != c.ours || (!c.ours && auth != "") {
			t.Errorf("%s: Authorization %q", c.url, auth)
		}
	}
	if got := trelloFile(importProvider.SourceAttachment{URL: "https://trello.com/1/x"}, nil); got.Headers["Authorization"] != "" {
		t.Errorf("with no token: %v", got.Headers)
	}
}
