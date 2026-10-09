package business

// The sending key the email service uses is the one an admin saved in Admin >
// Email, before the environment's (ResendAPIKey), and the sender that goes
// with it (savedSender). The service used to read only the environment, so a
// key saved there showed "Email is on" and sent nothing: every invitation and
// password reset failed while the admin was told email worked.

import (
	"github.com/akashc777/OneCamp/helpers"
	emailService "github.com/akashc777/OneCamp/services/Email"
)

func init() {
	emailService.UseAPIKeyFrom(ResendAPIKey)
	emailService.UseSenderFrom(savedSender)
}

// savedSender is the sender saved in Admin > Email while the key in use was
// saved there too, or "" to use the environment's. A key saved in Admin is
// for the admin's own verified domain: on OneCamp Cloud the environment's
// sender is OneCamp's, which that key cannot send from, and password resets
// would go nowhere. A change made in Admin > Email is seen within settingsTTL.
func savedSender() string {
	all := loadAll()
	enc := all[keyResendAPIKey]
	if enc == "" {
		return ""
	}
	if key, err := helpers.DecryptSecret(enc); err != nil || key == "" {
		return ""
	}
	return emailService.ChosenSender(all[keySenderEmail])
}
