package Calendar

// Own time: focus time, an out of office, a working location, a reminder and
// a task block are someone's own time, never a meeting (ownTimeKinds), and
// keep one rule at every write, by any route: they have no guests. The rule
// was Create's alone for a task block, and a working location's alone at
// every write; focus time, an out of office and a reminder took guests from
// any route, and /event/createEvent made own time with them.
//
// It is the one store's rule. With CALENDAR_STORE=graph, the rollback to the
// two old stores, the /event routes write the graph as they always did, and
// own time takes guests there.

import (
	"slices"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// ownTimeWords says, for each kind of own time, that it has no guests.
var ownTimeWords = map[EventKind]string{
	EventFocus:           "Focus time is yours alone: it has no guests.",
	EventAway:            "An out of office is yours alone: it has no guests.",
	EventWorkingLocation: "A working location is yours alone: it has no guests.",
	EventReminder:        "A reminder is yours alone: it has no guests.",
	EventTaskBlock:       "A task block is yours alone: it has no guests.",
}

// isOwnTime reports whether kind is someone's own time. Pure.
func isOwnTime(kind string) bool { return slices.Contains(ownTimeKinds, kind) }

// guestKeyOf is the guest an entry of a write's list names: none for an entry
// that names no one, or for creator, who is never their own guest. Pure.
func guestKeyOf(w GuestInput, creator uuid.UUID) (models.GuestKey, bool) {
	k, ok := inputKey(w)
	if !ok || (k.UserID != nil && *k.UserID == creator) {
		return models.GuestKey{}, false
	}
	return k, true
}

// keepKind holds an event to its kind's rules at every write, by any route
// (Create, Update, and the /event routes' create and update): own time has no
// guests (keepOwnTime), and a working location says where (keepWorkingLocation).
// was is the event's kind before the write ("" for a new one), creator who
// made it, kept its guests now, and wanted the whole new list the write names
// (nil when it names none).
func keepKind(next *models.Event, was string, creator uuid.UUID, kept []models.Attendee, wanted *[]GuestInput) error {
	if err := keepOwnTime(next, was, creator, kept, wanted); err != nil {
		return err
	}
	return keepWorkingLocation(next)
}

// keepOwnTime refuses a write that leaves own time with a guest it didn't
// have: a new one with guests, a guest added, or an event with guests made
// own time while keeping them. A guest is counted as guestsFor counts one
// (guestKeyOf). One from before the rule that has guests already can still be
// changed otherwise: the form has no way to take them off. Pure.
func keepOwnTime(next *models.Event, was string, creator uuid.UUID, kept []models.Attendee, wanted *[]GuestInput) error {
	if !isOwnTime(next.Kind) {
		return nil
	}
	refused := inputError(ownTimeWords[EventKind(next.Kind)])
	made := !isOwnTime(was)
	if wanted == nil {
		if made && len(kept) > 0 {
			return refused
		}
		return nil
	}
	for _, w := range *wanted {
		if k, ok := guestKeyOf(w, creator); ok && (made || answerOf(kept, k) == nil) {
			return refused
		}
	}
	return nil
}
