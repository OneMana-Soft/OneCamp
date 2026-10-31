package Calendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	"github.com/akashc777/OneCamp/business/CalendarSync/icsurl"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// addresses are files published at addresses, read as icsurl reads them:
// whole, by their digest.
type addresses struct {
	files map[string]string
	reads int
	fail  error
}

func (a *addresses) read(address string) (ICS.Calendar, string, error) {
	a.reads++
	if a.fail != nil {
		return ICS.Calendar{}, "", a.fail
	}
	body, ok := a.files[address]
	if !ok {
		return ICS.Calendar{}, "", errors.New("There's no calendar at that address. Check it, or copy it again from where it's published.")
	}
	sum := sha256.Sum256([]byte(body))
	file, err := ICS.Read(strings.NewReader(body))
	return file, hex.EncodeToString(sum[:]), err
}

func (*addresses) Provider() string { return providerICS }
func (*addresses) Calendars(context.Context, calendarSyncBusiness.Link) ([]calendarSyncBusiness.Remote, error) {
	return nil, nil
}
func (a *addresses) Pull(_ context.Context, cal calendarSyncBusiness.Remote, cursor string) ([]calendarSyncBusiness.Change, string, error) {
	file, digest, err := a.read(cal.Source)
	if err != nil || digest == cursor {
		return nil, cursor, err
	}
	return icsurl.ChangesOf(file), digest, nil
}
func (*addresses) Push(context.Context, calendarSyncBusiness.Op) (calendarSyncBusiness.Ref, error) {
	return calendarSyncBusiness.Ref{}, icsurl.ErrReadOnly
}
func (*addresses) Watch(context.Context, calendarSyncBusiness.Remote) (calendarSyncBusiness.Watch, error) {
	return calendarSyncBusiness.Watch{}, errors.ErrUnsupported
}
func (*addresses) Stop(context.Context, calendarSyncBusiness.Watch) error {
	return errors.ErrUnsupported
}

func withAddresses(t *testing.T, h *harness, files map[string]string) (*addresses, *memLedger) {
	t.Helper()
	a := &addresses{files: files}
	ledger := newMemLedger(h.store, func() time.Time { return h.clock })
	h.s.ledger, h.s.moveNow = ledger, true
	prevProvider, prevRead := syncProvider, readAddress
	syncProvider = func(p string) (calendarSyncBusiness.Adapter, bool) { return a, p == providerICS }
	readAddress = func(_ context.Context, address string) (ICS.Calendar, string, error) { return a.read(address) }
	t.Cleanup(func() { syncProvider, readAddress = prevProvider, prevRead })
	return a, ledger
}

const teamAddress = "https://calendar.example/private-7rKq2mX9/team.ics"

// teamFile is a team's published calendar, stamped at stamp as feeds stamp
// every read: a weekly review with one week moved, a holiday, and a launch
// with a guest and an alarm, marked busy.
func teamFile(stamp, review string, withHoliday bool, extra ...string) string {
	lines := []string{"BEGIN:VCALENDAR", "VERSION:2.0", "X-WR-CALNAME:Team",
		"BEGIN:VEVENT", "UID:review", "DTSTAMP:" + stamp, "SUMMARY:" + review, "DTSTART:20261021T090000Z", "DTEND:20261021T100000Z", "RRULE:FREQ=WEEKLY;COUNT=6", "END:VEVENT",
		"BEGIN:VEVENT", "UID:review", "DTSTAMP:" + stamp, "RECURRENCE-ID:20261028T090000Z", "SUMMARY:" + review + " (moved)", "DTSTART:20261028T140000Z", "DTEND:20261028T150000Z", "END:VEVENT",
		"BEGIN:VEVENT", "UID:launch", "DTSTAMP:" + stamp, "SUMMARY:Launch", "DTSTART:20261104T130000Z", "DTEND:20261104T140000Z", "TRANSP:OPAQUE",
		"ATTENDEE;PARTSTAT=ACCEPTED:mailto:maya@example.com", "BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT30M", "END:VALARM", "END:VEVENT"}
	if withHoliday {
		lines = append(lines, "BEGIN:VEVENT", "UID:diwali", "DTSTAMP:"+stamp, "SUMMARY:Diwali", "DTSTART;VALUE=DATE:20261108", "DTEND;VALUE=DATE:20261109", "END:VEVENT")
	}
	lines = append(lines, extra...)
	return strings.Join(append(lines, "END:VCALENDAR", ""), "\r\n")
}

