package firebaseInit

import "testing"

// A web push opens the workspace's own web app. It opened https://$FE_DOMAIN/,
// and on Cloud FE_DOMAIN is the zone (onemana.dev), the storefront: every
// notification clicked on a Cloud workspace landed on the product site.
func TestAPushOpensTheWorkspacesOwnWebApp(t *testing.T) {
	t.Setenv("FE_DOMAIN", "onemana.dev")
	t.Setenv("FRONTEND_DOMAIN", "")
	t.Setenv("FE_HOST_DOMAIN", "acme.onemana.dev")
	t.Setenv("COOKIE_SECURE", "")
	if got := pushLink(); got != "https://acme.onemana.dev/" {
		t.Errorf("a push opens %q, want the workspace's own web app", got)
	}
	// A self-hosted workspace on its own domain.
	t.Setenv("FE_DOMAIN", "acme.com")
	t.Setenv("FE_HOST_DOMAIN", "onecamp.acme.com")
	if got := pushLink(); got != "https://onecamp.acme.com/" {
		t.Errorf("a push opens %q, want https://onecamp.acme.com/", got)
	}
}
