package Calendar

import (
	"bytes"
	"context"
	"errors"
	"path"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// feedsOn sets the workspace's calendar.feeds.allowed for one test.
func feedsOn(t *testing.T, on bool) {
	t.Helper()
	prev := feedsAllowed
	feedsAllowed = func(context.Context) bool { return on }
	t.Cleanup(func() { feedsAllowed = prev })
}

// tokenOf is the token in a feed's link.
func tokenOf(t *testing.T, v *FeedView) string {
	t.Helper()
	if v == nil || v.URL == "" || !strings.HasSuffix(v.URL, ".ics") {
		t.Fatalf("no link in %+v", v)
	}
	return FeedTokenOf(path.Base(v.URL))
}

// Calendar B8: Jonas's own calendar in another app, whole; Maya's link to it
// only when he's busy, then what too once he shares it, his private event
// still "Busy", and nothing at all once the share stops. A reset link and a
// workspace that turns links off answer nothing at once. The link is kept
// only as its hash, and given once.
func TestAFeedIsTheCalendarAsItsPersonSeesItNow(t *testing.T) {
	h := newHarness(t)
	feedsOn(t, true)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	mk := func(title string, hour int, tweak func(*EventInput)) {
		t.Helper()
		start, end := timed(day.Add(time.Duration(hour)*time.Hour), 60, "Europe/London")
		in := EventInput{Title: title, Start: start, End: end}
		if tweak != nil {
			tweak(&in)
		}
		if _, err := h.s.Create(ctx, jonas, in); err != nil {
			t.Fatal(err)
		}
	}
	mk("Pricing review", 9, nil)
	mk("Doctor", 11, func(in *EventInput) { in.Visibility = visibilityPrivate })
	mk("Focus reading", 14, func(in *EventInput) { in.Transparency = transparencyTransparent })
	own, _ := h.store.personalCalendar(ctx, personOf(jonas), "UTC")

	// Jonas's own, whole, named for where it comes from, with no alarms.
	made, err := h.s.MakeFeed(ctx, jonas, own.String(), models.FeedFull)
	if err != nil || !made.Live || made.Most != models.FeedFull {
		t.Fatalf("made %+v %v", made, err)
	}
	token := tokenOf(t, made)
	if !bytes.Equal(h.store.feeds[0].TokenHash, helpers.LinkSecretHash(token)) || bytes.Contains(h.store.feeds[0].TokenHash, []byte(token)) {
		t.Fatal("the link is kept as anything but its hash")
	}
	again, err := h.s.Feed(ctx, jonas, own.String())
	if err != nil || !again.Live || again.URL != "" || again.Scope != models.FeedFull {
		t.Fatalf("the link was given again, or the feed forgotten: %+v %v", again, err)
	}
	ics := func(token string) (string, error) {
		b, err := h.s.FeedICS(ctx, token)
		return string(b), err
	}
	body, err := ics(token)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SUMMARY:Pricing review", "SUMMARY:Doctor", "SUMMARY:Focus reading", "X-WR-CALNAME:OneCamp", "REFRESH-INTERVAL"} {
		if !strings.Contains(body, want) {
			t.Errorf("Jonas's feed has no %q", want)
		}
	}
	if strings.Contains(body, "BEGIN:VALARM") {
		t.Error("a feed rang an alarm")
	}

	// Maya sees only when he's busy: her link may show only that.
	theirs, err := h.s.PersonCalendar(ctx, maya, personOf(jonas).String())
	if err != nil || theirs != own {
		t.Fatalf("Jonas's calendar for Maya: %s %v", theirs, err)
	}
	var in *EventInputError
	if _, err := h.s.MakeFeed(ctx, maya, own.String(), models.FeedFull); !errors.As(err, &in) {
		t.Fatalf("Maya's full link to a calendar she sees as busy: %v", err)
	}
	busy, err := h.s.MakeFeed(ctx, maya, own.String(), models.FeedBusy)
	if err != nil || busy.Most != models.FeedBusy {
		t.Fatalf("busy %+v %v", busy, err)
	}
	body, err = ics(tokenOf(t, busy))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(body, "SUMMARY:Busy") != 2 || strings.Contains(body, "Pricing") || strings.Contains(body, "Doctor") || strings.Contains(body, "Focus") || strings.Contains(body, "ATTENDEE") {
		t.Fatalf("a busy feed said more than when:\n%s", body)
	}

	// Shared with details: titles, but his private event is still "Busy".
	if err := h.s.Share(ctx, jonas, own.String(), personOf(maya).String(), models.ShareDetails); err != nil {
		t.Fatal(err)
	}
	full, err := h.s.MakeFeed(ctx, maya, own.String(), models.FeedFull)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ics(tokenOf(t, busy)); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("Maya's old link still answers after her new one: %v", err)
	}
	body, err = ics(tokenOf(t, full))
	if err != nil || !strings.Contains(body, "SUMMARY:Pricing review") || strings.Contains(body, "Doctor") || !strings.Contains(body, "SUMMARY:Busy") {
		t.Fatalf("Maya's full feed: %v\n%s", err, body)
	}
	// Once she may change his calendar, her feed shows his private event
	// whole, as the grid does and as in Google Calendar (audit A12).
	if err := h.s.Share(ctx, jonas, own.String(), personOf(maya).String(), models.ShareEdit); err != nil {
		t.Fatal(err)
	}
	body, err = ics(tokenOf(t, full))
	if err != nil || !strings.Contains(body, "SUMMARY:Doctor") || strings.Contains(body, "SUMMARY:Busy") {
		t.Fatalf("Maya's feed once she may change his calendar: %v\n%s", err, body)
	}
	// The share stops: her link to what is on it answers nothing.
	if err := h.s.Share(ctx, jonas, own.String(), personOf(maya).String(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ics(tokenOf(t, full)); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("a full link answered after its share stopped: %v", err)
	}

	// Reset: the old link answers nothing, the new one does.
	reset, err := h.s.MakeFeed(ctx, jonas, own.String(), models.FeedBusy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ics(token); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("a reset link still answers: %v", err)
	}
	if _, err := ics(tokenOf(t, reset)); err != nil {
		t.Fatalf("the new link: %v", err)
	}
	// Stopped: nothing.
	if err := h.s.StopFeed(ctx, jonas, own.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := ics(tokenOf(t, reset)); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("a stopped link answers: %v", err)
	}
	if v, _ := h.s.Feed(ctx, jonas, own.String()); v.Live {
		t.Fatalf("a stopped feed is live: %+v", v)
	}

	// The workspace turns links off: every one answers nothing, and none is made.
	live, _ := h.s.MakeFeed(ctx, jonas, own.String(), models.FeedFull)
	feedsOn(t, false)
	if _, err := ics(tokenOf(t, live)); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("a link answered with links off: %v", err)
	}
	if _, err := h.s.MakeFeed(ctx, jonas, own.String(), models.FeedFull); !errors.As(err, &in) {
		t.Fatalf("a link made with links off: %v", err)
	}
	feedsOn(t, true)
	// Someone no longer in the workspace: nothing.
	h.store.noAccount[personOf(jonas)] = true
	if _, err := ics(tokenOf(t, live)); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("a link answered for someone gone: %v", err)
	}
	if _, err := ics("not-a-token"); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("an unknown link: %v", err)
	}
}

