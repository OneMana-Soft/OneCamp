package oauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// What the boot log says when OIDC can't be set up: which settings are
// missing, or which issuer couldn't be discovered. It used to say only
// "missing required OIDC environment variables", and nothing logged it.
func TestOIDCSetupSaysWhatIsWrong(t *testing.T) {
	t.Setenv("OIDC_ENABLED", "true")
	t.Setenv("OIDC_ISSUER_URL", "https://idp.example.com")
	t.Setenv("OIDC_CLIENT_ID", "")
	t.Setenv("OIDC_CLIENT_SECRET", "")
	t.Setenv("BACKEND_DOMAIN", "")
	err := InitGenericOIDC()
	if err == nil {
		t.Fatal("OIDC was set up without a client ID, a secret or a backend address")
	}
	for _, want := range []string{"OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "BACKEND_DOMAIN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q doesn't name the missing %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "OIDC_ISSUER_URL") {
		t.Errorf("%q names the issuer, which is set, as missing", err)
	}

	// An issuer that answers, but isn't an OIDC provider.
	notAProvider := httptest.NewServer(http.NotFoundHandler())
	defer notAProvider.Close()
	t.Setenv("OIDC_ISSUER_URL", notAProvider.URL)
	t.Setenv("OIDC_CLIENT_ID", "onecamp")
	t.Setenv("OIDC_CLIENT_SECRET", "a-secret")
	t.Setenv("BACKEND_DOMAIN", "api.example.com")
	err = InitGenericOIDC()
	if err == nil || !strings.Contains(err.Error(), notAProvider.URL) {
		t.Fatalf("discovery failed with %v; want an error naming the issuer %s", err, notAProvider.URL)
	}
	if OIDCGeneric.Config != nil || OIDCGeneric.Verifier != nil {
		t.Fatal("a failed setup left OIDC half set up")
	}
}
