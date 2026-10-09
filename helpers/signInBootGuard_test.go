package helpers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A sign-in method that can't be set up at boot has to say why in the log.
// OIDC's error was thrown away (`_ = oauth.InitGenericOIDC()`), so an issuer
// the server couldn't reach, or a setting left out, left no trace: the button
// showed, and clicking it said only "OIDC is misconfigured on the server".
// Google's was thrown away the same way. Read from the startup path, since
// that is where each error is either logged or dropped.
func TestSignInSetupFailuresAreLogged(t *testing.T) {
	raw, err := os.ReadFile("../cmd/server/main.go")
	if err != nil {
		t.Fatalf("reading the startup path: %v", err)
	}
	// What each call's error is handled with: the body of its `if err := ...`.
	handled := func(call string) string {
		m := regexp.MustCompile(`(?s)if err := ` + regexp.QuoteMeta(call) + `; err != nil \{(.*?)\n\t\}`).FindSubmatch(raw)
		if m == nil {
			return ""
		}
		return string(m[1])
	}
	for _, call := range []string{"oauth.InitOAuth()", "oauth.InitGenericOIDC()", "oauth.InitLDAP()", "saml.InitSAML()"} {
		if !strings.Contains(handled(call), "helpers.LogErrorWithContext(") {
			t.Errorf("cmd/server/main.go doesn't log the error from %s", call)
		}
	}
	// The issuer is what an admin checks first: the address the server could
	// not reach, or one that isn't the provider's.
	if !strings.Contains(handled("oauth.InitGenericOIDC()"), `os.Getenv("OIDC_ISSUER_URL")`) {
		t.Error("the OIDC setup error is logged without the issuer it was for")
	}
}
