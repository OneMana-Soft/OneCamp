package authService

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// LDAPConfig holds LDAP/AD bind parameters resolved for a single login attempt.
//
// Multi-tenant note: today this is built from process env vars (single-tenant).
// When oneCamp grows org tables, the resolver below should be swapped for a
// per-org lookup keyed by something derivable from the request (subdomain,
// org_id query param, email domain, etc.). The shape of LDAPConfig and its
// callers don't need to change.
type LDAPConfig struct {
	Enabled         bool
	Host            string
	Port            int
	UseTLS          bool
	BindDN          string
	BindPassword    string
	BaseDN          string
	UserFilter      string // expects exactly one %s for the escaped username/email
	GroupAttribute  string // user-entry attr listing groups; default "memberOf"
	AdminGroupAllow []string
}

// OIDCConfig is the per-login-attempt OIDC settings.
type OIDCConfig struct {
	Enabled              bool
	IssuerURL            string
	ClientID             string
	ClientSecret         string
	RequireVerifiedEmail bool
	AdminGroupClaim      string   // claim name to inspect for admin membership
	AdminGroupAllow      []string // values that grant admin
}

// SAMLConfig is the per-login-attempt SAML settings.
type SAMLConfig struct {
	Enabled         bool
	SPCertPath      string
	SPKeyPath       string
	IdPMetadataURL  string
	BackendDomain   string
	AdminGroupAttr  string   // attribute name (e.g. "memberOf")
	AdminGroupAllow []string // values that grant admin
}

// ResolveLDAPConfig is the single place LDAP config is read. ctx is unused
// today but is the seam for per-org lookup tomorrow.
func ResolveLDAPConfig(_ context.Context) LDAPConfig {
	port, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LDAP_PORT")))
	if err != nil || port == 0 {
		port = 389
	}
	return LDAPConfig{
		Enabled:         strings.EqualFold(os.Getenv("LDAP_ENABLED"), "true"),
		Host:            strings.TrimSpace(os.Getenv("LDAP_HOST")),
		Port:            port,
		UseTLS:          strings.EqualFold(os.Getenv("LDAP_USE_TLS"), "true"),
		BindDN:          os.Getenv("LDAP_BIND_DN"),
		BindPassword:    os.Getenv("LDAP_BIND_PASSWORD"),
		BaseDN:          os.Getenv("LDAP_BASE_DN"),
		UserFilter:      os.Getenv("LDAP_USER_FILTER"),
		GroupAttribute:  strings.TrimSpace(os.Getenv("LDAP_GROUP_ATTRIBUTE")),
		AdminGroupAllow: splitCSV(os.Getenv("LDAP_ADMIN_GROUPS")),
	}
}

// ResolveOIDCConfig is the seam for per-org OIDC config lookup.
func ResolveOIDCConfig(_ context.Context) OIDCConfig {
	return OIDCConfig{
		Enabled:              strings.EqualFold(os.Getenv("OIDC_ENABLED"), "true"),
		IssuerURL:            strings.TrimSpace(os.Getenv("OIDC_ISSUER_URL")),
		ClientID:             strings.TrimSpace(os.Getenv("OIDC_CLIENT_ID")),
		ClientSecret:         strings.TrimSpace(os.Getenv("OIDC_CLIENT_SECRET")),
		RequireVerifiedEmail: !strings.EqualFold(os.Getenv("OIDC_REQUIRE_VERIFIED_EMAIL"), "false"),
		AdminGroupClaim:      strings.TrimSpace(os.Getenv("OIDC_ADMIN_GROUP_CLAIM")),
		AdminGroupAllow:      splitCSV(os.Getenv("OIDC_ADMIN_GROUPS")),
	}
}

// ResolveSAMLConfig is the seam for per-org SAML config lookup.
func ResolveSAMLConfig(_ context.Context) SAMLConfig {
	return SAMLConfig{
		Enabled:         strings.EqualFold(os.Getenv("SAML_ENABLED"), "true"),
		SPCertPath:      os.Getenv("SAML_SP_CERT_PATH"),
		SPKeyPath:       os.Getenv("SAML_SP_KEY_PATH"),
		IdPMetadataURL:  os.Getenv("SAML_IDP_METADATA_URL"),
		BackendDomain:   os.Getenv("BACKEND_DOMAIN"),
		AdminGroupAttr:  strings.TrimSpace(os.Getenv("SAML_ADMIN_GROUP_ATTR")),
		AdminGroupAllow: splitCSV(os.Getenv("SAML_ADMIN_GROUPS")),
	}
}

