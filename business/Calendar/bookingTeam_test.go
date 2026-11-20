package Calendar

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	"github.com/google/uuid"
)

func teamInput(mode string, hosts ...string) PageInput {
	return PageInput{Slug: "team-call", Title: "Team call", DurationMinutes: 30, MaxDaysAhead: 30, Active: true,
		Hours: WorkingHours{Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00", TZ: "UTC"}, TeamMode: mode, Hosts: hosts}
}

// gcal-7 H4: a page is solo unless it says collective; a collective page has
// up to nine hosts besides its owner, each named once; round robin is the next
// head's, and refused until then.
func TestATeamPageIsCheckedForItsHosts(t *testing.T) {
	now := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	p, err := CheckPage(teamInput(""), now)
	if err != nil || p.TeamMode != "solo" || len(p.HostIds) != 0 {
		t.Fatalf("no mode: %q %v %v; want solo, no hosts", p.TeamMode, p.HostIds, err)
	}
	a, b := uuid.New(), uuid.New()
	p, err = CheckPage(teamInput("collective", a.String(), " "+b.String()+" ", a.String()), now)
	if err != nil || p.TeamMode != "collective" || !reflect.DeepEqual(p.HostIds, []uuid.UUID{a, b}) {
		t.Fatalf("collective: %q %v %v; want collective with %s and %s once each", p.TeamMode, p.HostIds, err, a, b)
	}
	nine := []string{}
	for range 9 {
		nine = append(nine, uuid.NewString())
	}
	if p, err := CheckPage(teamInput("collective", nine...), now); err != nil || len(p.HostIds) != 9 {
		t.Fatalf("nine hosts: %d %v; want them kept", len(p.HostIds), err)
	}
	refused := map[string]PageInput{
		"ten hosts":               teamInput("collective", append(nine, uuid.NewString())...),
		"a solo page with hosts":  teamInput("solo", a.String()),
		"round robin (H4b)":       teamInput("round_robin", a.String()),
		"a mode of its own":       teamInput("party", a.String()),
		"a host who isn't a uuid": teamInput("collective", "sam@acme.test"),
	}
	for what, in := range refused {
		var ae *AvailabilityError
		if _, err := CheckPage(in, now); !errors.As(err, &ae) || ae.Msg == "" {
			t.Errorf("%s: %v; want a plain refusal", what, err)
		}
	}
}

// The owner is the page's: never one of its hosts. Refused before anything is
// read or written.
func TestTheOwnerIsNoHostOfTheirOwnPage(t *testing.T) {
	owner := uuid.New()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reached the database: %v", r)
		}
	}()
	_, err := SaveBookingPage(context.Background(), owner, teamInput("collective", uuid.NewString(), owner.String()))
	var ae *AvailabilityError
	if !errors.As(err, &ae) {
		t.Fatalf("owner as host: %v; want a plain refusal", err)
	}
}

// The web's editor sends no hosts: an update that leaves them out (nil, not
// []) keeps the page's host rows, unless it makes a team page solo; a create
// has none to keep.
func TestAnUpdateWithoutHostsKeepsThem(t *testing.T) {
	team, solo := &bookingModel.Page{TeamMode: teamCollective}, &bookingModel.Page{TeamMode: teamSolo}
	for _, c := range []struct {
		what   string
		stored *bookingModel.Page
		hosts  []string
		mode   string
		want   bool
	}{
		{"a create", nil, nil, teamSolo, false},
		{"hosts left out, team kept", team, nil, teamCollective, true},
		{"hosts left out, solo kept", solo, nil, teamSolo, true},
		{"hosts left out, solo made a team", solo, nil, teamCollective, true},
		{"hosts left out, team made solo", team, nil, teamSolo, false},
		{"an explicit empty list", team, []string{}, teamCollective, false},
		{"a list", team, []string{uuid.NewString()}, teamCollective, false},
	} {
		if got := keepsHosts(c.stored, c.hosts, c.mode); got != c.want {
			t.Errorf("%s: keeps hosts %v; want %v", c.what, got, c.want)
		}
	}
}

var teamDay = time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC) // a Thursday

func teamAt(h int) time.Time { return teamDay.Add(time.Duration(h) * time.Hour) }

// teamPage is a collective page of owner's, open 09:00 to 17:00 UTC every day,
// with hosts.
func teamPage(t *testing.T, owner uuid.UUID, hosts ...bookingModel.Host) *livePage {
	t.Helper()
	hours := WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "09:00", End: "17:00", TZ: "UTC"}
	loc, err := hours.Check()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(hours)
	return &livePage{page: &bookingModel.Page{Id: uuid.New(), UserId: owner, Slug: "team-call", Title: "Team call",
		DurationMinutes: 30, Hours: raw, MaxDaysAhead: 30, Active: true, TeamMode: "collective"},
		hours: hours, loc: loc, name: "Sam Rivera", hosts: hosts}
}

