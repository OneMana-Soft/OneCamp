package Calendar

import (
	"context"
	"errors"
	"strings"
	"testing"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// calendarTold are the events told about calendars themselves (not their
// events), in order.
func calendarTold(h *harness) []resourceEventBusiness.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []resourceEventBusiness.Event
	for _, e := range h.emitted {
		if e.ResourceKind == KindCalendar {
			out = append(out, e)
		}
	}
	return out
}

// Every change to a calendar is told once it's made, for the audit log's
// "calendar" (gcal-new-audit-retention): a share given, changed or stopped,
// a rename or a new colour, an address subscribed to or dropped. A change
// that changes nothing, or is refused, tells nothing; an address is never
// told, only its host, since it can carry a private key.
func TestEveryCalendarChangeIsToldOnceItIsMade(t *testing.T) {
	h := newHarness(t)
	jonas, maya, sam := h.member("Jonas"), h.member("Maya"), h.member("Sam")
	withAddresses(t, h, map[string]string{teamAddress: teamFile("20261020T100000Z", "Review", true)})
	ctx := context.Background()
	cal, err := h.s.PersonCalendar(ctx, jonas, personOf(jonas).String())
	if err != nil {
		t.Fatal(err)
	}
	mayaID := personOf(maya).String()
	steps := []struct {
		name string
		do   func() error
		want []string // kind|level|from|changed|host|people
	}{
		{"Maya can't share Jonas's calendar", func() error { _ = h.s.Share(ctx, maya, cal.String(), mayaID, models.ShareEdit); return nil }, nil},
		{"shared to see details", func() error { return h.s.Share(ctx, jonas, cal.String(), mayaID, models.ShareDetails) },
			[]string{"calendar.shared|details||||" + mayaID}},
		{"the same share again", func() error { return h.s.Share(ctx, jonas, cal.String(), mayaID, models.ShareDetails) }, nil},
		{"raised to edit", func() error { return h.s.Share(ctx, jonas, cal.String(), mayaID, models.ShareEdit) },
			[]string{"calendar.shared|edit|details|||" + mayaID}},
		{"lowered to details", func() error { return h.s.Share(ctx, jonas, cal.String(), mayaID, models.ShareDetails) },
			[]string{"calendar.shared|details|edit|||" + mayaID}},
		{"stopped", func() error { return h.s.Share(ctx, jonas, cal.String(), mayaID, "") },
			[]string{"calendar.unshared||details|||" + mayaID}},
		{"stopped again", func() error { return h.s.Share(ctx, jonas, cal.String(), mayaID, "") }, nil},
		{"recoloured", func() error {
			hue := "moss"
			return h.s.UpdateCalendar(ctx, jonas, cal.String(), CalendarPatch{Hue: &hue})
		},
			[]string{"calendar.updated|||hue||"}},
		{"nothing asked", func() error { return h.s.UpdateCalendar(ctx, jonas, cal.String(), CalendarPatch{}) }, nil},
	}
	var subscribed string
	steps = append(steps,
		struct {
			name string
			do   func() error
			want []string
		}{"subscribed", func() error {
			v, err := h.s.Subscribe(ctx, sam, teamAddress, "", "")
			if err == nil {
				subscribed = v.CalendarID
			}
			return err
		}, []string{"calendar.subscribed||||calendar.example|"}},
		struct {
			name string
			do   func() error
			want []string
		}{"renamed", func() error {
			name := "Team plans"
			return h.s.UpdateCalendar(ctx, sam, subscribed, CalendarPatch{Name: &name})
		}, []string{"calendar.updated|||name||"}},
		struct {
			name string
			do   func() error
			want []string
		}{"unsubscribed", func() error { return h.s.Unsubscribe(ctx, sam, subscribed) }, []string{"calendar.unsubscribed||||calendar.example|"}},
	)
	for _, st := range steps {
		before := len(calendarTold(h))
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		told := calendarTold(h)[before:]
		var got []string
		for _, e := range told {
			got = append(got, strings.Join([]string{string(e.Kind), e.Data["level"], e.Data["from"], e.Data["changed"], e.Data["host"], strings.Join(e.Subjects, ",")}, "|"))
			if e.Actor.UserUUID == "" || e.At.IsZero() || e.ResourceUUID == "" {
				t.Errorf("%s: told without who, when or which calendar: %+v", st.name, e)
			}
			if e.Kind == resourceEventBusiness.CalendarShared || e.Kind == resourceEventBusiness.CalendarUnshared || e.Kind == resourceEventBusiness.CalendarUpdated && e.Actor.UserUUID == personOf(jonas).String() {
				if e.ResourceUUID != cal.String() {
					t.Errorf("%s: told of calendar %s, want %s", st.name, e.ResourceUUID, cal)
				}
			}
			for k, v := range e.Data {
				if strings.Contains(v, "private-7rKq2mX9") || strings.Contains(v, "Team plans") {
					t.Errorf("%s: %s tells the address or the name: %s=%s", st.name, e.Kind, k, v)
				}
			}
		}
		if strings.Join(got, " ; ") != strings.Join(st.want, " ; ") {
			t.Errorf("%s: told %q, want %q", st.name, got, st.want)
		}
	}
	if _, err := uuid.Parse(subscribed); err != nil {
		t.Fatalf("no subscribed calendar: %q", subscribed)
	}
}

