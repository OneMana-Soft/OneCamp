package helpers

import (
	"os"
	"strings"
)

// EnvFlag reports whether the environment switches key on: true, 1, yes or on,
// in any case, spaces around ignored. Anything else, unset included, is off.
//
// One reading wherever a switch is checked, so two places can't disagree about
// it. They did for sign-in: the sign-in page's button took OIDC_ENABLED=True
// as on, the setup at boot took only "true", and the button led to a sign-in
// that had never been set up.
func EnvFlag(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}