// onCalendar are the events on a calendar, by their copied UIDs' own part.
func onCalendar(h *harness, calendar uuid.UUID) map[string]*models.Event {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	out := map[string]*models.Event{}
	for _, e := range h.store.events {
		if e.CalendarID != calendar {
			continue
		}
		key := exportedUID(e.ICalUID)
		if e.SeriesID != nil {
			key += "@" + e.RecurrenceID.UTC().Format("0102T1504")
		}
		out[key] = copyEvent(e)
	}
	return out
}

// Calendar B8 (SY-14): an address is read once, and only a calendar is
// added: its events come into a calendar of the person's own, named as the
// file names itself, the address's copies (read only, never busy, no guests,
// no alarms), and only its host is shown; the address is kept sealed.
func TestASubscriptionReadsAnAddressIntoACalendarOfItsOwn(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	_, ledger := withAddresses(t, h, map[string]string{teamAddress: teamFile("20261020T100000Z", "Review", true)})
	v, err := h.s.Subscribe(context.Background(), sam, " webcal://calendar.example/private-7rKq2mX9/team.ics ", "", "lake")
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "Team" || v.Host != "calendar.example" || v.Events != 3 || v.Reading {
		t.Fatalf("view %+v", v)
	}
	cal := uuid.MustParse(v.CalendarID)
	if c := h.store.subscribed[cal]; c.Kind != kindSubscribed || *c.OwnerID != personOf(sam) || c.Hue != "lake" {
		t.Fatalf("calendar %+v", c)
	}
	evs := onCalendar(h, cal)
	if len(evs) != 4 || evs["review"] == nil || evs["review@1028T0900"] == nil || evs["launch"] == nil || evs["diwali"] == nil {
		t.Fatalf("events %v", evs)
	}
	for k, e := range evs {
		if e.Source != sourceSubscribed || e.Transparency != transparencyTransparent || !strings.HasSuffix(e.ICalUID, "~"+cal.String()) && e.SeriesID == nil {
			t.Errorf("%s: source %q, transparency %q, uid %q", k, e.Source, e.Transparency, e.ICalUID)
		}
		if len(h.store.guests[e.ID]) != 0 {
			t.Errorf("%s brought guests", k)
		}
	}
	if mins, _ := h.s.rem.remindersOf(context.Background(), evs["launch"].ID, []uuid.UUID{personOf(sam)}); len(mins[personOf(sam)]) != 0 {
		t.Errorf("the file's alarm rings: %v", mins)
	}
	if len(ledger.subs) != 1 || ledger.subs[0].Cursor == "" || strings.Contains(ledger.subs[0].SourceSealed, "calendar.example") || ledger.subs[0].SourceSealed == "" {
		t.Fatalf("ledger %+v", ledger.subs)
	}
	if len(ledger.refs) != 3 {
		t.Fatalf("refs %+v", ledger.refs)
	}
	cals, err := h.s.Calendars(context.Background(), sam)
	if err != nil {
		t.Fatal(err)
	}
	var listed *CalendarView
	for i := range cals {
		if cals[i].ID == v.CalendarID {
			listed = &cals[i]
		}
	}
	if listed == nil || listed.Kind != kindSubscribed || listed.Access != "view" || !listed.Mine || listed.Name != "Team" {
		t.Fatalf("listed %+v", listed)
	}
	subs, err := h.s.Subscriptions(context.Background(), sam)
	if err != nil || len(subs) != 1 || subs[0].Host != "calendar.example" || subs[0].Error != "" || subs[0].Reading {
		t.Fatalf("subscriptions %+v %v", subs, err)
	}
}

