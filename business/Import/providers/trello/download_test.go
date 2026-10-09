package trello

import "testing"

// The importing admin's Trello key and token are added to a Trello URL and to
// nothing else; with a substring check, each of the "someone else's" URLs
// below got both.
func TestTrelloSendsItsKeyOnlyToTrello(t *testing.T) {
	tok := &trelloToken{APIKey: "key1", Token: "tok1"}
	for _, c := range []struct {
		url  string
		want string
	}{
		{"https://trello.com/1/cards/c1/attachments/a1/download/plan.pdf",
			"https://trello.com/1/cards/c1/attachments/a1/download/plan.pdf?key=key1&token=tok1"},
		{"https://trello.com/1/cards/c1/attachments/a1/download/plan.pdf?v=2",
			"https://trello.com/1/cards/c1/attachments/a1/download/plan.pdf?v=2&key=key1&token=tok1"},
		{"https://api.trello.com/1/x", "https://api.trello.com/1/x?key=key1&token=tok1"},

		// Someone else's.
		{"https://evil.example/trello.com", "https://evil.example/trello.com"},
		{"https://evil.example/?u=trello.com", "https://evil.example/?u=trello.com"},
		{"https://trello.com.evil.example/x", "https://trello.com.evil.example/x"},
		{"https://nottrello.com/x", "https://nottrello.com/x"},
		{"http://trello.com/1/x", "http://trello.com/1/x"},
	} {
		if got := trelloDownloadURL(c.url, tok); got != c.want {
			t.Errorf("trelloDownloadURL(%s) = %s, want %s", c.url, got, c.want)
		}
	}
	if got := trelloDownloadURL("https://trello.com/1/x", nil); got != "https://trello.com/1/x" {
		t.Errorf("with no token: %s", got)
	}
}
