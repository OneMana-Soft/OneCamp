package Calendar

// A working location per day (calendar head 5d, gcal-9b): an all-day or
// timed event of kind working_location says where its owner works ("Home",
// "Office", a place), shown on their day and beside them where others pick
// a time with them. It is never busy, and has no guests.

import (
	"context"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// placeOf is where a working location says its owner works. Pure.
func placeOf(e *models.Event) string {
	if e.Location != "" {
		return e.Location
	}
	return e.Title
}

// keepWorkingLocation holds a working location to its own rules when it is
// made and at every change, by any route (keepKind): never busy, and named by
// its place. That it has no guests is own time's rule (keepOwnTime). Other
// kinds pass as they are.
func keepWorkingLocation(e *models.Event) error {
	if EventKind(e.Kind) != EventWorkingLocation {
		return nil
	}
	if e.Location == "" {
		e.Location = e.Title
	}
	if e.Location == "" {
		return inputError("Say where you're working.")
	}
	if e.Title == "" {
		e.Title = e.Location
	}
	e.Transparency = transparencyTransparent
	return nil
}

// whereOn is where each of these people works on day, as asker may be told,
// by a working location that day (of two, the later starting), a repeating
// one by its instance that day: a person with none isn't named. One its
// owner marked private is theirs alone (CR-68 MF1: hiddenFrom hides a private
// event's details; here even from a delegate).
func (s *service) whereOn(ctx context.Context, asker uuid.UUID, people []uuid.UUID, day time.Time) (map[string]string, error) {
	out := map[string]string{}
	if len(people) == 0 {
		return out, nil
	}
	w, err := window(day, day.Add(24*time.Hour-time.Minute))
	if err != nil {
		return nil, err
	}
	rows, err := s.store.placesOf(ctx, people, w, maxInstances)
	if err != nil {
		return nil, err
	}
	places, err := s.instancesOf(ctx, rows, w, maxInstances)
	if err != nil {
		return nil, err
	}
	date := day.Format(dateLayout)
	for _, e := range places {
		if !onDate(e.Event, day, date) || e.Visibility == visibilityPrivate && e.CreatedBy != asker {
			continue
		}
		out[e.CreatedBy.String()] = placeOf(e.Event)
	}
	return out, nil
}

// onDate is whether e covers date: an all-day one by its dates (the end
// exclusive), a timed one when it runs at any time that UTC day. Pure.
func onDate(e *models.Event, day time.Time, date string) bool {
	if e.AllDay() {
		return e.StartDate <= date && date < e.EndDate
	}
	if e.StartAt == nil || e.EndAt == nil {
		return false
	}
	return e.StartAt.Before(day.Add(24*time.Hour)) && e.EndAt.After(day)
}

// WhereSeenBy is where each of these people works on day (a date), for those
// the asker may see when they're busy (PeopleSeenBy, the one rule): the rest
// say nothing.
func WhereSeenBy(ctx context.Context, askerUUID string, people []uuid.UUID, day time.Time) (map[string]string, error) {
	asked := make([]string, len(people))
	for i, p := range people {
		asked[i] = p.String()
	}
	seen, _, err := PeopleSeenBy(ctx, askerUUID, asked)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(seen))
	for _, p := range seen {
		if id, err := uuid.Parse(p); err == nil {
			ids = append(ids, id)
		}
	}
	asker, _ := uuid.Parse(askerUUID)
	return stored.whereOn(ctx, asker, ids, day)
}
