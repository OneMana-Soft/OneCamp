package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// B-1: a provider with no personal tokens (Basecamp) is connected by
// signing in at the provider.
func TestSigningInAtTheProvider(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a1","refresh_token":"r1","expires_in":1209600}`))
	}))
	defer srv.Close()
	c := OAuthConnect{
		Config: OAuthConfig{ClientID: "id", ClientSecret: "secret", Endpoint: oauth2.Endpoint{AuthURL: "https://launchpad.test/authorization/new", TokenURL: srv.URL}},
	}
	if !c.Configured() || (OAuthConnect{}).Configured() {
		t.Fatal("configured only with a client id and secret")
	}
	u, err := url.Parse(c.AuthURL("st", "https://api.test/admin/import/oauth/callback"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("state") != "st" || q.Get("response_type") != "code" || q.Get("client_id") != "id" || q.Get("redirect_uri") != "https://api.test/admin/import/oauth/callback" {
		t.Fatalf("authorize url %s", u)
	}
	tok, err := c.Exchange(context.Background(), "code1", "https://api.test/admin/import/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "a1" || tok.RefreshToken != "r1" || time.Until(tok.Expiry) < 13*24*time.Hour {
		t.Fatalf("token %+v", tok)
	}
	if form.Get("code") != "code1" || form.Get("grant_type") != "authorization_code" {
		t.Fatalf("token request %v", form)
	}
}
