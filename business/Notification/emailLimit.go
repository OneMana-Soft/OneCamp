package notification

import (
	"context"

	emailService "github.com/akashc777/OneCamp/services/Email"
)

// leftToday is the email account's day (services/Email, EMAIL_DAILY_CAP), a
// seam for tests.
var leftToday = emailService.LeftToday

// EmailLimitReached is whether today's email is spent (EMAIL_DAILY_CAP, the
// shared free tier of a workspace on OneCamp Cloud), keeping the reserve
// password resets need. An email queued past it goes out tomorrow, too late
// for one due now such as a reminder, which asks this first and, when it is
// reached, comes another way and says so (calendar head A, gcal-5).
func EmailLimitReached(context.Context) bool {
	left, capped := leftToday(emailService.InvitationReserve)
	return capped && left <= 0
}