// FrontendBaseURL returns the canonical frontend origin for redirects, with
// scheme. Resolution order matches the Google Calendar controller pattern:
//
//	FRONTEND_DOMAIN → FE_HOST_DOMAIN → "localhost:3001" fallback.
//
// The scheme is http only when the host is localhost/127.0.0.1 or
// COOKIE_SECURE=false (dev). Otherwise https.
func FrontendBaseURL() string {
	frontendDomain := strings.TrimSpace(os.Getenv("FRONTEND_DOMAIN"))
	if frontendDomain == "" {
		frontendDomain = strings.TrimSpace(os.Getenv("FE_HOST_DOMAIN"))
	}
	if frontendDomain == "" {
		frontendDomain = "localhost:3001"
	}
	// Already a full URL? (FE_HOST_DOMAIN historically permitted this.)
	if strings.HasPrefix(frontendDomain, "http://") || strings.HasPrefix(frontendDomain, "https://") {
		return strings.TrimRight(frontendDomain, "/")
	}
	return scheme(frontendDomain) + "://" + strings.TrimRight(frontendDomain, "/")
}

// IsRedirectAllowed defends against open-redirect: only same-origin (FE base
// URL) targets may be used as the post-login landing.
//
// Strict comparison: parses both URLs and matches scheme + host + port. Naive
// prefix matching is unsafe (e.g. "https://app.example.com.evil.com" begins
// with "https://app.example.com"), so we never use HasPrefix here.
//
// FrontendBaseURL always returns something (with localhost:3001 as the final
// fallback), so a misconfigured deploy can't accidentally widen this gate.
func IsRedirectAllowed(target string) bool {
	if target == "" {
		return false
	}
	feBase := FrontendBaseURL()
	if feBase == "" {
		return false
	}
	feURL, err := url.Parse(feBase)
	if err != nil || feURL.Scheme == "" || feURL.Host == "" {
		return false
	}
	tgtURL, err := url.Parse(target)
	if err != nil || tgtURL.Scheme == "" || tgtURL.Host == "" {
		return false
	}
	return strings.EqualFold(tgtURL.Scheme, feURL.Scheme) &&
		strings.EqualFold(tgtURL.Host, feURL.Host)
}

// BackendBaseURL is this server's public origin: where a third party (Slack's
// Events API, for one) must send its requests.
func BackendBaseURL() string {
	backend := strings.TrimRight(strings.TrimSpace(os.Getenv("BACKEND_DOMAIN")), "/")
	if backend == "" {
		backend = "localhost:3000"
	}
	if strings.HasPrefix(backend, "http://") || strings.HasPrefix(backend, "https://") {
		return backend
	}
	return scheme(backend) + "://" + backend
}

func scheme(host string) string {
	host = strings.ToLower(host)
	if strings.Contains(host, "localhost") ||
		strings.Contains(host, "127.0.0.1") ||
		strings.EqualFold(os.Getenv("COOKIE_SECURE"), "false") {
		return "http"
	}
	return "https"
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// MatchAdminGroup reports whether any IdP-supplied group claim matches the
// configured admin allowlist (case-insensitive). Empty allowlist disables
// SSO-driven admin sync entirely.
//
// The check is intentionally case-insensitive because Okta/AzureAD often
// surface group names with different casing than configured.
func MatchAdminGroup(allow []string, claimed []string) bool {
	if len(allow) == 0 || len(claimed) == 0 {
		return false
	}
	allowSet := make(map[string]struct{}, len(allow))
	for _, a := range allow {
		allowSet[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
	}
	for _, c := range claimed {
		if _, ok := allowSet[strings.ToLower(strings.TrimSpace(c))]; ok {
			return true
		}
	}
	return false
}