// Its events are the address's: the person sees them and changes none, adds
// nothing to the calendar, can't share it or give it a link of their own,
// and doesn't export it; they may rename and recolour it. Nobody else sees
// the calendar or its events, not even as busy.
func TestASubscribedCalendarIsReadOnlyAndItsPersonsAlone(t *testing.T) {
	h := newHarness(t)
	sam, maya := h.member("Sam"), h.member("Maya")
	withAddresses(t, h, map[string]string{teamAddress: teamFile("20261020T100000Z", "Review", true)})
	v, err := h.s.Subscribe(context.Background(), sam, teamAddress, "Team calendar", "")
	if err != nil {
		t.Fatal(err)
	}
	cal := uuid.MustParse(v.CalendarID)
	launch := onCalendar(h, cal)["launch"]
	if got, err := h.s.Get(context.Background(), sam, launch.ID.String()); err != nil || !got.ReadOnly {
		t.Errorf("Sam reads it: %+v %v", got, err)
	}
	seq := launch.Sequence
	title := "Mine now"
	if _, err := h.s.Update(context.Background(), sam, launch.ID.String(), EventPatch{Sequence: &seq, Title: &title}); !errors.Is(err, ErrSubscribedEvent) {
		t.Errorf("an update: %v", err)
	}
	if err := h.s.Delete(context.Background(), sam, launch.ID.String(), "", &seq); !errors.Is(err, ErrSubscribedEvent) {
		t.Errorf("a delete: %v", err)
	}
	start, end := timed(time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC), 30, "UTC")
	if _, err := h.s.Create(context.Background(), sam, EventInput{Title: "Added", Start: start, End: end, CalendarID: v.CalendarID}); err == nil || !strings.Contains(err.Error(), "can't add events") {
		t.Errorf("an event added to it: %v", err)
	}
	if err := h.s.Share(context.Background(), sam, v.CalendarID, personOf(maya).String(), models.ShareDetails); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("a share: %v", err)
	}
	if _, err := h.s.MakeFeed(context.Background(), sam, v.CalendarID, models.FeedFull); !errors.Is(err, errFeedOfSubscribed) {
		t.Errorf("a link of their own: %v", err)
	}
	name, hue := "Holidays and the team", "berry"
	if err := h.s.UpdateCalendar(context.Background(), sam, v.CalendarID, CalendarPatch{Name: &name, Hue: &hue}); err != nil {
		t.Errorf("rename and recolour: %v", err)
	}
	var out strings.Builder
	if n, err := h.s.Export(context.Background(), sam, &out); err != nil || n != 0 || strings.Contains(out.String(), "Launch") {
		t.Errorf("export: %d %v", n, err)
	}
	// Sam sees it whole; Maya, a member who sees when Sam is busy, sees none of it.
	if d, _ := h.s.decideMany(context.Background(), subjectOf(sam), []string{launch.ID.String()}); d[launch.ID.String()].Level != accessBusiness.LevelView || d[launch.ID.String()].Owner {
		t.Errorf("Sam's decision %+v", d)
	}
	if d, _ := h.s.decideMany(context.Background(), subjectOf(maya), []string{launch.ID.String()}); d[launch.ID.String()].Level != accessBusiness.LevelNone {
		t.Errorf("Maya's decision %+v", d)
	}
	if d, _ := h.s.decideCalendars(context.Background(), subjectOf(maya), []string{v.CalendarID}); d[v.CalendarID].Level != accessBusiness.LevelNone {
		t.Errorf("Maya sees the calendar: %+v", d)
	}
	evs, _, err := h.s.CalendarEvents(context.Background(), maya, []string{v.CalendarID}, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || len(evs) != 0 {
		t.Errorf("Maya reads %d events, %v", len(evs), err)
	}
}

