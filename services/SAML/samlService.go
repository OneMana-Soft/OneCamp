package saml

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/crewjam/saml/samlsp"
)

var SAMLMiddleware *samlsp.Middleware

func InitSAML() error {
	if !helpers.EnvFlag("SAML_ENABLED") {
		return nil
	}

	certPath := os.Getenv("SAML_SP_CERT_PATH")
	keyPath := os.Getenv("SAML_SP_KEY_PATH")
	metadataURLStr := os.Getenv("SAML_IDP_METADATA_URL")
	backendURLStr := os.Getenv("BACKEND_DOMAIN") // e.g. "api.onecamp.com" or "localhost:8080"

	if certPath == "" || keyPath == "" || metadataURLStr == "" || backendURLStr == "" {
		return fmt.Errorf("missing required SAML environment variables")
	}

	keyPair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("failed to load SAML X509 key pair: %w", err)
	}

	keyPair.Leaf, err = x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		return fmt.Errorf("failed to parse SAML certificate leaf: %w", err)
	}

	idpMetadataURL, err := url.Parse(metadataURLStr)
	if err != nil {
		return fmt.Errorf("failed to parse SAML IdP metadata URL: %w", err)
	}

	// Bounded fetch: the IdP metadata endpoint is enterprise-managed
	// but we don't want a slow / hanging IdP to keep server startup
	// blocked or the SAML re-init handler hung. 30s is more than
	// generous for what is typically <100KB of XML.
	metadataClient := &http.Client{Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	idpMetadata, err := samlsp.FetchMetadata(ctx, metadataClient, *idpMetadataURL)
	if err != nil {
		return fmt.Errorf("failed to fetch SAML IdP metadata: %w", err)
	}

	protocol := "https://"
	if os.Getenv("COOKIE_SECURE") == "false" || stringsContainsLocalhost(backendURLStr) {
		protocol = "http://"
	}

	rootURL, err := url.Parse(protocol + backendURLStr)
	if err != nil {
		return fmt.Errorf("failed to parse SAML root URL: %w", err)
	}

	// The cookie that tracks a sign-in started here must come back on the
	// IdP's answer, a cross-site POST to /saml/acs. Without a SameSite, Chrome
	// takes it as Lax and carries it on a cross-site POST only within 2 minutes
	// of setting it, so a sign-in that took longer at the IdP (MFA) failed as
	// an invalid assertion. None carries it however long it takes; it is
	// host-only on the API and scoped to the ACS path. None needs Secure, so
	// over plain http (development) it stays Lax.
	trackerSameSite := http.SameSiteLaxMode
	if rootURL.Scheme == "https" {
		trackerSameSite = http.SameSiteNoneMode
	}

	SAMLMiddleware, err = samlsp.New(samlsp.Options{
		URL:               *rootURL,
		Key:               keyPair.PrivateKey.(*rsa.PrivateKey),
		Certificate:       keyPair.Leaf,
		IDPMetadata:       idpMetadata,
		AllowIDPInitiated: true,
		CookieSameSite:    trackerSameSite,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize SAML service provider: %w", err)
	}

	return nil
}

func stringsContainsLocalhost(s string) bool {
	return len(s) > 0 && (s == "localhost" || s == "127.0.0.1" || (len(s) >= 9 && s[:9] == "localhost") || (len(s) >= 9 && s[:9] == "127.0.0.1"))
}
