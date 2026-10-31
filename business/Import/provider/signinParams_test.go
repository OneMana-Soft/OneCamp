package provider

import (
	"net/url"
	"testing"
)

// Atlassian's authorize page needs audience=api.atlassian.com and
// prompt=consent, or it refuses the sign-in; the shared sign-in sends a
// provider's AuthParams.
func TestTheAuthorizePageCarriesTheProvidersParams(t *testing.T) {
	c := OAuthConnect{Config: OAuthConfig{ClientID: "id", ClientSecret: "s"}, AuthParams: map[string]string{"audience": "api.atlassian.com", "prompt": "consent"}}
	u, err := url.Parse(c.AuthURL("st", "https://api.example/cb"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("audience") != "api.atlassian.com" || q.Get("prompt") != "consent" || q.Get("state") != "st" {
		t.Fatalf("authorize URL %s, want audience, prompt and state", u)
	}
}