// A calendar's secret link is a change to it too (V-398): a link made, made
// anew over the one before, or stopped is told once the store has it, with
// what the link shows and what the stopped one showed, as the store says. A
// refused link, or a stop with no live link, tells nothing; the link's token
// and its address are never told.
func TestACalendarsLinkIsToldOnceItIsMadeOrStopped(t *testing.T) {
	h := newHarness(t)
	feedsOn(t, true)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	cal, err := h.s.PersonCalendar(ctx, jonas, personOf(jonas).String())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{personOf(jonas).String(): "Jonas", personOf(maya).String(): "Maya"}
	var secrets []string // every link made, and its token
	makes := func(who *model.UserInfo, scope string) func() error {
		return func() error {
			v, err := h.s.MakeFeed(ctx, who, cal.String(), scope)
			if err == nil {
				secrets = append(secrets, v.URL, tokenOf(t, v))
			}
			return err
		}
	}
	refused := func(do func() error) func() error {
		return func() error {
			if err := do(); err == nil {
				return errors.New("it wasn't refused")
			}
			return nil
		}
	}
	stops := func(who *model.UserInfo) func() error {
		return func() error { return h.s.StopFeed(ctx, who, cal.String()) }
	}
	steps := []struct {
		name string
		do   func() error
		want []string // kind|scope|replaced|who
	}{
		{"stopped with no live link", stops(jonas), nil},
		{"the workspace turned links off", func() error {
			prev := feedsAllowed
			feedsAllowed = func(context.Context) bool { return false }
			defer func() { feedsAllowed = prev }()
			return refused(makes(jonas, models.FeedFull))()
		}, nil},
		{"Maya asks for everything of a calendar she sees as busy", refused(makes(maya, models.FeedFull)), nil},
		{"made, busy only", makes(jonas, models.FeedBusy), []string{"calendar.feed.made|busy||Jonas"}},
		{"made anew, everything", makes(jonas, models.FeedFull), []string{"calendar.feed.made|full|busy|Jonas"}},
		{"Maya's own, busy only", makes(maya, models.FeedBusy), []string{"calendar.feed.made|busy||Maya"}},
		{"stopped", stops(jonas), []string{"calendar.feed.stopped|full||Jonas"}},
		{"stopped again", stops(jonas), nil},
	}
	for _, st := range steps {
		before := len(calendarTold(h))
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		var got []string
		for _, e := range calendarTold(h)[before:] {
			got = append(got, strings.Join([]string{string(e.Kind), e.Data["scope"], e.Data["replaced"], names[e.Actor.UserUUID]}, "|"))
			if e.At.IsZero() || e.ResourceUUID != cal.String() {
				t.Errorf("%s: told without when, or of calendar %q, want %s", st.name, e.ResourceUUID, cal)
			}
			for k, v := range e.Data {
				if k != "scope" && k != "replaced" {
					t.Errorf("%s: %s tells %s=%q", st.name, e.Kind, k, v)
				}
				for _, s := range secrets {
					if strings.Contains(v, s) {
						t.Errorf("%s: %s tells the link or its token: %s=%q", st.name, e.Kind, k, v)
					}
				}
			}
		}
		if strings.Join(got, " ; ") != strings.Join(st.want, " ; ") {
			t.Errorf("%s: told %q, want %q", st.name, got, st.want)
		}
	}
	if len(secrets) != 6 {
		t.Fatalf("made %d links, want 3", len(secrets)/2)
	}
}

