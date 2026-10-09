package controllers

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	business "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	authService "github.com/akashc777/OneCamp/services/Auth"
)

// A refused Google or GitHub sign-in says why, as a code the sign-in page has
// words for. Every refusal arrived as "unauthorized", which the page reads as
// not invited, so "verify your address" and "the workspace is full" never
// reached the person.
func TestOAuthRefusalSaysWhy(t *testing.T) {
	internal := errors.New(`invalid google exchange code: oauth2: "invalid_grant" "Bad Request" client_secret=abc123`)
	for name, tc := range map[string]struct {
		err  error
		code string
	}{
		"an address the provider hasn't verified": {business.ErrUnverifiedEmail, "oauth_email_unverified"},
		"an address nobody invited":               {fmt.Errorf("admitting: %w", business.ErrNotInvited), "oauth_not_invited"},
		"an invitation past its expiry":           {fmt.Errorf("admitting: %w", business.ErrInvitationExpired), "invitation_expired"},
		"an address outside ASCII":                {business.ErrAddressNotASCII, "address_unsupported"},
		"a free workspace with no seat left":      {fmt.Errorf("creating user: %w", &helpers.SeatLimitError{Limit: 25}), "seat_limit"},
		"anything else":                           {internal, "oauth_failed"},
	} {
		code, message := oauthRefusal(tc.err)
		if code != tc.code {
			t.Errorf("%s: code %q, want %q", name, code, tc.code)
		}
		if message == "" || strings.Contains(message, "invalid_grant") || strings.Contains(message, "client_secret") {
			t.Errorf("%s: the URL would carry %q", name, message)
		}
	}
}

// A sign-in that comes back without a code is sent to the sign-in page on the
// web app's origin, not told it isn't invited: cancelled at Google ("access
// denied") says so, and anything else says to try again.
func TestOAuthCallbackWithoutACodeSaysTryAgain(t *testing.T) {
	t.Setenv("FRONTEND_DOMAIN", "app.example.com")
	t.Setenv("FE_HOST_DOMAIN", "")
	t.Setenv("COOKIE_SECURE", "")
	for query, want := range map[string]string{
		"error=access_denied&state=s": authService.SignInCancelled,
		"state=s":                     "oauth_failed",
	} {
		rec := httptest.NewRecorder()
		OAuthCallback(rec, httptest.NewRequest(http.MethodGet, "/oauth_callback/google?"+query, nil))
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusFound || !authService.IsRedirectAllowed(loc) {
			t.Fatalf("%s: %d to %q, want a redirect to the web app", query, rec.Code, loc)
		}
		u, _ := url.Parse(loc)
		if got := u.Query().Get("error"); got != want {
			t.Errorf("%s: redirected with error=%q, want %s", query, got, want)
		}
	}
}

// A Google or GitHub sign-in that fails after the person was admitted, when
// the session cannot be started, lands on the sign-in page saying to try
// again, as every other refusal does (authService.SignInErrorURL). It used to
// go to /app?error=login_failed, which the web app has no words for.
func TestNoGoogleOrGitHubFailureLandsWhereNothingSaysSo(t *testing.T) {
	src, err := os.ReadFile("userController.go")
	if err != nil {
		t.Fatal(err)
	}
	// The code it sent, quoted; a comment may still say what it replaced.
	if strings.Contains(string(src), `"login_failed"`) {
		t.Fatal("userController.go still sends login_failed, which the sign-in page never shows; refuse through authService.SignInErrorURL")
	}
}
