package oauth

import (
	"fmt"
	"os"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// InitLDAP performs config sanity-checks at boot when LDAP_ENABLED=true.
// It does NOT open a connection (the LDAP service binds per-request and
// connections to AD/389DS aren't pooled here), so a temporarily unreachable
// directory at boot doesn't block the server. It only catches misconfigs.
//
// Returns nil when LDAP is disabled; otherwise returns a descriptive error
// when required env is missing. Caller should log + continue (parity with
// InitOAuth / InitGenericOIDC behaviour).
func InitLDAP() error {
	if !helpers.EnvFlag("LDAP_ENABLED") {
		return nil
	}

	missing := []string{}
	for _, k := range []string{"LDAP_HOST", "LDAP_BASE_DN", "LDAP_USER_FILTER"} {
		if strings.TrimSpace(os.Getenv(k)) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("LDAP_ENABLED but missing required env: %s", strings.Join(missing, ", "))
	}

	// Filter must contain exactly one %s — otherwise fmt.Sprintf at request
	// time will produce a malformed filter and silently match nothing.
	filter := os.Getenv("LDAP_USER_FILTER")
	if strings.Count(filter, "%s") != 1 {
		return fmt.Errorf("LDAP_USER_FILTER must contain exactly one %%s placeholder; got %q", filter)
	}

	return nil
}
