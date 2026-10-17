package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	calendarAdapter "github.com/akashc777/OneCamp/adapter/Calendar"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	savedItemBusiness "github.com/akashc777/OneCamp/business/SavedItem"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	savedItemModel "github.com/akashc777/OneCamp/models/postgres/SavedItem"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

func inZone(tz string) context.Context {
	return context.WithValue(context.Background(), ai.LocalizationContextKey, map[string]string{"timezone": tz})
}

// noCalendarWrites fails the test if anything puts an event on a calendar.
func noCalendarWrites(t *testing.T) {
	t.Helper()
	prev := createCalendarEvent
	t.Cleanup(func() { createCalendarEvent = prev })
	createCalendarEvent = func(context.Context, *userModels.UserInfo, calendarAdapter.CreateOrUpdateEventInput) (*calendarAdapter.OutputEvent, error) {
		t.Fatal("a calendar event was made")
		return nil, nil
	}
}

// CAL-BUG-6: "remind me" sets a reminder that comes back by itself, a Later
// item due at the time, and puts nothing on the calendar.
func TestAReminderIsALaterItemNotAnEvent(t *testing.T) {
	noCalendarWrites(t)
	var got savedItemBusiness.SaveInput
	var owner uuid.UUID
	prev := saveLater
	t.Cleanup(func() { saveLater = prev })
	saveLater = func(_ context.Context, userID uuid.UUID, in savedItemBusiness.SaveInput) (*savedItemModel.SavedItem, error) {
		got, owner = in, userID
		return &savedItemModel.SavedItem{ID: uuid.New(), UserID: userID, ItemType: in.ItemType, RemindAt: in.RemindAt}, nil
	}

	me := uuid.New()
	msg, data, err := executeSetReminder(inZone("Asia/Kolkata"), ai.ProposedAction{ToolName: "set_reminder", Params: map[string]string{
		"title": "Call the bank", "start_time": "2026-10-13T09:30:00Z", "description": "about the card",
	}}, me.String())
	if err != nil {
		t.Fatal(err)
	}
	if owner != me || got.ItemType != "reminder" || got.Title != "Call the bank" || got.Link != "/app/later" || got.Context != "about the card" {
		t.Fatalf("saved %+v for %s", got, owner)
	}
	if got.RemindAt == nil || !got.RemindAt.Equal(time.Date(2026, 10, 13, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("due %v, want 09:30 UTC", got.RemindAt)
	}
	// Later takes it as it is: a reminder is a kind of saved item.
	if err := got.Validate(got.RemindAt.Add(-time.Hour)); err != nil {
		t.Fatalf("Later refuses the reminder: %v", err)
	}
	// Said on the person's clock, not the model's offset in a US format.
	if !strings.Contains(msg, "13 Oct, 15:00") {
		t.Fatalf("answer %q, want the time in Asia/Kolkata, 13 Oct, 15:00", msg)
	}
	if data["saved_item_id"] == "" || data["event_uuid"] != "" {
		t.Fatalf("action data %v", data)
	}
}

func TestAReminderForATimeLaterRefusesIsSaidPlainly(t *testing.T) {
	noCalendarWrites(t)
	prev := saveLater
	t.Cleanup(func() { saveLater = prev })
	saveLater = func(context.Context, uuid.UUID, savedItemBusiness.SaveInput) (*savedItemModel.SavedItem, error) {
		return nil, savedItemBusiness.ErrInvalid
	}
	msg, _, err := executeSetReminder(context.Background(), ai.ProposedAction{Params: map[string]string{"title": "x", "start_time": "2020-01-01T00:00:00Z"}}, uuid.NewString())
	if err != nil || !strings.Contains(msg, "That time has passed") {
		t.Fatalf("answer %q (%v)", msg, err)
	}
}

func TestAReminderTimeIsOnThePersonsClock(t *testing.T) {
	at, _ := time.Parse(time.RFC3339, "2026-10-13T09:30:00Z")
	if got := reminderWhen(inZone("America/New_York"), at); got != "13 Oct, 05:30" {
		t.Errorf("New York: %q", got)
	}
	if got := reminderWhen(context.Background(), at); got != "13 Oct, 09:30 UTC" {
		t.Errorf("no zone, UTC: %q", got)
	}
	offset, _ := time.Parse(time.RFC3339, "2026-10-13T15:00:00+05:30")
	if got := reminderWhen(context.Background(), offset); got != "13 Oct, 15:00 (UTC+05:30)" {
		t.Errorf("no zone, an offset: %q", got)
	}
}

// fakeCalendar stands in for the calendar's list, recording the window asked.
func fakeCalendar(t *testing.T, events []*dgraphStruct.DgraphEvent, asked *[2]time.Time) {
	t.Helper()
	prev := calendarEventsOf
	t.Cleanup(func() { calendarEventsOf = prev })
	calendarEventsOf = func(_ context.Context, _ *userModels.UserInfo, from, to *time.Time) ([]*dgraphStruct.DgraphEvent, error) {
		if asked != nil {
			asked[0], asked[1] = *from, *to
		}
		return events, nil
	}
}

func event(id, title string, start, end time.Time) *dgraphStruct.DgraphEvent {
	return &dgraphStruct.DgraphEvent{Uuid: id, Title: title, StartTime: &start, EndTime: &end}
}

// CAL-BUG-13 (amended): today's calendar is OneCamp's, with no Google link,
// on the reader's clock, and what is over is left out.
func TestTodaysCalendarIsOneCampsWithoutGoogle(t *testing.T) {
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC) // 14:30 in India
	allDayFrom, allDayTo := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	offsite := &dgraphStruct.DgraphEvent{Uuid: "gcal-offsite", Title: "Offsite", StartTime: &allDayFrom, EndTime: &allDayTo, AllDay: true, StartDate: "2026-10-13", EndDate: "2026-10-14"}
	var asked [2]time.Time
	fakeCalendar(t, []*dgraphStruct.DgraphEvent{
		event("standup", "Standup", now.Add(-time.Hour), now.Add(-30*time.Minute)), // over
		event("review", "Launch review", now.Add(time.Hour), now.Add(2*time.Hour)),
		event("late", "Late call", time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC), time.Date(2026, 10, 13, 21, 0, 0, 0, time.UTC)), // tomorrow in India
		offsite,
	}, &asked)
	user := &userModels.UserInfo{}

	day := calendarDay(WithReaderZone(context.Background(), "Asia/Kolkata"), user, now)
	if !asked[0].Equal(now) || !asked[1].Equal(time.Date(2026, 10, 13, 18, 30, 0, 0, time.UTC)) {
		t.Errorf("read %v..%v, want now to midnight in India", asked[0], asked[1])
	}
	if len(day) != 2 || day[0].Title != "Offsite" || day[1].Title != "Launch review" {
		t.Fatalf("today %+v, want the offsite then the review", day)
	}
	if day[0].Subtitle != "All day" || day[1].Subtitle != "Tue 15:30" || day[1].URL != "/app/calendar/event/review" || day[1].Source != "calendar" {
		t.Fatalf("items %+v", day)
	}

	// Without the reader's zone: the next 24 hours, in UTC, saying so.
	day = calendarDay(context.Background(), user, now)
	if !asked[1].Equal(now.Add(24 * time.Hour)) {
		t.Errorf("read to %v, want a day from now", asked[1])
	}
	if len(day) != 3 || day[1].Subtitle != "Tue 10:00 UTC" {
		t.Fatalf("today %+v", day)
	}
}

