package Calendar

import (
	"slices"
	"testing"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// Who an event's call keeps what it makes for (meet-02): its organiser first,
// then each member it invites once, in the order they were added. A guest
// invited by email is never one of them.
func TestAnEventsCallPeopleAreItsOrganiserAndMembers(t *testing.T) {
	org, maya, lee := uuid.New(), uuid.New(), uuid.New()
	e := &models.Event{ID: uuid.New(), Title: "Design review", CreatedBy: org}
	p := callPeople(e, []models.Attendee{
		{UserID: &maya}, {Email: "guest@example.org", Name: "Guest"}, {UserID: &org}, {UserID: &lee}, {UserID: &maya},
	})
	if want := []uuid.UUID{org, maya, lee}; p.Title != "Design review" || !slices.Equal(p.Members, want) {
		t.Errorf("callPeople = %q %v, want %q %v", p.Title, p.Members, "Design review", want)
	}
	if alone := callPeople(&models.Event{ID: uuid.New(), CreatedBy: org}, []models.Attendee{{Email: "x@example.org"}}); !slices.Equal(alone.Members, []uuid.UUID{org}) {
		t.Errorf("an organiser with only guests by email = %v, want the organiser alone", alone.Members)
	}
}