// Read again every 15 minutes: a changed event changes in place (its id
// kept), an instance moved back is the rule's again, what left the file
// goes, and what's new comes; a file unchanged changes nothing; a read not
// yet due, or a Refresh now within the minute, reads nothing.
func TestASubscriptionKeepsInStepWithItsAddress(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	files := map[string]string{teamAddress: teamFile("20261020T100000Z", "Review", true)}
	a, ledger := withAddresses(t, h, files)
	v, err := h.s.Subscribe(context.Background(), sam, teamAddress, "", "")
	if err != nil {
		t.Fatal(err)
	}
	cal := uuid.MustParse(v.CalendarID)
	before := onCalendar(h, cal)
	reads := a.reads

	// Stamped anew, nothing else changed: read, and nothing written.
	h.clock = h.clock.Add(16 * time.Minute)
	files[teamAddress] = teamFile("20261020T113000Z", "Review", true)
	writes := h.store.writes
	if err := h.s.readSubscriptions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.reads != reads+1 || h.store.writes != writes {
		t.Fatalf("restamped: %d reads, %d writes", a.reads-reads, h.store.writes-writes)
	}

	// Changed: the review renamed with its week moved back, the holiday
	// gone, a new event.
	h.clock = h.clock.Add(16 * time.Minute)
	files[teamAddress] = strings.Replace(teamFile("20261020T120000Z", "Weekly review", false,
		"BEGIN:VEVENT", "UID:offsite", "DTSTAMP:20261020T120000Z", "SUMMARY:Offsite", "DTSTART;VALUE=DATE:20261112", "DTEND;VALUE=DATE:20261114", "END:VEVENT"),
		"BEGIN:VEVENT\r\nUID:review\r\nDTSTAMP:20261020T120000Z\r\nRECURRENCE-ID:20261028T090000Z\r\nSUMMARY:Weekly review (moved)\r\nDTSTART:20261028T140000Z\r\nDTEND:20261028T150000Z\r\nEND:VEVENT\r\n", "", 1)
	if err := h.s.readSubscriptions(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := onCalendar(h, cal)
	if after["review"] == nil || after["review"].ID != before["review"].ID || after["review"].Title != "Weekly review" {
		t.Fatalf("review %+v", after["review"])
	}
	if after["review@1028T0900"] != nil || after["diwali"] != nil || after["offsite"] == nil || after["launch"].ID != before["launch"].ID {
		t.Fatalf("after %v", after)
	}
	if refs, _ := ledger.refsOf(context.Background(), ledger.subs[0].LinkID); len(refs) != 3 {
		t.Fatalf("refs %+v", refs)
	}

	// Not due for 15 minutes; Refresh now reads at most once a minute.
	reads = a.reads
	h.clock = h.clock.Add(5 * time.Minute)
	_ = h.s.readSubscriptions(context.Background())
	if a.reads != reads {
		t.Fatal("read before it was due")
	}
	if _, err := h.s.RefreshSubscription(context.Background(), sam, v.CalendarID); err != nil || a.reads != reads+1 {
		t.Fatalf("refresh: %v, %d reads", err, a.reads-reads)
	}
	h.clock = h.clock.Add(20 * time.Second)
	if _, err := h.s.RefreshSubscription(context.Background(), sam, v.CalendarID); err != nil || a.reads != reads+1 {
		t.Fatalf("refresh within the minute: %v, %d reads", err, a.reads-reads)
	}
}

// What can't be read says why, in words, and nothing is made; the same
// address twice is one calendar; a read that fails later keeps the events
// and says why for its person to see.
func TestASubscriptionThatCantBeReadSaysWhy(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	a, ledger := withAddresses(t, h, map[string]string{teamAddress: teamFile("20261020T100000Z", "Review", true)})
	if _, err := h.s.Subscribe(context.Background(), sam, "https://calendar.example/nothing.ics", "", ""); err == nil || !strings.Contains(err.Error(), "no calendar at that address") {
		t.Fatalf("a missing address: %v", err)
	}
	if _, err := h.s.Subscribe(context.Background(), sam, "http://127.0.0.1/team.ics", "", ""); err == nil || !strings.Contains(err.Error(), "private network") {
		t.Fatalf("a local address: %v", err)
	}
	if len(h.store.subscribed) != 0 || len(ledger.subs) != 0 || a.reads != 1 {
		t.Fatalf("made %d calendars, %d subscriptions, %d reads", len(h.store.subscribed), len(ledger.subs), a.reads)
	}
	v, err := h.s.Subscribe(context.Background(), sam, teamAddress, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Subscribe(context.Background(), sam, "webcal://calendar.example/private-7rKq2mX9/team.ics", "", ""); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("the same address again: %v", err)
	}
	a.fail = errors.New("The address took too long to answer.")
	h.clock = h.clock.Add(16 * time.Minute)
	_ = h.s.readSubscriptions(context.Background())
	subs, _ := h.s.Subscriptions(context.Background(), sam)
	if len(subs) != 1 || subs[0].Error != "The address took too long to answer." || len(onCalendar(h, uuid.MustParse(v.CalendarID))) != 4 {
		t.Fatalf("after a failed read %+v", subs)
	}
}

// Removing a subscription removes its calendar and everything on it, and
// the address can be added again.
func TestUnsubscribingRemovesTheCalendarAndEverythingOnIt(t *testing.T) {
	h := newHarness(t)
	sam, maya := h.member("Sam"), h.member("Maya")
	_, ledger := withAddresses(t, h, map[string]string{teamAddress: teamFile("20261020T100000Z", "Review", true)})
	v, err := h.s.Subscribe(context.Background(), sam, teamAddress, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.Unsubscribe(context.Background(), maya, v.CalendarID); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("someone else removes it: %v", err)
	}
	if err := h.s.Unsubscribe(context.Background(), sam, v.CalendarID); err != nil {
		t.Fatal(err)
	}
	if len(onCalendar(h, uuid.MustParse(v.CalendarID))) != 0 || len(ledger.subs) != 0 || len(ledger.refs) != 0 || len(h.store.subscribed) != 0 {
		t.Fatalf("left %d subscriptions, %d refs", len(ledger.subs), len(ledger.refs))
	}
	if _, err := h.s.Subscribe(context.Background(), sam, teamAddress, "", ""); err != nil {
		t.Fatalf("added again: %v", err)
	}
}

// A read whose file changed many events reads them here once, and keeps
// where they are once, after their writes; each is changed in place.
func TestAChangedFileReadsItsEventsAndKeepsTheirRefsOnce(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	file := func(stamp, suffix string) string {
		lines := []string{"BEGIN:VCALENDAR", "VERSION:2.0", "X-WR-CALNAME:Team"}
		for i, uid := range []string{"standup", "launch", "retro"} {
			day := fmt.Sprintf("202611%02d", 3+i)
			lines = append(lines, "BEGIN:VEVENT", "UID:"+uid, "DTSTAMP:"+stamp, "SUMMARY:"+uid+suffix,
				"DTSTART:"+day+"T090000Z", "DTEND:"+day+"T100000Z", "END:VEVENT")
		}
		return strings.Join(append(lines, "END:VCALENDAR", ""), "\r\n")
	}
	files := map[string]string{teamAddress: file("20261020T100000Z", "")}
	_, ledger := withAddresses(t, h, files)
	v, err := h.s.Subscribe(context.Background(), sam, teamAddress, "", "")
	if err != nil {
		t.Fatal(err)
	}
	cal := uuid.MustParse(v.CalendarID)
	before := onCalendar(h, cal)

	h.clock = h.clock.Add(16 * time.Minute)
	files[teamAddress] = file("20261020T120000Z", " (moved)")
	h.store.resetReads()
	ledger.mu.Lock()
	ledger.keepCalls = 0
	ledger.mu.Unlock()
	if err := h.s.readSubscriptions(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := onCalendar(h, cal)
	for _, uid := range []string{"standup", "launch", "retro"} {
		if after[uid] == nil || after[uid].ID != before[uid].ID || after[uid].Title != uid+" (moved)" {
			t.Fatalf("%s: %+v", uid, after[uid])
		}
	}
	if n := h.store.readsOf("get"); n != 0 {
		t.Fatalf("the changed events were read one at a time: %d reads", n)
	}
	ledger.mu.Lock()
	keeps := ledger.keepCalls
	ledger.mu.Unlock()
	if keeps != 1 {
		t.Fatalf("where they are was kept %d times, want once", keeps)
	}
}
