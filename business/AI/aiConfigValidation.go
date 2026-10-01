package business

// Input validation and normalization for admin-managed AI config.
//
// Custom endpoints accept admin-supplied URLs and labels that flow into
// server-side HTTP requests and the DB, so everything is validated and
// bounded here before it reaches the model layer.

import (
	"fmt"
	"strings"

	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	maxLabelLen  = 100
	maxModelLen  = 200
	maxAPIKeyLen = 8192
)

// validateLabel bounds and trims a provider label.
func validateLabel(label string) (string, error) {
	l := strings.TrimSpace(label)
	if l == "" {
		return "", fmt.Errorf("label is required")
	}
	if len(l) > maxLabelLen {
		return "", fmt.Errorf("label too long (max %d characters)", maxLabelLen)
	}
	return l, nil
}

// validateBaseURL parses and normalizes a custom-endpoint base URL through
// the one endpoint check every admin-supplied URL goes through
// (ai.ValidateHTTPEndpoint). The SSRF dial-time guard
// (services/AI/httpguard.go) is the runtime defense; this is the static
// front door that rejects obviously malformed/abusable input.
func validateBaseURL(raw string) (string, error) {
	return ai.ValidateHTTPEndpoint(raw, "base_url")
}

// validateModelName bounds a model id. Model ids vary widely across
// providers (slashes, colons, dots) so we only bound length and reject
// control characters / whitespace-only.
func validateModelName(model string) (string, error) {
	m := strings.TrimSpace(model)
	if m == "" {
		return "", fmt.Errorf("model is required")
	}
	if len(m) > maxModelLen {
		return "", fmt.Errorf("model id too long (max %d characters)", maxModelLen)
	}
	for _, r := range m {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("model id contains control characters")
		}
	}
	return m, nil
}

// validateAPIKey bounds an API key (when provided).
func validateAPIKey(key string) (string, error) {
	if key == "" {
		return "", nil
	}
	if len(key) > maxAPIKeyLen {
		return "", fmt.Errorf("api_key too long (max %d characters)", maxAPIKeyLen)
	}
	// Keys must not contain newlines/control chars (they go into HTTP headers).
	for _, r := range key {
		if r == '\n' || r == '\r' || r == 0 {
			return "", fmt.Errorf("api_key contains invalid characters")
		}
	}
	return strings.TrimSpace(key), nil
}