func TestWhatNeedsMeHoldsTheDaysMeetings(t *testing.T) {
	now := time.Now()
	fakeCalendar(t, []*dgraphStruct.DgraphEvent{event("review", "Launch review", now.Add(time.Hour), now.Add(2*time.Hour))}, nil)
	items := gatherCalendarAttentionItems(context.Background(), &userModels.UserInfo{})
	if len(items) != 1 || items[0].Kind != "Upcoming event" || items[0].Title != "Launch review" || items[0].Priority != prCalendar {
		t.Fatalf("items %+v, want the review as an upcoming event", items)
	}
}

// fakeBusy stands in for the calendar's busy engine.
func fakeBusy(t *testing.T, busy map[string]calendarBusiness.Busy, fail map[string]bool, except **dgraphStruct.DgraphEvent) {
	t.Helper()
	prev := readBusy
	t.Cleanup(func() { readBusy = prev })
	readBusy = func(_ context.Context, uid string, _, _ time.Time, opts calendarBusiness.BusyOptions) (calendarBusiness.Busy, error) {
		if except != nil {
			*except = opts.Except
		}
		if fail[uid] {
			return calendarBusiness.Busy{}, errors.New("dgraph down")
		}
		return busy[uid], nil
	}
}

// CAL-BUG-12: Find a time reads the calendar's one engine, Google included:
// an hour Google shows busy is never proposed.
func TestFindATimeNeverProposesAGoogleBusyHour(t *testing.T) {
	now := mondayUTC()
	googleBusy := calendarBusiness.Interval{Start: now.Add(9 * time.Hour), End: now.Add(12 * time.Hour)}
	fakeBusy(t, map[string]calendarBusiness.Busy{"me": {Intervals: []calendarBusiness.Interval{googleBusy}}}, nil, nil)
	resp := &adapter.ScheduleProposeResponse{Participants: []adapter.ScheduleParticipant{{UUID: "me", Name: "You"}}}
	proposeSlots(context.Background(), resp, []string{"me"}, now, 1, 60, time.UTC, 9, 18)
	if len(resp.Candidates) == 0 {
		t.Fatal("no candidates")
	}
	for _, c := range resp.Candidates {
		st, _ := time.Parse(time.RFC3339, c.Start)
		if st.Before(googleBusy.End) && st.Add(time.Hour).After(googleBusy.Start) {
			t.Fatalf("proposed %s, inside the Google meeting", c.Start)
		}
	}
	if resp.Note != "" {
		t.Errorf("note %q", resp.Note)
	}
}

