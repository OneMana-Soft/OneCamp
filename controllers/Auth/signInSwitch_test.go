package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/oauth"
	saml "github.com/akashc777/OneCamp/services/SAML"
)

// The sign-in page shows a single sign-on or directory button exactly when
// the server sets that sign-in up at boot. OIDC_ENABLED=True showed the OIDC
// and SAML buttons while their setup, which read only "true", never ran: the
// button led to "OIDC is misconfigured on the server" or "SAML is disabled".
//
// Each setup here is missing everything else it needs, so one that runs at all
// fails, and one that reads its switch as off returns nil.
func TestSignInButtonsShowExactlyWhenSignInIsSetUp(t *testing.T) {
	old := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = old })
	helpers.SeatLimit = ""
	for _, k := range []string{"OIDC_ISSUER_URL", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "SAML_SP_CERT_PATH",
		"SAML_SP_KEY_PATH", "SAML_IDP_METADATA_URL", "LDAP_HOST", "LDAP_BASE_DN", "LDAP_USER_FILTER"} {
		t.Setenv(k, "")
	}

	for _, value := range []string{"true", "True", "TRUE", "1", "yes", "on", "", "false", "False", "0", "no", "off", "enabled"} {
		t.Setenv("OIDC_ENABLED", value)
		t.Setenv("SAML_ENABLED", value)
		t.Setenv("LDAP_ENABLED", value)

		rec := httptest.NewRecorder()
		GetEnabledProviders(rec, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
		var body struct{ Providers map[string]bool }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%q: /auth/providers answered %s", value, rec.Body.String())
		}
		for provider, setUp := range map[string]bool{
			"oidc": oauth.InitGenericOIDC() != nil,
			"saml": saml.InitSAML() != nil,
			"ldap": oauth.InitLDAP() != nil,
		} {
			if body.Providers[provider] != setUp {
				t.Errorf("with %q, the %s button shows: %v, but its setup at boot runs: %v",
					value, provider, body.Providers[provider], setUp)
			}
		}
	}
}
