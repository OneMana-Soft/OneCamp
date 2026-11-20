package Calendar

import (
	"context"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A channel's calendar and a team's are as the channel and the team are to
// each person, through the Access Resolver's own rules for them: one
// calendar each, made the first time by someone who may see the container,
// and nothing at all to anyone who may not.
func TestChannelAndTeamCalendarsAreAsTheirContainerIs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jonas, maya, lee := h.member("Jonas"), h.member("Maya"), h.member("Lee")
	channel, team := uuid.NewString(), uuid.NewString()
	projectsDecide(t, map[string]map[string]accessBusiness.Level{
		// Jonas posts in the channel and runs the team; Maya reads the
		// (public) channel and is in the team; Lee is in neither.
		personOf(jonas).String(): {channel: accessBusiness.LevelEdit, team: accessBusiness.LevelFull},
		personOf(maya).String():  {channel: accessBusiness.LevelView, team: accessBusiness.LevelEdit},
	}, map[string]string{channel: "launch", team: "Design"})

	for _, c := range []struct {
		kind, id string
		levels   map[*model.UserInfo]accessBusiness.Level
	}{
		{containerChannel, channel, map[*model.UserInfo]accessBusiness.Level{jonas: accessBusiness.LevelEdit, maya: accessBusiness.LevelView, lee: accessBusiness.LevelNone}},
		{containerTeam, team, map[*model.UserInfo]accessBusiness.Level{jonas: accessBusiness.LevelEdit, maya: accessBusiness.LevelEdit, lee: accessBusiness.LevelNone}},
	} {
		cal, err := h.s.ContainerCalendarFor(ctx, jonas, c.kind, c.id)
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		if again, err := h.s.ContainerCalendarFor(ctx, maya, c.kind, c.id); err != nil || again != cal {
			t.Fatalf("%s: one calendar each: %s, then %s (%v)", c.kind, cal, again, err)
		}
		if _, err := h.s.ContainerCalendarFor(ctx, lee, c.kind, c.id); err != ErrEventNotFound {
			t.Fatalf("%s: someone outside it got its calendar: %v", c.kind, err)
		}
		for who, want := range c.levels {
			got, err := h.s.decideCalendars(ctx, subjectOf(who), []string{cal.String()})
			if err != nil {
				t.Fatal(err)
			}
			if d := got[cal.String()]; d.Level != want || d.Owner {
				t.Errorf("%s's calendar to %s: %+v, want %v", c.kind, who.UserDgraphInfo.UserName, d, want)
			}
		}
		// The list shows it to those who may see it, by the container's name.
		for who, want := range c.levels {
			cals, err := h.s.Calendars(ctx, who)
			if err != nil {
				t.Fatal(err)
			}
			listed := ""
			for _, v := range cals {
				if v.ID == cal.String() {
					listed = v.Name
				}
			}
			if (listed != "") != (want >= accessBusiness.LevelView) {
				t.Errorf("%s's calendar in %s's list: %q, want listed %v", c.kind, who.UserDgraphInfo.UserName, listed, want >= accessBusiness.LevelView)
			}
		}
	}
	if _, err := h.s.ContainerCalendarFor(ctx, jonas, "board", channel); err == nil {
		t.Fatal("a board has no calendar of its own")
	}
}

// freeBusyChoices stands in for the Policy Registry: the workspace's
// calendar.freebusy.members, and each person's own value of it (unset: the
// workspace's).
func freeBusyChoices(t *testing.T, workspace bool, own map[string]bool) {
	t.Helper()
	prevPolicy, prevOwn := freeBusyPolicy, freeBusyShownBy
	freeBusyPolicy = func(context.Context) bool { return workspace }
	freeBusyShownBy = func(_ context.Context, people []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, p := range people {
			out[p] = workspace
			if v, ok := own[p]; ok {
				out[p] = v
			}
		}
		return out, nil
	}
	t.Cleanup(func() { freeBusyPolicy, freeBusyShownBy = prevPolicy, prevOwn })
}

// Someone who keeps when they're busy to themselves is kept from every other
// member's view of it, the calendar's decisions and Find a time's people
// alike; a calendar they shared at a level still shows as shared; and the
// workspace's "off" wins over anyone's own "on".
func TestAPersonCanKeepWhenTheyAreBusyToThemselves(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jonas, maya, lee := h.member("Jonas"), h.member("Maya"), h.member("Lee")
	prev := stored
	stored = h.s
	t.Cleanup(func() { stored = prev })
	mayas, _ := h.store.personalCalendar(ctx, personOf(maya), "UTC")
	lees, _ := h.store.personalCalendar(ctx, personOf(lee), "UTC")
	m, l, j := personOf(maya).String(), personOf(lee).String(), personOf(jonas).String()

	level := func(who *model.UserInfo, cal uuid.UUID) accessBusiness.Decision {
		t.Helper()
		got, err := h.s.decideCalendars(ctx, subjectOf(who), []string{cal.String()})
		if err != nil {
			t.Fatal(err)
		}
		return got[cal.String()]
	}
	seenBy := func(asker string) map[string]bool {
		t.Helper()
		seen, _, err := PeopleSeenBy(ctx, asker, []string{m, l, j})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, p := range seen {
			out[p] = true
		}
		return out
	}

	// Maya hides; Lee leaves it as the workspace has it.
	freeBusyChoices(t, true, map[string]bool{m: false})
	if d := level(jonas, mayas); d.Level != accessBusiness.LevelNone {
		t.Errorf("Maya hides when she's busy, yet Jonas has %+v on her calendar", d)
	}
	if d := level(jonas, lees); !busyOnly(d) {
		t.Errorf("Lee's busy times should show to Jonas as busy alone: %+v", d)
	}
	if d := level(maya, mayas); d.Level != accessBusiness.LevelFull || !d.Owner {
		t.Errorf("Maya's own calendar to her: %+v", d)
	}
	if s := seenBy(j); s[m] || !s[l] || !s[j] {
		t.Errorf("Find a time for Jonas: %v, want Lee and himself, never Maya", s)
	}
	if s := seenBy(m); !s[m] || !s[l] {
		t.Errorf("Find a time for Maya sees her own and Lee's: %v", s)
	}
	// A calendar she shared to show its details still shows them.
	if err := h.s.Share(ctx, maya, mayas.String(), j, "details"); err != nil {
		t.Fatal(err)
	}
	if d := level(jonas, mayas); d.Level != accessBusiness.LevelView || busyOnly(d) {
		t.Errorf("a calendar Maya shared with Jonas: %+v", d)
	}

	// The workspace's "off" wins over Lee's own "on".
	freeBusyChoices(t, false, map[string]bool{m: false, l: true})
	if d := level(jonas, lees); d.Level != accessBusiness.LevelNone {
		t.Errorf("with the workspace off, Lee's own \"on\" showed his calendar to Jonas: %+v", d)
	}
	if s := seenBy(j); s[l] || s[m] || !s[j] {
		t.Errorf("Find a time for Jonas with the workspace off: %v, want himself alone", s)
	}
}
