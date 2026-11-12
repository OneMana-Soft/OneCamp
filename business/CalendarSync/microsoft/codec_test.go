package microsoft

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	ICS "github.com/akashc777/OneCamp/business/ICS"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func graphOf(t *testing.T, raw string) graphEvent {
	t.Helper()
	var g graphEvent
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// Each Graph event reads as the codec's event: zones by their Windows names,
// all-day dates, a series' pattern as its rule, guests with their answers,
// free time, reminders, a cancelled event and an HTML body as text.
func TestAGraphEventReadsAsTheCodecsEvent(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		zone string
		want ICS.Event
	}{
		{"a timed event in a Windows zone", `{"id":"e1","iCalUId":"uid-1","subject":"Plan","changeKey":"k1",
			"start":{"dateTime":"2026-10-20T08:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-10-20T09:00:00.0000000","timeZone":"UTC"},
			"originalStartTimeZone":"W. Europe Standard Time","showAs":"busy","lastModifiedDateTime":"2026-10-19T10:00:00Z"}`, "",
			ICS.Event{UID: "uid-1", Summary: "Plan", Start: at("2026-10-20T08:00:00Z"), End: at("2026-10-20T09:00:00Z"), Zone: "Europe/Berlin", Stamp: at("2026-10-19T10:00:00Z")}},
		{"an all-day event keeps its dates", `{"id":"e2","iCalUId":"uid-2","subject":"Offsite","isAllDay":true,
			"start":{"dateTime":"2026-10-22T00:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-10-24T00:00:00.0000000","timeZone":"UTC"},"showAs":"free"}`, "Europe/London",
			ICS.Event{UID: "uid-2", Summary: "Offsite", StartDate: "2026-10-22", EndDate: "2026-10-24", Transparent: true}},
		{"no zone named falls back to the calendar's", `{"id":"e3","iCalUId":"uid-3","subject":"Call",
			"start":{"dateTime":"2026-10-20T15:30:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-20T16:00:00","timeZone":"UTC"},"originalStartTimeZone":"tzone://Microsoft/Custom"}`, "Asia/Kolkata",
			ICS.Event{UID: "uid-3", Summary: "Call", Start: at("2026-10-20T15:30:00Z"), End: at("2026-10-20T16:00:00Z"), Zone: "Asia/Kolkata"}},
		{"guests, their answers, a reminder, a room left out", `{"id":"e4","iCalUId":"uid-4","subject":"Review",
			"start":{"dateTime":"2026-10-20T10:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-20T11:00:00","timeZone":"UTC"},"originalStartTimeZone":"UTC",
			"organizer":{"emailAddress":{"name":"Ana","address":"ana@contoso.com"}},
			"attendees":[{"type":"required","status":{"response":"accepted"},"emailAddress":{"name":"Bo","address":"bo@contoso.com"}},
				{"type":"optional","status":{"response":"tentativelyAccepted"},"emailAddress":{"address":"cy@contoso.com"}},
				{"type":"required","status":{"response":"declined"},"emailAddress":{"address":"di@contoso.com"}},
				{"type":"required","status":{"response":"notResponded"},"emailAddress":{"address":"ed@contoso.com"}},
				{"type":"resource","status":{"response":"accepted"},"emailAddress":{"name":"Room 4","address":"room4@contoso.com"}}],
			"isReminderOn":true,"reminderMinutesBeforeStart":15,"location":{"displayName":"Room 4"},
			"onlineMeeting":{"joinUrl":"https://teams.microsoft.com/l/meetup-join/x"}}`, "",
			ICS.Event{UID: "uid-4", Summary: "Review", Start: at("2026-10-20T10:00:00Z"), End: at("2026-10-20T11:00:00Z"), Zone: "UTC", Location: "Room 4",
				Organizer: ICS.Address{Name: "Ana", Email: "ana@contoso.com"},
				Attendees: []ICS.Attendee{{Address: ICS.Address{Name: "Bo", Email: "bo@contoso.com"}, PartStat: ICS.Accepted},
					{Address: ICS.Address{Email: "cy@contoso.com"}, Optional: true, PartStat: ICS.Tentative},
					{Address: ICS.Address{Email: "di@contoso.com"}, PartStat: ICS.Declined},
					{Address: ICS.Address{Email: "ed@contoso.com"}, PartStat: ICS.NeedsAction}},
				Alarms: []ICS.Alarm{{Before: 15 * time.Minute}}, Conference: "https://teams.microsoft.com/l/meetup-join/x"}},
		{"a weekly series with an end date and a cancelled occurrence", `{"id":"m1","iCalUId":"uid-m1","type":"seriesMaster","subject":"Standup",
			"start":{"dateTime":"2026-10-19T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-19T09:15:00","timeZone":"UTC"},"originalStartTimeZone":"GMT Standard Time",
			"recurrence":{"pattern":{"type":"weekly","interval":1,"daysOfWeek":["monday","wednesday"],"firstDayOfWeek":"monday"},
				"range":{"type":"endDate","startDate":"2026-10-19","endDate":"2026-12-31","recurrenceTimeZone":"GMT Standard Time"}},
			"cancelledOccurrences":["OID.m1.2026-10-28"]}`, "",
			ICS.Event{UID: "uid-m1", Summary: "Standup", Start: at("2026-10-19T09:00:00Z"), End: at("2026-10-19T09:15:00Z"), Zone: "Europe/London",
				RRule: "FREQ=WEEKLY;INTERVAL=1;BYDAY=MO,WE;WKST=MO;UNTIL=20261231T235959Z", ExDates: []time.Time{at("2026-10-28T10:00:00Z")}}},
		{"a monthly series on the second Tuesday, ten times", `{"id":"m2","iCalUId":"uid-m2","type":"seriesMaster","subject":"Board",
			"start":{"dateTime":"2026-11-10T14:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-10T15:00:00","timeZone":"UTC"},"originalStartTimeZone":"UTC",
			"recurrence":{"pattern":{"type":"relativeMonthly","interval":1,"daysOfWeek":["tuesday"],"index":"second"},
				"range":{"type":"numbered","startDate":"2026-11-10","numberOfOccurrences":10}}}`, "",
			ICS.Event{UID: "uid-m2", Summary: "Board", Start: at("2026-11-10T14:00:00Z"), End: at("2026-11-10T15:00:00Z"), Zone: "UTC",
				RRule: "FREQ=MONTHLY;INTERVAL=1;BYDAY=TU;BYSETPOS=2;COUNT=10"}},
		{"a yearly all-day series with no end", `{"id":"m3","iCalUId":"uid-m3","type":"seriesMaster","subject":"Anniversary","isAllDay":true,
			"start":{"dateTime":"2026-03-14T00:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-03-15T00:00:00","timeZone":"UTC"},
			"recurrence":{"pattern":{"type":"absoluteYearly","interval":1,"month":3,"dayOfMonth":14},"range":{"type":"noEnd","startDate":"2026-03-14"}},
			"cancelledOccurrences":["OID.m3.2027-03-14"]}`, "",
			ICS.Event{UID: "uid-m3", Summary: "Anniversary", StartDate: "2026-03-14", EndDate: "2026-03-15",
				RRule: "FREQ=YEARLY;INTERVAL=1;BYMONTH=3;BYMONTHDAY=14", ExDates: []time.Time{at("2027-03-14T00:00:00Z")}}},
		{"an exception carries its original start", `{"id":"x1","iCalUId":"uid-x1","type":"exception","seriesMasterId":"m1","subject":"Standup (late)",
			"start":{"dateTime":"2026-11-02T10:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-02T10:15:00","timeZone":"UTC"},"originalStartTimeZone":"GMT Standard Time",
			"originalStart":"2026-11-02T09:00:00Z"}`, "",
			ICS.Event{UID: "uid-x1", Summary: "Standup (late)", Start: at("2026-11-02T10:00:00Z"), End: at("2026-11-02T10:15:00Z"), Zone: "Europe/London",
				RecurrenceID: func() *time.Time { t := at("2026-11-02T09:00:00Z"); return &t }()}},
		{"a cancelled event and an HTML body as text", `{"id":"e5","iCalUId":"uid-5","subject":"Gone","isCancelled":true,
			"start":{"dateTime":"2026-10-20T10:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-20T11:00:00","timeZone":"UTC"},"originalStartTimeZone":"UTC",
			"body":{"contentType":"html","content":"<html><body><p>Bring the <b>numbers</b></p></body></html>"}}`, "",
			ICS.Event{UID: "uid-5", Summary: "Gone", Status: "CANCELLED", Description: "Bring the numbers", Start: at("2026-10-20T10:00:00Z"), End: at("2026-10-20T11:00:00Z"), Zone: "UTC"}},
		{"no iCalUId takes the id", `{"id":"e6","subject":"Bare","start":{"dateTime":"2026-10-20T10:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-20T10:30:00","timeZone":"UTC"},"originalStartTimeZone":"UTC",
			"body":{"contentType":"text","content":"plain words"}}`, "",
			ICS.Event{UID: "e6@outlook.com", Summary: "Bare", Description: "plain words", Start: at("2026-10-20T10:00:00Z"), End: at("2026-10-20T10:30:00Z"), Zone: "UTC"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := eventOf(graphOf(t, c.raw), c.zone)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// What OneCamp sends Graph: the zone by its Windows name, all-day dates at
// midnight, the rule as Graph's pattern, free time, a reminder, the notes as
// text, and no guests (Graph would email them; OneCamp tells them itself).
func TestTheCodecsEventIsWrittenAsGraphTakesIt(t *testing.T) {
	cases := []struct {
		name string
		ev   ICS.Event
		want string // the JSON Graph is sent
	}{
		{"a timed event in a zone", ICS.Event{UID: "u1", Summary: "Plan", Description: "agenda", Location: "Room 1", Start: at("2026-10-20T08:00:00Z"), End: at("2026-10-20T09:00:00Z"), Zone: "Europe/Berlin",
			Attendees: []ICS.Attendee{{Address: ICS.Address{Email: "bo@contoso.com"}}}, Alarms: []ICS.Alarm{{Before: 10 * time.Minute}}},
			`{"subject":"Plan","body":{"contentType":"text","content":"agenda"},"start":{"dateTime":"2026-10-20T10:00:00","timeZone":"W. Europe Standard Time"},"end":{"dateTime":"2026-10-20T11:00:00","timeZone":"W. Europe Standard Time"},"isAllDay":false,"location":{"displayName":"Room 1"},"showAs":"busy","sensitivity":"normal","isReminderOn":true,"reminderMinutesBeforeStart":10}`},
		{"an all-day free event", ICS.Event{Summary: "Offsite", StartDate: "2026-10-22", EndDate: "2026-10-24", Transparent: true},
			`{"subject":"Offsite","body":{"contentType":"text","content":""},"start":{"dateTime":"2026-10-22T00:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-24T00:00:00","timeZone":"UTC"},"isAllDay":true,"location":{"displayName":""},"showAs":"free","sensitivity":"normal","isReminderOn":false}`},
		{"a zone with no Windows name goes in UTC", ICS.Event{Summary: "Call", Start: at("2026-10-20T08:00:00Z"), End: at("2026-10-20T08:30:00Z"), Zone: "Mars/Olympus"},
			`{"subject":"Call","body":{"contentType":"text","content":""},"start":{"dateTime":"2026-10-20T08:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-20T08:30:00","timeZone":"UTC"},"isAllDay":false,"location":{"displayName":""},"showAs":"busy","sensitivity":"normal","isReminderOn":false}`},
		{"a weekly series until a day", ICS.Event{Summary: "Standup", Start: at("2026-10-19T09:00:00Z"), End: at("2026-10-19T09:15:00Z"), Zone: "Europe/London", RRule: "FREQ=WEEKLY;BYDAY=MO,WE;UNTIL=20261231T235959Z"},
			`{"subject":"Standup","body":{"contentType":"text","content":""},"start":{"dateTime":"2026-10-19T10:00:00","timeZone":"GMT Standard Time"},"end":{"dateTime":"2026-10-19T10:15:00","timeZone":"GMT Standard Time"},"isAllDay":false,"location":{"displayName":""},"showAs":"busy","sensitivity":"normal","isReminderOn":false,"recurrence":{"pattern":{"type":"weekly","interval":1,"daysOfWeek":["monday","wednesday"],"firstDayOfWeek":"monday"},"range":{"type":"endDate","startDate":"2026-10-19","endDate":"2026-12-31","recurrenceTimeZone":"GMT Standard Time"}}}`},
		{"a monthly series by day, counted", ICS.Event{Summary: "Pay", Start: at("2026-10-15T09:00:00Z"), End: at("2026-10-15T09:30:00Z"), Zone: "UTC", RRule: "FREQ=MONTHLY;INTERVAL=2;BYMONTHDAY=15;COUNT=6"},
			`{"subject":"Pay","body":{"contentType":"text","content":""},"start":{"dateTime":"2026-10-15T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-15T09:30:00","timeZone":"UTC"},"isAllDay":false,"location":{"displayName":""},"showAs":"busy","sensitivity":"normal","isReminderOn":false,"recurrence":{"pattern":{"type":"absoluteMonthly","interval":2,"dayOfMonth":15,"firstDayOfWeek":"monday"},"range":{"type":"numbered","startDate":"2026-10-15","numberOfOccurrences":6,"recurrenceTimeZone":"UTC"}}}`},
		{"the last Friday, for ever", ICS.Event{Summary: "Demo", Start: at("2026-10-30T16:00:00Z"), End: at("2026-10-30T17:00:00Z"), Zone: "UTC", RRule: "FREQ=MONTHLY;BYDAY=-1FR"},
			`{"subject":"Demo","body":{"contentType":"text","content":""},"start":{"dateTime":"2026-10-30T16:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-30T17:00:00","timeZone":"UTC"},"isAllDay":false,"location":{"displayName":""},"showAs":"busy","sensitivity":"normal","isReminderOn":false,"recurrence":{"pattern":{"type":"relativeMonthly","interval":1,"daysOfWeek":["friday"],"firstDayOfWeek":"monday","index":"last"},"range":{"type":"noEnd","startDate":"2026-10-30","recurrenceTimeZone":"UTC"}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, err := graphEventOf(c.ev)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(g)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != c.want {
				t.Fatalf("got  %s\nwant %s", b, c.want)
			}
		})
	}
}

// A rule Outlook can't repeat by is refused, so it waits rather than
// arriving as something else.
func TestARuleOutlookCantRepeatByIsRefused(t *testing.T) {
	for _, rule := range []string{"FREQ=HOURLY", "FREQ=MINUTELY;INTERVAL=30", "FREQ=DAILY;BYHOUR=9,17", "FREQ=MONTHLY;BYDAY=1MO,-1FR"} {
		_, err := graphEventOf(ICS.Event{Summary: "x", Start: at("2026-10-20T08:00:00Z"), End: at("2026-10-20T09:00:00Z"), Zone: "UTC", RRule: rule})
		if err == nil || !strings.Contains(err.Error(), "Outlook") {
			t.Fatalf("%s: %v", rule, err)
		}
	}
}

// Windows zone names read as IANA zones, IANA names pass, and anything else
// is no zone.
func TestWindowsZonesReadAsIANAZones(t *testing.T) {
	for in, want := range map[string]string{
		"Pacific Standard Time": "America/Los_Angeles", "India Standard Time": "Asia/Kolkata", "GMT Standard Time": "Europe/London",
		"W. Europe Standard Time": "Europe/Berlin", "Tokyo Standard Time": "Asia/Tokyo", "AUS Eastern Standard Time": "Australia/Sydney",
		"UTC": "UTC", "Coordinated Universal Time": "UTC", "Europe/Paris": "Europe/Paris", "tzone://Microsoft/Custom": "", "": "", "Nowhere Standard Time": "",
	} {
		if got := ianaZone(in); got != want {
			t.Errorf("ianaZone(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"America/Los_Angeles": "Pacific Standard Time", "Asia/Calcutta": "India Standard Time", "Asia/Kolkata": "India Standard Time", "UTC": "UTC", "": "UTC", "Mars/Olympus": ""} {
		if got := windowsZone(in); got != want {
			t.Errorf("windowsZone(%q) = %q, want %q", in, got, want)
		}
	}
}

// Outlook's sensitivity is the codec's CLASS both ways: private and
// confidential stay so; personal and normal are neither.
func TestSensitivityIsTheCodecsClass(t *testing.T) {
	for s, class := range map[string]string{"private": "PRIVATE", "confidential": "CONFIDENTIAL", "personal": "", "normal": "", "": ""} {
		g := graphOf(t, `{"id":"e1","type":"singleInstance","subject":"x","sensitivity":"`+s+`","start":{"dateTime":"2026-11-10T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-10T10:00:00","timeZone":"UTC"}}`)
		if got := eventOf(g, "UTC").Class; got != class {
			t.Errorf("sensitivity %q reads as %q, want %q", s, got, class)
		}
	}
	for class, s := range map[string]string{"PRIVATE": "private", "CONFIDENTIAL": "confidential", "PUBLIC": "normal", "": "normal"} {
		w, err := graphEventOf(ICS.Event{Summary: "x", Start: at("2026-11-10T09:00:00Z"), End: at("2026-11-10T10:00:00Z"), Zone: "UTC", Class: class})
		if err != nil || w.Sensitivity != s {
			t.Errorf("CLASS %q is sent as %q (%v), want %q", class, w.Sensitivity, err, s)
		}
	}
}
