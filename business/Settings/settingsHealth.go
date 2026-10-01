package business

// Will an invitation actually arrive?
//
// THE DAY-ONE CHURN. The first thing an admin does after installing is invite
// their team. With no email key that produces no error and no email: the
// invitation row is created, the worker is not running, and the admin waits for
// people who were never told. They conclude the product is broken, and they are
// not wrong.
//
// THE SILENT BREAK. The key can also be set and unusable. It is stored encrypted
// and read DB-first, so a changed KEK makes DecryptSecret fail and the code falls
// through to the environment variable — which on an install configured through
// the admin UI is empty. Email that worked yesterday stops today, with the key
// still shown as configured in the settings page. This install has that exact
// failure on its MCP secret already, so it is not hypothetical.
//
// The key itself is never read, logged or returned here. Only whether one is
// present, and whether it can be decrypted.

import (
	"context"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
)

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "email",
		Kind: helpers.CheckKindDependency,
		Describe: "An email key is configured and readable, so invitations and password resets can be sent. " +
			"It does not prove delivery: a valid key can still be rejected by the provider or land in spam.",
		Probe: func(_ context.Context) error {
			all := loadAll()
			enc, inDB := all[keyResendAPIKey]

			// Configured in the admin UI but unreadable. Unambiguously broken:
			// somebody set this expecting it to work, and it silently does not.
			if inDB && enc != "" {
				if _, err := helpers.DecryptSecret(enc); err != nil {
					return fmt.Errorf("an email key is saved but cannot be decrypted, so no invitation or " +
						"password reset is being sent; this is what a changed encryption key looks like. " +
						"Re-enter the key in admin email settings")
				}
				return nil
			}

			if !EmailEnabled() {
				// Not an error. Plenty of installs never send mail, and a check
				// that goes red for a deliberate choice is a check people learn
				// to ignore. It is reported as a note instead, because an admin
				// who is about to invite their team needs to know now.
				return helpers.SystemCheckNote("no email key is set, so invitations and password resets are " +
					"silently not sent. Set one in admin email settings before inviting anyone")
			}
			return nil
		},
	})
}
