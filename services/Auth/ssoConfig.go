package authService

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
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
	CACertPath      string // LDAP_CA_CERT: PEM bundle of CAs LDAPS trusts besides the system's
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
		Enabled:         helpers.EnvFlag("LDAP_ENABLED"),
		Host:            strings.TrimSpace(os.Getenv("LDAP_HOST")),
		Port:            port,
		UseTLS:          helpers.EnvFlag("LDAP_USE_TLS"),
		BindDN:          os.Getenv("LDAP_BIND_DN"),
		BindPassword:    os.Getenv("LDAP_BIND_PASSWORD"),
		BaseDN:          os.Getenv("LDAP_BASE_DN"),
		UserFilter:      os.Getenv("LDAP_USER_FILTER"),
		GroupAttribute:  strings.TrimSpace(os.Getenv("LDAP_GROUP_ATTRIBUTE")),
		AdminGroupAllow: splitCSV(os.Getenv("LDAP_ADMIN_GROUPS")),
		CACertPath:      strings.TrimSpace(os.Getenv("LDAP_CA_CERT")),
	}
}

// ResolveOIDCConfig is the seam for per-org OIDC config lookup.
func ResolveOIDCConfig(_ context.Context) OIDCConfig {
	return OIDCConfig{
		Enabled:              helpers.EnvFlag("OIDC_ENABLED"),
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
		Enabled:         helpers.EnvFlag("SAML_ENABLED"),
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

// BackendBaseURL returns this server's public origin, with scheme, from
// BACKEND_DOMAIN. The same scheme rule as FrontendBaseURL. Used wherever the
// server names itself to someone outside: email links, and the OAuth issuer
// and resource an MCP client checks against the URL it was given.
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

// MCPResourceMetadataURL is where an MCP client that was refused learns how to
// sign in (RFC 9728). Here rather than in the OAuth package so the API-token
// middleware, which every edition compiles, can name it without importing
// agent code.
func MCPResourceMetadataURL() string {
	return BackendBaseURL() + "/.well-known/oauth-protected-resource/v1/mcp"
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

// Refusals every browser sign-in shares (Google, GitHub, single sign-on),
// each with words on the sign-in page: cancelled at the provider (it comes
// back as error=access_denied), and a sign-in whose state is gone, having
// taken too long or been opened twice (a second tab, the back button).
const (
	SignInCancelled        = "signin_cancelled"
	SignInCancelledMessage = "Sign-in was cancelled. Try again when you're ready."
	SignInExpired          = "signin_expired"
	SignInExpiredMessage   = "That sign-in took too long or was already used. Start again."
)

// SignInErrorURL is the sign-in page saying why a sign-in failed, on the web
// app's own origin, so a refusal never sends anyone elsewhere. code is one the
// page has words for (knownErrorMessages in the web app's app/page.tsx), and
// they are all it shows. message says the same plainly for whoever reads the
// URL; the page never displays it, so a crafted link can't put words on the
// page, and it is never an internal error, which can carry what a provider
// answered. Every browser sign-in (single sign-on, Google, GitHub) refuses
// through this.
func SignInErrorURL(code, message string) string {
	feBase := FrontendBaseURL()
	u, err := url.Parse(feBase)
	if feBase == "" || err != nil || u.Host == "" {
		return "/?" + url.Values{"error": {code}, "message": {message}}.Encode()
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("message", message)
	u.RawQuery = q.Encode()
	return u.String()
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
//
// A directory names a group by its DN (Active Directory's memberOf:
// "CN=OneCamp Admins,OU=Groups,DC=example,DC=com"), and the allowlist is
// comma-separated, so a DN put in it arrives in pieces. A DN claim matched
// neither way, and directory admin sync never promoted anyone. It matches by
// its whole DN, its first part ("cn=onecamp admins") or that part's value,
// the group's name ("onecamp admins"), so LDAP_ADMIN_GROUPS takes names.
func MatchAdminGroup(allow []string, claimed []string) bool {
	if len(allow) == 0 || len(claimed) == 0 {
		return false
	}
	allowSet := make(map[string]struct{}, len(allow))
	for _, a := range allow {
		allowSet[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
	}
	for _, c := range claimed {
		for _, name := range groupNames(c) {
			if _, ok := allowSet[name]; ok {
				return true
			}
		}
	}
	return false
}

// groupNames is what a group claim may be matched by, lower case: itself, and
// for a DN its first RDN and that RDN's value. Pure.
func groupNames(claim string) []string {
	c := strings.ToLower(strings.TrimSpace(claim))
	names := []string{c}
	if !strings.Contains(c, "=") {
		return names
	}
	rdn := firstRDN(c)
	if eq := strings.IndexByte(rdn, '='); eq > 0 {
		names = append(names, strings.TrimSpace(rdn), strings.TrimSpace(unescapeDNValue(rdn[eq+1:])))
	}
	return names
}

// firstRDN is a DN up to its first comma that isn't escaped. Pure.
func firstRDN(dn string) string {
	for i := 0; i < len(dn); i++ {
		switch dn[i] {
		case '\\':
			i++ // the next character is escaped
		case ',':
			return dn[:i]
		}
	}
	return dn
}

// unescapeDNValue undoes a DN value's escapes: a backslash before a special
// character, or before two hex digits standing for a byte. Pure.
func unescapeDNValue(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 >= len(v) {
			b.WriteByte(v[i])
			continue
		}
		if i+2 < len(v) {
			if n, err := strconv.ParseUint(v[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 2
				continue
			}
		}
		b.WriteByte(v[i+1])
		i++
	}
	return b.String()
}
