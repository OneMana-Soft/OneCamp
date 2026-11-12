package Calendar

import (
	"context"
	"errors"
	"testing"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// synced is a row of the ledger for a calendar moved from provider.
func synced(provider string, readOnly bool) models.SyncCalendar {
	return models.SyncCalendar{Provider: provider, RemoteID: provider + "-cal", ReadOnly: readOnly}
}

// V-594: a calendar says where its events come from and whether its provider
// lets OneCamp only read it. Only a calendar a move made for one of the
// provider's calendars takes the provider's word: a person's own calendar
// holds their primary's events and stays OneCamp's, and one a file brought
// has no provider at all.
func TestACalendarSaysWhereItComesFromAndWhetherItIsReadOnly(t *testing.T) {
	for _, c := range []struct {
		name string
		kind string
		rows []models.SyncCalendar
		want calendarOrigin
	}{
		{"see-only at Google", kindExternal, []models.SyncCalendar{synced("google", true)}, calendarOrigin{"google", true}},
		{"writable at Google", kindExternal, []models.SyncCalendar{synced("google", false)}, calendarOrigin{"google", false}},
		{"see-only at Outlook", kindExternal, []models.SyncCalendar{synced("microsoft", true)}, calendarOrigin{"microsoft", true}},
		{"writable at Outlook", kindExternal, []models.SyncCalendar{synced("microsoft", false)}, calendarOrigin{"microsoft", false}},
		// One name moved from both: a push to the see-only one is refused.
		{"writable at Google, see-only at Outlook", kindExternal, []models.SyncCalendar{synced("google", false), synced("microsoft", true)}, calendarOrigin{"microsoft", true}},
		{"see-only at Google, writable at Outlook", kindExternal, []models.SyncCalendar{synced("google", true), synced("microsoft", false)}, calendarOrigin{"google", true}},
		{"a file imported", kindExternal, nil, calendarOrigin{"onecamp", false}},
		{"read from an address", kindSubscribed, []models.SyncCalendar{synced("ics", false)}, calendarOrigin{"ics", true}},
		{"one's own, with the primary moved in", "personal", []models.SyncCalendar{synced("google", true)}, calendarOrigin{"onecamp", false}},
		{"a project's", "project", nil, calendarOrigin{"onecamp", false}},
	} {
		if got := originOf(c.kind, c.rows); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// movedCalendars gives who a calendar moved from Google that is see-only
// there, one moved from Google they may write to, and one moved from Outlook
// that is see-only there, as a move leaves them in the ledger.
func movedCalendars(t *testing.T, h *harness, who *model.UserInfo) (ledger *memLedger, seeOnly, writable, outlook uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	ledger = h.s.ledger.(*memLedger)
	for _, m := range []struct {
		provider, name string
		readOnly       bool
		into           *uuid.UUID
	}{{"google", "Holidays in India", true, &seeOnly}, {"google", "Team offsites", false, &writable}, {"microsoft", "Board", true, &outlook}} {
		link, err := ledger.ensureLink(ctx, personOf(who), m.provider)
		if err != nil {
			t.Fatal(err)
		}
		cal, err := h.store.importedCalendar(ctx, personOf(who), m.name, "UTC")
		if err != nil {
			t.Fatal(err)
		}
		*m.into = cal
		if err := ledger.keepCalendar(ctx, models.SyncCalendar{LinkID: link.ID, RemoteID: m.name + "@group", Name: m.name, CalendarID: &cal, Cursor: "c1", ReadOnly: m.readOnly}); err != nil {
			t.Fatal(err)
		}
	}
	return ledger, seeOnly, writable, outlook
}

// The calendar list says each calendar's source and whether it is read-only,
// from one read of the ledger for the whole list.
func TestTheCalendarListSaysWhereEachCalendarComesFrom(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	ctx := context.Background()
	ledger, seeOnly, writable, outlook := movedCalendars(t, h, sam)
	file, err := h.store.importedCalendar(ctx, personOf(sam), "From a file", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	address := uuid.New()
	if err := h.store.subscribedCalendar(ctx, address, personOf(sam), "Cricket", "", "UTC"); err != nil {
		t.Fatal(err)
	}
	own, _ := h.store.personalCalendar(ctx, personOf(sam), "UTC")
	ledger.intoCalls = 0
	list, err := h.s.Calendars(ctx, sam)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]calendarOrigin{}
	for _, v := range list {
		got[uuid.MustParse(v.ID)] = calendarOrigin{v.Source, v.ReadOnly}
	}
	want := map[uuid.UUID]calendarOrigin{
		seeOnly: {"google", true}, writable: {"google", false}, outlook: {"microsoft", true},
		file: {"onecamp", false}, address: {"ics", true}, own: {"onecamp", false},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("calendar %s: %+v, want %+v", id, got[id], w)
		}
	}
	if ledger.intoCalls != 1 {
		t.Errorf("the list read the ledger %d times, want once for the whole list", ledger.intoCalls)
	}
}

// A new event on a calendar its provider lets OneCamp only read is refused
// before anything is written, in words naming the provider; one on a
// calendar the provider takes is made. A sharee who may edit the calendar in
// OneCamp is refused the same: the provider's rights only ever narrow.
func TestANewEventOnACalendarSeeOnlyAtItsProviderIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	h := newHarness(t)
	sam, ria := h.member("Sam"), h.member("Ria")
	ctx := context.Background()
	_, seeOnly, writable, outlook := movedCalendars(t, h, sam)
	if err := h.s.Share(ctx, sam, seeOnly.String(), personOf(ria).String(), models.ShareEdit); err != nil {
		t.Fatal(err)
	}
	start := h.clock.Add(24 * time.Hour)
	on := func(cal uuid.UUID) EventInput {
		end := start.Add(time.Hour)
		return EventInput{Title: "Lunch", Start: When{At: &start, Zone: "UTC"}, End: When{At: &end, Zone: "UTC"}, CalendarID: cal.String()}
	}
	for _, c := range []struct {
		who  *model.UserInfo
		cal  uuid.UUID
		want string
	}{
		{sam, seeOnly, "This calendar is read-only in Google. Put the event on another calendar."},
		{sam, outlook, "This calendar is read-only in Outlook. Put the event on another calendar."},
		{ria, seeOnly, "This calendar is read-only in Google. Put the event on another calendar."},
	} {
		before := len(h.store.events)
		_, err := h.s.Create(ctx, c.who, on(c.cal))
		var readOnly *CalendarReadOnlyError
		if !errors.As(err, &readOnly) || err.Error() != c.want {
			t.Errorf("%s on %s: %v, want %q", c.who.UserDgraphInfo.UserName, c.cal, err, c.want)
		}
		if len(h.store.events) != before {
			t.Errorf("%s on %s: an event was written before the refusal", c.who.UserDgraphInfo.UserName, c.cal)
		}
	}
	made, err := h.s.Create(ctx, sam, on(writable))
	if err != nil || made.CalendarID != writable.String() {
		t.Fatalf("on the writable calendar: %+v, %v", made, err)
	}
}