// threeHosts are Ana who accepted, Ben who is only invited and Cy who left,
// busy at 10:00, 11:00 and 12:00 on teamDay.
func threeHosts() (ana, ben, cy bookingModel.Host, busy map[string][]Interval) {
	then := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ana = bookingModel.Host{UserId: uuid.New(), Name: "Ana", State: bookingModel.HostAccepted, AcceptedAt: &then}
	ben = bookingModel.Host{UserId: uuid.New(), Name: "Ben", State: bookingModel.HostInvited}
	cy = bookingModel.Host{UserId: uuid.New(), Name: "Cy", State: bookingModel.HostLeft, AcceptedAt: &then, LeftAt: &then}
	busy = map[string][]Interval{
		ana.UserId.String(): {{Start: teamAt(10), End: teamAt(10).Add(30 * time.Minute)}},
		ben.UserId.String(): {{Start: teamAt(11), End: teamAt(11).Add(30 * time.Minute)}},
		cy.UserId.String():  {{Start: teamAt(12), End: teamAt(12).Add(30 * time.Minute)}},
	}
	return
}

// A collective page's slot is free only when the owner and every host who
// accepted are free; an invited or a departed host's time never counts, and is
// never read. A move leaves its own event out for every one of them. A host's
// calendar that can't be read makes the time not free.
func TestATeamSlotIsFreeOnlyWhenEveryAcceptedHostIs(t *testing.T) {
	owner := uuid.New()
	ana, ben, cy, busy := threeHosts()
	l := teamPage(t, owner, ana, ben, cy)
	own := &dgraphStruct.DgraphEvent{Uuid: uuid.NewString()}
	unread := ""
	var mu sync.Mutex
	read := map[string]int{}
	oldRead := readBusy
	t.Cleanup(func() { readBusy = oldRead })
	readBusy = func(_ context.Context, who string, _, _ time.Time, opts BusyOptions) (Busy, error) {
		mu.Lock()
		read[who]++
		mu.Unlock()
		if opts.Except != own {
			t.Errorf("read %s without leaving the moved event out", who)
		}
		return Busy{Intervals: busy[who], GoogleUnread: who == unread}, nil
	}
	now := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	free := func(h int) error {
		t.Helper()
		start, end, err := l.slotTimes(teamAt(h), now)
		if err != nil {
			t.Fatal(err)
		}
		return l.slotFree(context.Background(), start, end, now, own)
	}
	var ae *AvailabilityError
	if err := free(10); !errors.As(err, &ae) {
		t.Errorf("10:00, Ana (accepted) busy: %v; want taken", err)
	}
	if err := free(11); err != nil {
		t.Errorf("11:00, Ben (invited) busy: %v; want free", err)
	}
	if err := free(12); err != nil {
		t.Errorf("12:00, Cy (left) busy: %v; want free", err)
	}
	if read[ben.UserId.String()]+read[cy.UserId.String()] != 0 || read[owner.String()] == 0 || read[ana.UserId.String()] == 0 {
		t.Errorf("reads %v; want the owner's and Ana's alone", read)
	}
	unread = ana.UserId.String()
	if err := free(13); !errors.Is(err, ErrCalendarUnread) {
		t.Errorf("13:00 with Ana's Google unread: %v; want ErrCalendarUnread", err)
	}
}

// The public page offers what every accepted host is free for, and names them:
// names alone, never an invited or departed host.
func TestATeamPagesPublicSlotsAndNames(t *testing.T) {
	owner := uuid.New()
	ana, ben, cy, busy := threeHosts()
	withPage(t, owner, nil)
	loadLive = func(context.Context, string) (*livePage, error) { return teamPage(t, owner, ana, ben, cy), nil }
	busyFor = func(_ context.Context, who string, _, _ time.Time) (Busy, error) {
		if who == ben.UserId.String() || who == cy.UserId.String() {
			t.Errorf("read the calendars of a host who hasn't accepted (%s)", who)
		}
		return Busy{Intervals: busy[who]}, nil
	}
	now := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	p, err := GetPublicPage(context.Background(), "team-call", teamDay, now)
	if err != nil {
		t.Fatal(err)
	}
	offered := map[time.Time]bool{}
	for _, s := range p.Slots {
		offered[s.Start] = true
	}
	if offered[teamAt(10)] || !offered[teamAt(11)] || !offered[teamAt(12)] {
		t.Errorf("10:00 %v 11:00 %v 12:00 %v; want Ana's 10:00 alone taken", offered[teamAt(10)], offered[teamAt(11)], offered[teamAt(12)])
	}
	if !reflect.DeepEqual(p.HostNames, []string{"Ana"}) {
		t.Errorf("host names %v; want [Ana]", p.HostNames)
	}
}