// A project's calendar has a feed for each member who may see it
// (basecamp-05), named for the project, and none once they leave it.
func TestAProjectsCalendarHasAFeedForEachMember(t *testing.T) {
	h := newHarness(t)
	feedsOn(t, true)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	project := uuid.NewString()
	levels := map[string]map[string]accessBusiness.Level{
		personOf(jonas).String(): {project: accessBusiness.LevelFull},
		personOf(maya).String():  {project: accessBusiness.LevelView},
	}
	projectsDecide(t, levels, map[string]string{project: "Launch"})
	ctx := context.Background()
	cal, err := h.s.ProjectCalendarFor(ctx, jonas, project)
	if err != nil {
		t.Fatal(err)
	}
	h.onCalendar(cal, jonas, "Release", time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC), "")
	made, err := h.s.MakeFeed(ctx, maya, cal.String(), models.FeedFull)
	if err != nil {
		t.Fatal(err)
	}
	body, err := h.s.FeedICS(ctx, tokenOf(t, made))
	if err != nil || !strings.Contains(string(body), "SUMMARY:Release") || !strings.Contains(string(body), "X-WR-CALNAME:Launch") {
		t.Fatalf("Maya's feed of the project's calendar: %v\n%s", err, body)
	}
	delete(levels[personOf(maya).String()], project)
	if _, err := h.s.FeedICS(ctx, tokenOf(t, made)); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("a feed answered after its person left the project: %v", err)
	}
	if _, err := h.s.MakeFeed(ctx, maya, cal.String(), models.FeedFull); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("a feed made of a project's calendar its person can't see: %v", err)
	}
}
