//go:build integration
// +build integration

package integration_test

// A directory's SAML answer signs in once (A7). SAML takes IdP-initiated
// answers (any InResponseTo), and crewjam keeps no replay cache, so an answer
// caught in a proxy, a log or a shared machine signed its subject in again,
// from any browser, for as long as it was accepted (MaxIssueDelay, 90 s from
// its issue). Against a real Postgres, Redis and Dgraph, with SAML started as
// the server starts it (InitSAML), against an IdP made here that signs its
// answers:
//   - an answer signs in once; posted again it is refused and starts no
//     session, and its ID is kept only until it expires;
//   - a new answer for the same person signs in;
//   - an answer to a sign-in started here ends that sign-in's tracking cookie.
//
// Run: go test -tags=integration ./tests/integration/ -run TestASAMLAnswerSignsInOnce -v

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"encoding/xml"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	crewjam "github.com/crewjam/saml"
	"github.com/google/uuid"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	authController "github.com/akashc777/OneCamp/controllers/Auth"
	"github.com/akashc777/OneCamp/helpers/authcookie"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	samlService "github.com/akashc777/OneCamp/services/SAML"
)

// selfSigned is a key and a certificate for it, for one side of SAML.
func selfSigned(t *testing.T, name string) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert
}

// startSAML starts SAML as the server does, trusting an IdP made here, and
// returns that IdP.
func startSAML(t *testing.T) *crewjam.IdentityProvider {
	t.Helper()
	idpKey, idpCert := selfSigned(t, "idp.example.test")
	idp := &crewjam.IdentityProvider{
		Key:         idpKey,
		Certificate: idpCert,
		MetadataURL: url.URL{Scheme: "https", Host: "idp.example.test", Path: "/metadata"},
		SSOURL:      url.URL{Scheme: "https", Host: "idp.example.test", Path: "/sso"},
	}
	metadata, err := xml.Marshal(idp.Metadata())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		_, _ = w.Write(metadata)
	}))
	t.Cleanup(server.Close)

	spKey, spCert := selfSigned(t, "api.example.test")
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "sp.crt"), filepath.Join(dir, "sp.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: spCert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(spKey)}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAML_ENABLED", "true")
	t.Setenv("SAML_SP_CERT_PATH", certPath)
	t.Setenv("SAML_SP_KEY_PATH", keyPath)
	t.Setenv("SAML_IDP_METADATA_URL", server.URL)
	t.Setenv("BACKEND_DOMAIN", "api.example.test")
	old := samlService.SAMLMiddleware
	t.Cleanup(func() { samlService.SAMLMiddleware = old })
	if err := samlService.InitSAML(); err != nil {
		t.Fatalf("InitSAML: %v", err)
	}
	return idp
}

// samlAnswer is the IdP's signed answer naming email, as the form its page
// posts to /saml/acs, and the assertion's ID. inResponseTo and relayState are
// a sign-in started here; both are empty for one started at the IdP.
func samlAnswer(t *testing.T, idp *crewjam.IdentityProvider, email, inResponseTo, relayState string) (url.Values, string) {
	t.Helper()
	sp := samlService.SAMLMiddleware.ServiceProvider.Metadata()
	sso := sp.SPSSODescriptors[0]
	req := &crewjam.IdpAuthnRequest{
		IDP:                     idp,
		HTTPRequest:             httptest.NewRequest(http.MethodGet, "https://idp.example.test/sso", nil),
		RelayState:              relayState,
		Request:                 crewjam.AuthnRequest{ID: inResponseTo},
		ServiceProviderMetadata: sp,
		SPSSODescriptor:         &sso,
		ACSEndpoint:             &sso.AssertionConsumerServices[0],
		Now:                     crewjam.TimeNow(),
	}
	session := &crewjam.Session{
		ID: uuid.NewString(), CreateTime: req.Now, ExpireTime: req.Now.Add(time.Hour), Index: "1",
		NameID: email, NameIDFormat: string(crewjam.EmailAddressNameIDFormat), UserEmail: email,
	}
	if err := (crewjam.DefaultAssertionMaker{}).MakeAssertion(req, session); err != nil {
		t.Fatal(err)
	}
	form, err := req.PostBinding()
	if err != nil {
		t.Fatal(err)
	}
	return url.Values{"SAMLResponse": {form.SAMLResponse}, "RelayState": {form.RelayState}}, req.Assertion.ID
}

// postToACS posts a directory's answer to /saml/acs, with cookies a browser
// would carry.
func postToACS(answer url.Values, cookies ...*http.Cookie) *http.Response {
	req := httptest.NewRequest(http.MethodPost, "https://api.example.test/saml/acs", strings.NewReader(answer.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	authController.SAMLCallback(rec, req)
	return rec.Result()
}

// startedASession is whether an answer to /saml/acs started a session.
func startedASession(res *http.Response) bool {
	for _, c := range res.Cookies() {
		if c.Name == authcookie.Name(authcookie.Access) && c.Value != "" {
			return res.StatusCode == http.StatusFound && !strings.Contains(res.Header.Get("Location"), "error=")
		}
	}
	return false
}

func TestASAMLAnswerSignsInOnce(t *testing.T) {
	lr := setupLandReady(t)
	if _, _, err := userBusiness.CreateUserWithMethod(lr.ctx, "ada@example.test", "Ada", nil, userModels.AuthMethodSAML, true); err != nil {
		t.Fatal(err)
	}
	idp := startSAML(t)

	answer, id := samlAnswer(t, idp, "ada@example.test", "", "")
	if res := postToACS(answer); !startedASession(res) {
		t.Fatalf("the answer, first posted: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	res := postToACS(answer)
	if startedASession(res) || !strings.Contains(res.Header.Get("Location"), "error=signin_expired") {
		t.Fatalf("the same answer, posted again: %d %s, want it refused as already used", res.StatusCode, res.Header.Get("Location"))
	}
	ttl, err := redisInit.RedisClient.TTL(lr.ctx, registry.SAMLAssertionUsed.Build(id)).Result()
	if err != nil || ttl <= 0 || ttl > crewjam.MaxIssueDelay {
		t.Errorf("the used answer is kept for %v (%v), want until it expires, at most %v", ttl, err, crewjam.MaxIssueDelay)
	}

	if answer, _ := samlAnswer(t, idp, "ada@example.test", "", ""); !startedASession(postToACS(answer)) {
		t.Fatal("a new answer for the same person was refused")
	}

	// A sign-in started here: its tracking cookie ends with its answer.
	started, requestID := httptest.NewRecorder(), "id-"+uuid.NewString()
	index, err := samlService.SAMLMiddleware.RequestTracker.TrackRequest(started, httptest.NewRequest(http.MethodGet, "https://api.example.test/saml/login", nil), requestID)
	if err != nil {
		t.Fatal(err)
	}
	tracking := started.Result().Cookies()
	if len(tracking) != 1 {
		t.Fatalf("starting a sign-in set %d cookies, want its tracking cookie", len(tracking))
	}
	answer, _ = samlAnswer(t, idp, "ada@example.test", requestID, index)
	res = postToACS(answer, tracking...)
	if !startedASession(res) {
		t.Fatalf("the answer to a sign-in started here: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	ended := false
	for _, c := range res.Cookies() {
		if c.Name == tracking[0].Name && (c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(time.Now()))) {
			ended = true
		}
	}
	if !ended {
		t.Errorf("the sign-in's tracking cookie %s was left to vouch for another answer", tracking[0].Name)
	}
}