// CR-452 F4: a team page's people are read one provider call each (each has
// their own calendars and Google token), but side by side, at most
// teamBusyReads at once, under one timeout: nine slow hosts take about three
// reads' time, not nine, and one past the timeout leaves the rest intact.
func TestATeamsCalendarsAreReadSideBySideUnderOneTimeout(t *testing.T) {
	const call = 200 * time.Millisecond
	hosts := make([]string, maxHosts)
	for i := range hosts {
		hosts[i] = uuid.NewString()
	}
	var mu sync.Mutex
	running, most := 0, 0
	slow := map[string]time.Duration{}
	read := func(ctx context.Context, who string) (Busy, error) {
		mu.Lock()
		running++
		most = max(most, running)
		took := call
		if d, ok := slow[who]; ok {
			took = d
		}
		mu.Unlock()
		defer func() { mu.Lock(); running--; mu.Unlock() }()
		select {
		case <-time.After(took):
		case <-ctx.Done():
			return Busy{GoogleUnread: true}, nil // as ReadBusy: a calendar not reached is read in part
		}
		i := teamIndex(hosts, who)
		return Busy{Intervals: []Interval{{Start: teamAt(i), End: teamAt(i).Add(30 * time.Minute)}}}, nil
	}
	intact := func(busy Busy, skip int) {
		t.Helper()
		var want []Interval
		for i := range hosts {
			if i != skip {
				want = append(want, Interval{Start: teamAt(i), End: teamAt(i).Add(30 * time.Minute)})
			}
		}
		if !reflect.DeepEqual(busy.Intervals, want) {
			t.Errorf("busy %v; want each host's own, in order: %v", busy.Intervals, want)
		}
	}

	start := time.Now()
	busy, err := teamBusy(context.Background(), hosts, read)
	took := time.Since(start)
	t.Logf("nine hosts of %v each: %v, at most %d at once", call, took.Round(time.Millisecond), most)
	if err != nil || busy.GoogleUnread {
		t.Fatalf("err %v, read in part %v; want every host read", err, busy.GoogleUnread)
	}
	intact(busy, -1)
	if most > teamBusyReads || most < 2 {
		t.Errorf("at most %d read at once; want side by side, %d at most", most, teamBusyReads)
	}
	if took < 3*call || took > 5*call {
		t.Errorf("nine hosts read in %v; want about ceil(9/%d) = 3 reads' time, %v, not nine, %v", took, teamBusyReads, 3*call, 9*call)
	}

	// The first host's calendar is past the one timeout: the read ends at it,
	// read in part, and every other host's busy time is still there.
	old := teamBusyTimeout
	teamBusyTimeout = 4 * call
	defer func() { teamBusyTimeout = old }()
	slow[hosts[0]] = 10 * call
	most = 0
	start = time.Now()
	busy, err = teamBusy(context.Background(), hosts, read)
	took = time.Since(start)
	t.Logf("one host past a %v timeout: %v", teamBusyTimeout, took.Round(time.Millisecond))
	if err != nil || !busy.GoogleUnread {
		t.Fatalf("err %v, read in part %v; want the slow host's calendar unread, nothing failed", err, busy.GoogleUnread)
	}
	intact(busy, 0)
	if took > teamBusyTimeout+2*call {
		t.Errorf("a slow host held the read %v; want it ended at the timeout, %v", took, teamBusyTimeout)
	}
}

// The :472 leg: the owner's 13:00, read first, doesn't hide a host's 11:00
// from FreeSlots, which walks busy time in order.
func TestATeamsBusyTimeIsInOrderWhoeverIsReadFirst(t *testing.T) {
	owner, host := uuid.NewString(), uuid.NewString()
	at := map[string]int{owner: 13, host: 11}
	busy, err := teamBusy(context.Background(), []string{owner, host}, func(_ context.Context, who string) (Busy, error) {
		return Busy{Intervals: []Interval{{Start: teamAt(at[who]), End: teamAt(at[who]).Add(30 * time.Minute)}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	hours := WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "09:00", End: "17:00", TZ: "UTC"}
	for _, s := range FreeSlots(busy.Intervals, teamAt(9), teamAt(17), hours, SlotRules{Duration: 30 * time.Minute}) {
		if s.Start.Equal(teamAt(11)) || s.Start.Equal(teamAt(13)) {
			t.Errorf("%v offered; want the host's 11:00 and the owner's 13:00 taken", s.Start)
		}
	}
}

func teamIndex(hosts []string, who string) int {
	for i, h := range hosts {
		if h == who {
			return i
		}
	}
	return -1
}
