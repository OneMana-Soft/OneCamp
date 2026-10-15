package notification

import (
	"context"

	"github.com/google/uuid"
)

// bookingInbox would write a booking's inbox item for its owner. This edition
// has no inbox items of that kind (the notices of the docs programme are not
// in it), so the owner is told by push, and by email where it's on: it
// answers that they have no item, and that the telling is new, so it is
// pushed.
func bookingInbox(context.Context, uuid.UUID, string, BookingNotice) (told, fresh bool) {
	return false, true
}
