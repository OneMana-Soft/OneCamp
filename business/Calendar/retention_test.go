package Calendar

import (
	"errors"
	"testing"
	"time"

	retentionBusiness "github.com/akashc777/OneCamp/business/Retention"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// V-388 (1), CR-203's B6: a part of a hold's export ends on its last event,
// and the next part starts after that event's place in HeldEvents' order: its
// start, an all-day event's at midnight UTC on its first day, then its id. An
// all-day event read as year 1 would empty every part after it and call the
// part before it the last.
func TestAPartsCursorIsItsLastEventsPlace(t *testing.T) {
	id := uuid.MustParse("7d0c2b1e-4b8a-4c1d-9e57-2f1f3c1a9b10")
	at := time.Date(2026, 3, 4, 9, 30, 15, 123456000, time.FixedZone("IST", 5*3600+1800))
	for _, c := range []struct {
		name string
		e    models.Event
		want time.Time
	}{
		{"a timed event, by its start", models.Event{ID: id, StartAt: &at}, at},
		{"an all-day event, by midnight UTC on its first day", models.Event{ID: id, StartDate: "2026-03-04"}, time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)},
	} {
		got, gotID, err := heldCursorOf(heldCursor(&c.e))
		if err != nil || !got.Equal(c.want) || gotID != id {
			t.Errorf("%s: %v %v %v, want %v %v", c.name, got, gotID, err, c.want, id)
		}
	}
}

// A part token that is well formed but whose cursor isn't one heldCursor
// writes is a bad request (400), never an empty part.
func TestACursorTheExportNeverGaveIsRefused(t *testing.T) {
	for _, c := range []string{"", "nope", "2026-03-04T00:00:00Z", "2026-03-04T00:00:00Z_not-a-uuid", "yesterday_7d0c2b1e-4b8a-4c1d-9e57-2f1f3c1a9b10"} {
		if _, _, err := heldCursorOf(c); !errors.Is(err, retentionBusiness.ErrBadRequest) {
			t.Errorf("cursor %q: %v, want ErrBadRequest", c, err)
		}
	}
}
