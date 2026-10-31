package Calendar

import (
	"context"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func weekAt(day, hour, min int) time.Time { return time.Date(2026, 10, day, hour, min, 0, 0, time.UTC) }

func iv(day, h1, m1, h2, m2 int) Interval {
	return Interval{Start: weekAt(day, h1, m1), End: weekAt(day, h2, m2)}
}

// Overlaps count once, a one-to-one is one other person, focus inside a
// meeting is the meeting's, and the people met most come first.
func TestYourWeekCountsOverlapsOnce(t *testing.T) {
	maya, sam, lee := uuid.New(), uuid.New(), uuid.New()
	from, to := weekAt(26, 0, 0), weekAt(31, 0, 0)
	items := []weekItem{
		{Interval: iv(26, 9, 0, 10, 0), Kind: itemMeeting, Others: []uuid.UUID{maya}},
		// Overlaps the first by half an hour: 1:30 of meetings, not 2:00.
		{Interval: iv(26, 9, 30, 10, 30), Kind: itemMeeting, Others: []uuid.UUID{maya, sam}, ToAnswer: true},
		{Interval: iv(27, 14, 0, 15, 0), Kind: itemMeeting, Others: []uuid.UUID{lee}, External: 1},
		// Focus from 10:00 to 12:00: its first half hour is the meeting's.
		{Interval: iv(26, 10, 0, 12, 0), Kind: EventFocus},
		{Interval: iv(28, 8, 0, 9, 0), Kind: itemOwn},
		// Before the week: clipped away.
		{Interval: Interval{Start: weekAt(25, 9, 0), End: weekAt(25, 10, 0)}, Kind: itemMeeting, Others: []uuid.UUID{lee}},
	}
	work := []Interval{iv(26, 9, 0, 17, 0), iv(27, 9, 0, 17, 0)}
	busy := []Interval{iv(26, 9, 0, 12, 0), iv(27, 14, 0, 15, 0)}
	w := weekOf(items, from, to, work, busy)

	if w.MeetingMinutes != 150 || w.Meetings != 3 {
		t.Fatalf("meetings: %d minutes in %d, want 150 in 3 (9:00-10:30 once, and 14:00-15:00)", w.MeetingMinutes, w.Meetings)
	}
	if w.OneOnOneMinutes != 60 || w.GroupMinutes != 120 {
		t.Fatalf("one to one %d, group %d: want 60 (Maya) and 120 (Maya+Sam, Lee+a guest by email)", w.OneOnOneMinutes, w.GroupMinutes)
	}
	if w.FocusMinutes != 90 || w.OwnMinutes != 60 {
		t.Fatalf("focus %d, own %d: want 90 (10:30-12:00) and 60", w.FocusMinutes, w.OwnMinutes)
	}
	if w.AverageMeetingMinutes != 60 || w.ToAnswer != 1 {
		t.Fatalf("average %d, to answer %d: want 60 and 1", w.AverageMeetingMinutes, w.ToAnswer)
	}
	if w.WorkingMinutes != 960 || w.FreeMinutes != 960-180-60 {
		t.Fatalf("working %d, free %d: want 960 and 720", w.WorkingMinutes, w.FreeMinutes)
	}
	if len(w.People) != 3 || w.People[0].UUID != maya.String() || w.People[0].Minutes != 120 || w.People[0].Meetings != 2 {
		t.Fatalf("people met most: %+v, want Maya first with 120 minutes in 2", w.People)
	}
}

// The usual week is the average of the four before, meetings only.
func TestYourWeekUsualIsTheAverageOfTheFourBefore(t *testing.T) {
	span := 7 * 24 * time.Hour
	from := weekAt(26, 0, 0)
	before := from.Add(-4 * span)
	maya := []uuid.UUID{uuid.New()}
	items := []weekItem{
		{Interval: Interval{Start: before.Add(9 * time.Hour), End: before.Add(11 * time.Hour)}, Kind: itemMeeting, Others: maya},
		{Interval: Interval{Start: from.Add(-span + 9*time.Hour), End: from.Add(-span + 11*time.Hour)}, Kind: itemMeeting, Others: maya},
		{Interval: Interval{Start: from.Add(-span + 13*time.Hour), End: from.Add(-span + 15*time.Hour)}, Kind: EventFocus},
	}
	if got := usualMeetings(items, before, from, span); got != 60 {
		t.Fatalf("usual week: %d minutes, want 60 (240 over 4 weeks; focus isn't a meeting)", got)
	}
}

// Your week is only ever the asker's own: an event they aren't on, one they
// declined, one hidden from their own sight of the guests, and someone else's
// working hours tell it nothing. The route takes no person; this is its
// refusal: Sam's own meeting with Lee never reaches Maya's week.
func TestYourWeekIsOnlyTheAskersOwn(t *testing.T) {
	m := onMemStore(t)
	maya, sam, lee := uuid.New(), uuid.New(), uuid.New()
	prevHours, prevPeople := hoursOfPeople, stored.people
	t.Cleanup(func() { hoursOfPeople, stored.people = prevHours, prevPeople })
	stored.ready = func(context.Context) error { return nil }
	hoursOfPeople = func(_ context.Context, people []uuid.UUID) (map[uuid.UUID]WorkingHours, error) {
		if len(people) != 1 || people[0] != maya {
			t.Fatalf("hours read for %v, want only the asker", people)
		}
		return nil, nil
	}
	var named []string
	stored.people = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		named = append(named, ids...)
		out := []*dgraphStruct.DgraphUser{}
		for _, id := range ids {
			out = append(out, &dgraphStruct.DgraphUser{Uuid: id, UserFullName: "Sam Rivera"})
		}
		return out, nil
	}
	put := func(by uuid.UUID, start, end time.Time, kind string, guests ...models.Attendee) {
		id := uuid.New()
		s, e := start, end
		m.events[id] = &models.Event{ID: id, CalendarID: uuid.New(), Kind: kind, Title: "x", StartAt: &s, EndAt: &e, TZ: "UTC",
			Transparency: transparencyOpaque, Visibility: visibilityDefault, Status: statusConfirmed, CreatedBy: by, GuestsCanSeeGuests: true}
		for i := range guests {
			guests[i].EventID = id
		}
		m.guests[id] = guests
	}
	guest := func(who uuid.UUID, answer string) models.Attendee {
		w := who
		return models.Attendee{UserID: &w, Role: roleRequired, Response: answer}
	}
	put(maya, weekAt(26, 9, 0), weekAt(26, 10, 0), string(EventPlain), guest(sam, responseAccepted))
	put(sam, weekAt(26, 11, 0), weekAt(26, 12, 0), string(EventPlain), guest(lee, responseAccepted))    // not Maya's
	put(sam, weekAt(26, 13, 0), weekAt(26, 14, 0), string(EventPlain), guest(maya, responseDeclined))   // declined
	put(sam, weekAt(27, 9, 0), weekAt(27, 10, 0), string(EventPlain), guest(maya, responseNeedsAction)) // to answer

	w, err := YourWeek(context.Background(), &model.UserInfo{UserPostgresInfo: model.User{Id: maya}}, weekAt(26, 0, 0), weekAt(2, 0, 0).AddDate(0, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if w.MeetingMinutes != 120 || w.Meetings != 2 || w.ToAnswer != 1 {
		t.Fatalf("Maya's week: %d minutes in %d meetings, %d to answer; want 120 in 2 and 1 (not Sam and Lee's, not the declined one)", w.MeetingMinutes, w.Meetings, w.ToAnswer)
	}
	if w.HasHours || w.FreeMinutes != 0 {
		t.Fatalf("no working hours set: has %v, free %d", w.HasHours, w.FreeMinutes)
	}
	if len(w.People) != 1 || w.People[0].UUID != sam.String() || w.People[0].Name != "Sam Rivera" {
		t.Fatalf("people: %+v, want Sam alone, named", w.People)
	}
	for _, id := range named {
		if id == lee.String() {
			t.Fatalf("Lee was read for Maya's week: %v", named)
		}
	}
	if _, err := YourWeek(context.Background(), &model.UserInfo{UserPostgresInfo: model.User{Id: maya}}, weekAt(1, 0, 0), weekAt(1, 0, 0).AddDate(0, 2, 0)); err == nil {
		t.Fatal("two months at once should be refused")
	}
}

// A timed event a whole day long marks the day and isn't a meeting; time
// off as long still counts (the demo's 24-hour "Launch day" read as 24h of
// meetings on the live walk, 28 Oct).
func TestYourWeekLeavesOutAWholeDayMarker(t *testing.T) {
	me, maya := uuid.New(), uuid.New()
	timedEvent := func(kind EventKind, start time.Time, d time.Duration) *models.Event {
		end := start.Add(d)
		return &models.Event{ID: uuid.New(), CreatedBy: me, Kind: string(kind), StartAt: &start, EndAt: &end}
	}
	guests := []models.Attendee{{UserID: &maya, Response: responseAccepted}}
	if _, ok := weekItemOf(single(timedEvent(EventPlain, weekAt(29, 4, 0), 24*time.Hour)), guests, me); ok {
		t.Fatal("a 24-hour timed event counted as a meeting")
	}
	if _, ok := weekItemOf(single(timedEvent(EventPlain, weekAt(29, 9, 0), 23*time.Hour)), guests, me); !ok {
		t.Fatal("a 23-hour meeting should still count")
	}
	if it, ok := weekItemOf(single(timedEvent(EventAway, weekAt(29, 0, 0), 48*time.Hour)), nil, me); !ok || it.Kind != EventAway {
		t.Fatalf("two days away: %v %+v, want it counted as away", ok, it)
	}
}
