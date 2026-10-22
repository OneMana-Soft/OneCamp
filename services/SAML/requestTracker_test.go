package saml

// The cookie that tracks a sign-in started here (SP-initiated) must come back
// on the IdP's answer: a cross-site POST to /saml/acs. Written with no
// SameSite, Chrome took it as Lax and carried it on a cross-site POST only
// within 2 minutes of setting it, so a sign-in that took longer at the IdP
// (MFA, a password reset on the way) came back without it and failed as
// "Invalid SAML assertion from directory provider". SameSite=None (with
// Secure) carries it however long it takes; it is host-only on the API and
// scoped to the ACS path. Over plain http (development) None isn't allowed.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// serviceProvider starts SAML as the server does, against an IdP whose
// metadata a local server answers with, and returns the request tracker's
// cookie for one sign-in started here.
func trackerCookie(t *testing.T, backend, cookieSecure string) *http.Cookie {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "sp.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "sp.crt"), filepath.Join(dir, "sp.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.example.test/metadata">
  <IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <KeyDescriptor use="signing"><KeyInfo xmlns="http://www.w3.org/2000/09/xmldsig#"><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></KeyDescriptor>
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example.test/sso"/>
  </IDPSSODescriptor>
</EntityDescriptor>`, base64.StdEncoding.EncodeToString(der))
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		_, _ = w.Write([]byte(metadata))
	}))
	t.Cleanup(idp.Close)

	t.Setenv("SAML_ENABLED", "true")
	t.Setenv("SAML_SP_CERT_PATH", certPath)
	t.Setenv("SAML_SP_KEY_PATH", keyPath)
	t.Setenv("SAML_IDP_METADATA_URL", idp.URL)
	t.Setenv("BACKEND_DOMAIN", backend)
	t.Setenv("COOKIE_SECURE", cookieSecure)
	old := SAMLMiddleware
	t.Cleanup(func() { SAMLMiddleware = old })
	if err := InitSAML(); err != nil {
		t.Fatalf("InitSAML: %v", err)
	}

	rec := httptest.NewRecorder()
	if _, err := SAMLMiddleware.RequestTracker.TrackRequest(rec, httptest.NewRequest(http.MethodGet, "/saml/login", nil), "id-sign-in"); err != nil {
		t.Fatalf("track a sign-in: %v", err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Value != "" {
			return c
		}
	}
	t.Fatal("no request tracker cookie was written")
	return nil
}

func TestASignInStartedHereComesBackHoweverLongItTookAtTheIdP(t *testing.T) {
	c := trackerCookie(t, "acme-api.onemana.dev", "")
	if c.SameSite != http.SameSiteNoneMode {
		t.Errorf("SameSite=%v: Chrome carries it on the IdP's cross-site POST for 2 minutes only", c.SameSite)
	}
	if !c.Secure || !c.HttpOnly || c.Domain != "" || c.Path != "/saml/acs" {
		t.Errorf("Secure=%v HttpOnly=%v Domain=%q Path=%q, want Secure, HttpOnly, host-only, on the ACS path", c.Secure, c.HttpOnly, c.Domain, c.Path)
	}
}

func TestOverPlainHTTPTheTrackerStaysLax(t *testing.T) {
	// A browser refuses SameSite=None without Secure, and plain http can't be
	// Secure: development keeps Lax.
	c := trackerCookie(t, "localhost:8080", "false")
	if c.SameSite == http.SameSiteNoneMode {
		t.Error("SameSite=None over plain http, which a browser refuses")
	}
}