// CAL-BUG-12: a calendar that can't be read is named in the answer and never
// counted free.
func TestACalendarThatCantBeReadIsNamedNotFree(t *testing.T) {
	fakeBusy(t, map[string]calendarBusiness.Busy{"me": {}, "maya": {GoogleUnread: true}}, map[string]bool{"jonas": true}, nil)
	resp := &adapter.ScheduleProposeResponse{Participants: []adapter.ScheduleParticipant{{UUID: "me", Name: "You"}, {UUID: "maya", Name: "Maya"}, {UUID: "jonas", Name: "Jonas"}}}
	proposeSlots(context.Background(), resp, []string{"me", "maya", "jonas"}, mondayUTC(), 1, 60, time.UTC, 9, 18)
	for _, want := range []string{"Couldn't read Maya's calendar", "Couldn't read Jonas's calendar"} {
		if !strings.Contains(resp.Note, want) {
			t.Errorf("note %q lacks %q", resp.Note, want)
		}
	}
	if len(resp.Candidates) == 0 {
		t.Fatal("no candidates")
	}
	for _, c := range resp.Candidates {
		if c.AllFree || c.FreeCount != 1 || c.Total != 3 {
			t.Fatalf("candidate %+v counts an unread calendar free", c)
		}
	}
}

func TestRescheduleLeavesTheEventItselfOut(t *testing.T) {
	var except *dgraphStruct.DgraphEvent
	fakeBusy(t, map[string]calendarBusiness.Busy{}, nil, &except)
	moving := &dgraphStruct.DgraphEvent{Uuid: "review"}
	gatherFreeBusy(context.Background(), []string{"me"}, time.Now(), time.Now().Add(time.Hour), moving)
	if except != moving {
		t.Fatal("the event being moved wasn't left out of busy times")
	}
}

