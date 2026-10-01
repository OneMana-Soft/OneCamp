package ai

// One front door for every admin-supplied URL the server will connect to.

import (
	"fmt"
	"net/url"
	"strings"
)

// MaxEndpointURLLen bounds any admin-supplied endpoint.
const MaxEndpointURLLen = 2048

// ValidateHTTPEndpoint parses and normalises an admin-supplied URL that this
// server will make requests to: a custom model endpoint, a remote agent.
//
//   - http or https only (no file://, gopher://, and so on)
//   - a host present
//   - no userinfo (a credential belongs in its own field, never in the URL)
//   - within MaxEndpointURLLen
//
// field names the input in the error, so the message reads in the caller's
// vocabulary. The returned URL has its trailing slash dropped so two spellings
// of one address compare equal. This is the static check; the dial-time guard
// in httpguard.go is the runtime one, and the two are not substitutes.
func ValidateHTTPEndpoint(raw, field string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if len(s) > MaxEndpointURLLen {
		return "", fmt.Errorf("%s too long (max %d characters)", field, MaxEndpointURLLen)
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%s is not a valid URL: %w", field, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("%s must use http or https (got %q)", field, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s must include a host", field)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s must not embed credentials; use the secret field", field)
	}
	return strings.TrimRight(u.String(), "/"), nil
}
