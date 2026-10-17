package helpers

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The cases the web runs too (lib/utils/secretDestination.test.ts reads its
// copy, testdata/secret_destination.json in the web repo, which must stay
// byte-identical to this one), so the form asks for the secret exactly when
// the server would refuse to keep it.
type secretDestinationCase struct {
	Why     string   `json:"why"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	FromHow []string `json:"fromHow"`
	ToHow   []string `json:"toHow"`
	Moves   bool     `json:"moves"`
}

func TestAStoredSecretStaysWithItsDestination(t *testing.T) {
	raw, err := os.ReadFile("testdata/secret_destination.json")
	if err != nil {
		t.Fatal(err)
	}
	var shared struct {
		About string                  `json:"about"`
		Cases []secretDestinationCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &shared); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shared.About, "byte-identical") {
		t.Error("the case list no longer says it is shared with the web, byte for byte")
	}
	cases := shared.Cases
	if len(cases) < 10 {
		t.Fatalf("only %d cases", len(cases))
	}
	for _, c := range cases {
		from := SecretDestination{URL: c.From, How: c.FromHow}
		to := SecretDestination{URL: c.To, How: c.ToHow}
		if got := SecretMoves(from, to); got != c.Moves {
			t.Errorf("%s: SecretMoves = %v, want %v", c.Why, got, c.Moves)
		}
	}
}

func TestOnlyAKeptSecretIsHeldToItsAddress(t *testing.T) {
	from := SecretDestination{URL: "https://api.example.com/mcp", How: []string{"bearer", ""}}
	moved := SecretDestination{URL: "https://evil.example.net/mcp", How: []string{"bearer", ""}}
	err := CheckKeptSecret(true, false, from, moved, "token")
	if !IsSecretReentry(err) || err.Error() != "Enter the token again for the new address." {
		t.Fatalf("a kept token moving to a new host: %v", err)
	}
	if err := CheckKeptSecret(true, true, from, moved, "token"); err != nil {
		t.Errorf("a new token for the new host is the fix, got %v", err)
	}
	if err := CheckKeptSecret(false, false, from, moved, "token"); err != nil {
		t.Errorf("nothing stored, nothing to carry: %v", err)
	}
	if err := CheckKeptSecret(true, false, from, from, "token"); err != nil {
		t.Errorf("an unchanged address keeps its token: %v", err)
	}
	if IsSecretReentry(errors.New("Enter the token again for the new address.")) {
		t.Error("only the typed refusal counts")
	}
}

func TestTheReentryRefusalIsA400WithItsCode(t *testing.T) {
	w := httptest.NewRecorder()
	WriteSecretAwareBadRequest(w, &SecretReentryError{Noun: "secret"})
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"secret_reentry"`) ||
		!strings.Contains(w.Body.String(), "Enter the secret again for the new address.") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	WriteSecretAwareBadRequest(w, errors.New("name is required"))
	if strings.Contains(w.Body.String(), "secret_reentry") {
		t.Errorf("another 400 must not carry the code: %s", w.Body.String())
	}
}