// A calendar's rows say what changed, to whom and at what level, in the
// words the share dialog uses; they never name the calendar or quote an
// address. Pure.
func TestTheCalendarRowsSayWhatChangedWithoutNamingIt(t *testing.T) {
	maya, sam := uuid.NewString(), uuid.NewString()
	ev := func(kind resourceEventBusiness.Kind, subjects []string, data map[string]string) resourceEventBusiness.Event {
		return resourceEventBusiness.Event{Kind: kind, ResourceKind: KindCalendar, ResourceUUID: "c1",
			Actor: resourceEventBusiness.Actor{UserUUID: uuid.NewString()}, Subjects: subjects, Data: data}
	}
	for _, tc := range []struct {
		e    resourceEventBusiness.Event
		want string
	}{
		{ev(resourceEventBusiness.CalendarShared, []string{maya}, map[string]string{"level": "details"}), "Shared a calendar with 1 person who can see details"},
		{ev(resourceEventBusiness.CalendarShared, []string{maya, sam}, map[string]string{"level": "manage"}), "Shared a calendar with 2 people who can edit and share"},
		{ev(resourceEventBusiness.CalendarShared, []string{maya}, map[string]string{"level": "details", "from": "edit"}), "Changed a calendar's share: 1 person who can see details now (could edit)"},
		{ev(resourceEventBusiness.CalendarUnshared, []string{maya}, map[string]string{"from": "manage"}), "Stopped sharing a calendar with 1 person (could edit and share)"},
		{ev(resourceEventBusiness.CalendarUpdated, nil, map[string]string{"changed": "name,hue"}), "Changed a calendar (name, colour)"},
		{ev(resourceEventBusiness.CalendarSubscribed, nil, map[string]string{"host": "calendar.example"}), "Subscribed to a calendar at calendar.example"},
		{ev(resourceEventBusiness.CalendarUnsubscribed, nil, map[string]string{"host": "calendar.example"}), "Unsubscribed from a calendar at calendar.example"},
		{ev(resourceEventBusiness.Kind("calendar.feed.made"), nil, map[string]string{"scope": "busy"}), "Made a link to a calendar (busy only)"},
		{ev(resourceEventBusiness.Kind("calendar.feed.made"), nil, map[string]string{"scope": "full", "replaced": "busy"}), "Made a new link to a calendar (everything), stopping the old one (busy only)"},
		{ev(resourceEventBusiness.Kind("calendar.feed.stopped"), nil, map[string]string{"scope": "full"}), "Stopped a link to a calendar (everything)"},
	} {
		if !auditsEvent(tc.e) {
			t.Errorf("%s isn't recorded", tc.e.Kind)
			continue
		}
		summary, meta := auditRow(tc.e)
		if summary != tc.want {
			t.Errorf("%s: %q, want %q", tc.e.Kind, summary, tc.want)
		}
		if meta["calendar_uuid"] != "c1" || meta["resource_kind"] != KindCalendar {
			t.Errorf("%s: meta %+v, want the calendar's id and kind", tc.e.Kind, meta)
		}
		if len(tc.e.Subjects) > 0 && meta["people"] != strings.Join(tc.e.Subjects, ",") {
			t.Errorf("%s: meta people %v, want %v", tc.e.Kind, meta["people"], tc.e.Subjects)
		}
	}
	// An event on a calendar is still the event's row; a calendar's reminder
	// or sync failure isn't a change to it.
	if auditsEvent(ev(resourceEventBusiness.CalendarSyncFailed, nil, nil)) || auditsEvent(ev(resourceEventBusiness.CalendarEventCreated, nil, nil)) {
		t.Error("a calendar's sync failure, or an event kind on a calendar, is recorded as a calendar change")
	}
}