// CAL-BUG-12: business hours are the zone's wall clock, across a
// daylight-saving change too. A fixed offset put them an hour off after it.
func TestBusinessHoursFollowTheZoneAcrossDaylightSaving(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// Friday 30 Oct 2026; New York leaves daylight saving on Sunday 1 Nov.
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	got := computeCandidateSlots(now, 5, 60, ny, 9, 18, 1000, map[string][]busyInterval{"me": nil}, nil, []string{"me"})
	starts := map[string]bool{}
	for _, c := range got {
		st, _ := time.Parse(time.RFC3339, c.Start)
		local := st.In(ny)
		if local.Hour() < 9 || local.Hour()*60+local.Minute()+60 > 18*60 {
			t.Fatalf("%s is %s in New York, outside 9 to 6", c.Start, local.Format("Mon 15:04"))
		}
		starts[c.Start] = true
	}
	if !starts["2026-10-30T13:00:00Z"] || !starts["2026-11-02T14:00:00Z"] {
		t.Fatal("9:00 in New York is 13:00 UTC on Friday and 14:00 UTC on Monday")
	}
	if starts["2026-11-02T13:00:00Z"] {
		t.Fatal("8:00 on Monday, after the change, was proposed")
	}
}

func TestScheduleZoneIsTheAsksZone(t *testing.T) {
	if got := scheduleZone(context.Background(), "Asia/Kolkata", -240); got.String() != "Asia/Kolkata" {
		t.Errorf("request zone: %s", got)
	}
	if got := scheduleZone(inZone("Europe/London"), "", -240); got.String() != "Europe/London" {
		t.Errorf("told zone: %s", got)
	}
	if _, off := time.Date(2026, 1, 1, 0, 0, 0, 0, scheduleZone(context.Background(), "Not/AZone", 330)).Zone(); off != 330*60 {
		t.Errorf("fallback offset %d", off)
	}
	// "Local" is the server's zone, nobody's: the request's offset wins.
	if _, off := time.Date(2026, 1, 1, 0, 0, 0, 0, scheduleZone(context.Background(), "Local", -240)).Zone(); off != -240*60 {
		t.Errorf("\"Local\" was taken as a zone: offset %d", off)
	}
}

// CAL-BUG-13: the assistant invites only the teammate a name plainly means.
// An invitation shows someone the event, so "Sam" never invites Samantha.
func TestAnInviteGoesOnlyToWhomTheNameMeans(t *testing.T) {
	samantha := &dgraphStruct.DgraphUser{Uuid: "1", UserName: "Samantha Lee", Handle: "samantha"}
	maya := &dgraphStruct.DgraphUser{Uuid: "2", UserName: "Maya Patel", Handle: "maya"}
	mayaR := &dgraphStruct.DgraphUser{Uuid: "3", UserName: "Maya Rao", Handle: "mrao"}
	james := &dgraphStruct.DgraphUser{Uuid: "4", UserName: "James Holt", Handle: "james"}
	ravi := &dgraphStruct.DgraphUser{Uuid: "5", UserName: "Ravi Kumar", Handle: "rkumar"}
	raviS := &dgraphStruct.DgraphUser{Uuid: "6", UserName: "Ravi Shah", Handle: "rshah"}
	cases := []struct {
		found []*dgraphStruct.DgraphUser
		name  string
		want  *dgraphStruct.DgraphUser
	}{
		{[]*dgraphStruct.DgraphUser{samantha}, "Sam", nil},
		{[]*dgraphStruct.DgraphUser{james}, "me", nil},
		{[]*dgraphStruct.DgraphUser{maya, mayaR}, "maya", maya},      // her handle settles it
		{[]*dgraphStruct.DgraphUser{maya, mayaR}, "Maya Rao", mayaR}, // a whole name
		{[]*dgraphStruct.DgraphUser{ravi, raviS}, "Ravi", nil},       // two first names, neither a handle
		{[]*dgraphStruct.DgraphUser{mayaR}, "maya", mayaR},           // a first name, one person
	}
	for _, c := range cases {
		if got := personNamed(c.found, strings.TrimSpace(c.name), true); got != c.want {
			t.Errorf("%q in %d people: got %v, want %v", c.name, len(c.found), got, c.want)
		}
	}
	if !isAskerRef(" Me ") || isAskerRef("Mel") {
		t.Error("the asker is \"me\", and only that")
	}
}
