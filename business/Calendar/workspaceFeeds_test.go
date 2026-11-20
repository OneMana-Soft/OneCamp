package Calendar

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// basecamp-05: Maya's link to everything she may see carries Jonas's
// calendar while he shares it with details (his private event still
// "Busy"), never Sam's, which she sees only as busy, and the meeting she is
// invited to on Sam's; once Jonas stops sharing, his events go at the next
// read. A busy link shows only when. A guest never has one. A reset link,
// a stopped one and a workspace that turns links off answer as an unknown
// one does.
func TestAWorkspaceFeedIsEverythingItsPersonMaySeeNow(t *testing.T) {
	h := newHarness(t)
	feedsOn(t, true)
	jonas, maya, sam := h.member("Jonas"), h.member("Maya"), h.member("Sam")
	ctx := context.Background()
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	mk := func(who *Event, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	at := func(hour int, title string, tweak func(*EventInput)) EventInput {
		start, end := timed(day.Add(time.Duration(hour)*time.Hour), 60, "UTC")
		in := EventInput{Title: title, Start: start, End: end}
		if tweak != nil {
			tweak(&in)
		}
		return in
	}
	mk(h.s.Create(ctx, jonas, at(9, "Pricing review", nil)))
	mk(h.s.Create(ctx, jonas, at(11, "Doctor", func(in *EventInput) { in.Visibility = visibilityPrivate })))
	mk(h.s.Create(ctx, sam, at(13, "Sam's interview loop", nil)))
	mk(h.s.Create(ctx, sam, at(15, "Roadmap with Maya", func(in *EventInput) { in.Attendees = []GuestInput{{UUID: personOf(maya).String()}} })))
	jonasCal, _ := h.store.personalCalendar(ctx, personOf(jonas), "UTC")
	if err := h.s.Share(ctx, jonas, jonasCal.String(), personOf(maya).String(), models.ShareDetails); err != nil {
		t.Fatal(err)
	}
	read := func(token string) (string, error) {
		b, err := h.s.FeedICS(ctx, token)
		return string(b), err
	}
	made, err := h.s.MakeFeed(ctx, maya, WorkspaceFeedID, models.FeedFull)
	if err != nil || !made.Live || made.Most != models.FeedFull {
		t.Fatalf("Maya's link to everything: %+v %v", made, err)
	}
	token := tokenOf(t, made)
	// Kept only as its hash, and at least 128 random bits.
	if kept := h.store.feeds[len(h.store.feeds)-1]; !bytes.Equal(kept.TokenHash, helpers.LinkSecretHash(token)) || bytes.Contains(kept.TokenHash, []byte(token)) {
		t.Errorf("the link is kept as %x, not as its hash", kept.TokenHash)
	}
	if b, err := base64.RawURLEncoding.DecodeString(token); err != nil || len(b) < 16 {
		t.Errorf("the link's secret is %d bytes: %v", len(b), err)
	}
	body, err := read(token)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SUMMARY:Pricing review", "SUMMARY:Roadmap with Maya", "SUMMARY:Busy", "X-WR-CALNAME:" + workspaceFeedName} {
		if !strings.Contains(body, want) {
			t.Errorf("Maya's link to everything has no %q:\n%s", want, body)
		}
	}
	for _, never := range []string{"Doctor", "interview loop", "BEGIN:VALARM"} {
		if strings.Contains(body, never) {
			t.Errorf("Maya's link to everything shows %q", never)
		}
	}

	t.Run("a busy link to everything shows only when", func(t *testing.T) {
		busy, err := h.s.MakeFeed(ctx, maya, WorkspaceFeedID, models.FeedBusy)
		if err != nil {
			t.Fatal(err)
		}
		b, err := read(tokenOf(t, busy))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b, "Pricing") || strings.Contains(b, "Roadmap") || strings.Count(b, "SUMMARY:Busy") != 3 {
			t.Errorf("a busy link to everything said more than when:\n%s", b)
		}
		if _, err := read(token); !errors.Is(err, ErrEventNotFound) {
			t.Errorf("the link it replaced still answers: %v", err)
		}
		again, err := h.s.MakeFeed(ctx, maya, WorkspaceFeedID, models.FeedFull)
		if err != nil {
			t.Fatal(err)
		}
		token = tokenOf(t, again)
	})

	t.Run("a calendar she stops seeing goes at the next read", func(t *testing.T) {
		if err := h.s.Share(ctx, jonas, jonasCal.String(), personOf(maya).String(), ""); err != nil {
			t.Fatal(err)
		}
		b, err := read(token)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b, "Pricing") || !strings.Contains(b, "Roadmap with Maya") {
			t.Errorf("after Jonas stopped sharing:\n%s", b)
		}
	})

	t.Run("a guest never has one, and a person who leaves loses theirs", func(t *testing.T) {
		var refused *EventRefusal
		h.store.notMembers[personOf(sam)] = true
		if _, err := h.s.MakeFeed(ctx, sam, WorkspaceFeedID, models.FeedFull); !errors.As(err, &refused) {
			t.Errorf("a guest made a link to everything: %v", err)
		}
		if _, err := h.s.Feed(ctx, sam, WorkspaceFeedID); !errors.As(err, &refused) {
			t.Errorf("a guest was told of a link to everything: %v", err)
		}
		h.store.notMembers[personOf(maya)] = true
		if _, err := read(token); !errors.Is(err, ErrEventNotFound) {
			t.Errorf("a link to everything answered after its person left: %v", err)
		}
		delete(h.store.notMembers, personOf(maya))
	})

	t.Run("links off, and a stopped link, answer as an unknown one", func(t *testing.T) {
		feedsOn(t, false)
		if _, err := read(token); !errors.Is(err, ErrEventNotFound) {
			t.Errorf("a link to everything answered with links off: %v", err)
		}
		feedsOn(t, true)
		if err := h.s.StopFeed(ctx, maya, WorkspaceFeedID); err != nil {
			t.Fatal(err)
		}
		if _, err := read(token); !errors.Is(err, ErrEventNotFound) {
			t.Errorf("a stopped link to everything answered: %v", err)
		}
		if _, err := read(token + "x"); !errors.Is(err, ErrEventNotFound) {
			t.Errorf("an unknown link answered: %v", err)
		}
	})
}

// basecamp-05: a workspace admin lists every live link (never its address,
// token or hash) and stops any; the audit log names whose, and its person
// is shown who stopped it.
func TestAnAdminSeesAndStopsAnyLinkButNeverItsAddress(t *testing.T) {
	h := newHarness(t)
	feedsOn(t, true)
	ada, maya := h.member("Ada"), h.member("Maya")
	ada.UserPostgresInfo.IsAdmin = true
	ctx := context.Background()
	own, _ := h.store.personalCalendar(ctx, personOf(maya), "UTC")
	one, err := h.s.MakeFeed(ctx, maya, own.String(), models.FeedFull)
	if err != nil {
		t.Fatal(err)
	}
	all, err := h.s.MakeFeed(ctx, maya, WorkspaceFeedID, models.FeedBusy)
	if err != nil {
		t.Fatal(err)
	}
	// Maya hides her free/busy, so Ada may not see Maya's own calendar at all.
	freeBusyChoices(t, true, map[string]bool{personOf(maya).String(): false})
	list, err := h.s.Feeds(ctx, ada)
	if err != nil || len(list.Feeds) != 2 || list.More {
		t.Fatalf("Ada's list: %+v %v", list, err)
	}
	raw, _ := json.Marshal(list)
	secrets := []string{"/public/calendar/", "token", "hash", "url"}
	for _, made := range []*FeedView{one, all} {
		raw := tokenOf(t, made)
		hash := helpers.LinkSecretHash(raw)
		secrets = append(secrets, raw, hex.EncodeToString(hash), base64.StdEncoding.EncodeToString(hash), base64.RawURLEncoding.EncodeToString(hash))
	}
	for _, secret := range secrets {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the admin's list carries %q: %s", secret, raw)
		}
	}
	// So its link is listed by id and kind only: the admin's list never names a
	// calendar its admin can't see.
	if decided, err := h.s.decideCalendars(ctx, subjectOf(ada), []string{own.String()}); err != nil || decided[own.String()].Level >= accessBusiness.LevelView {
		t.Fatalf("Ada may see Maya's own calendar (%+v %v); this test needs one she may not", decided, err)
	}
	var workspace FeedListing
	for _, l := range list.Feeds {
		if l.Person.UUID != personOf(maya).String() {
			t.Errorf("a listed link isn't Maya's: %+v", l)
		}
		if l.Target == WorkspaceFeedID {
			workspace = l
		} else if l.Calendar == nil || l.Calendar.ID != own.String() {
			t.Errorf("a calendar's link names no calendar: %+v", l)
		} else if l.Calendar.Name != "" {
			t.Errorf("the admin's list names %q, a calendar Ada may not see: %+v", l.Calendar.Name, l)
		}
	}
	if workspace.ID == "" || workspace.Calendar != nil || workspace.Scope != models.FeedBusy || workspace.LastReadAt != nil {
		t.Fatalf("Maya's link to everything as listed: %+v", workspace)
	}
	// A read is listed as the link's last.
	if _, err := h.s.FeedICS(ctx, tokenOf(t, all)); err != nil {
		t.Fatal(err)
	}
	if again, err := h.s.Feeds(ctx, ada); err != nil || len(again.Feeds) != 2 {
		t.Fatalf("Ada's list after a read: %+v %v", again, err)
	} else {
		for _, l := range again.Feeds {
			if read := l.LastReadAt != nil; read != (l.ID == workspace.ID) {
				t.Errorf("listed as read %v, though only the link to everything was read: %+v", read, l)
			}
		}
	}
	h.emitted = nil
	if err := h.s.StopAnyFeed(ctx, ada, workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.FeedICS(ctx, tokenOf(t, all)); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("a link an admin stopped still answers: %v", err)
	}
	if err := h.s.StopAnyFeed(ctx, ada, workspace.ID); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("stopping it again: %v", err)
	}
	if len(h.emitted) != 1 || h.emitted[0].Kind != resourceEventBusiness.CalendarFeedStopped || h.emitted[0].Actor.UserUUID != personOf(ada).String() ||
		h.emitted[0].Data["by"] != "admin" || h.emitted[0].Data["target"] != WorkspaceFeedID || len(h.emitted[0].Subjects) != 1 || h.emitted[0].Subjects[0] != personOf(maya).String() {
		t.Fatalf("the stop as told to the audit log: %+v", h.emitted)
	}
	if what, _ := calendarChange(h.emitted[0]); what != "stopped a member's link to all their calendars (busy only)" {
		t.Errorf("its audit row says %q", what)
	}
	v, err := h.s.Feed(ctx, maya, WorkspaceFeedID)
	if err != nil || v.Live || v.StoppedBy == nil || v.StoppedBy.UUID != personOf(ada).String() || v.StoppedAt == nil {
		t.Fatalf("Maya isn't shown who stopped her link: %+v %v", v, err)
	}
	mine, err := h.s.Feed(ctx, maya, own.String())
	if err != nil || !mine.Live || mine.StoppedBy != nil {
		t.Fatalf("her other link: %+v %v", mine, err)
	}
	if err := h.s.StopFeed(ctx, maya, own.String()); err != nil {
		t.Fatal(err)
	}
	if mine, _ = h.s.Feed(ctx, maya, own.String()); mine.StoppedBy != nil {
		t.Errorf("a link she stopped herself names who stopped it: %+v", mine.StoppedBy)
	}
}

// basecamp-05: a project's calendar is in Maya's link to everything while
// she is on the project, and gone at the next read once she leaves it; the
// calendar of a project she was never on is never in it.
func TestAWorkspaceFeedCarriesAProjectsCalendarOnlyWhileItsPersonIsOnIt(t *testing.T) {
	h := newHarness(t)
	feedsOn(t, true)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	launch, hiring := uuid.NewString(), uuid.NewString()
	levels := map[string]map[string]accessBusiness.Level{
		personOf(jonas).String(): {launch: accessBusiness.LevelFull, hiring: accessBusiness.LevelFull},
		personOf(maya).String():  {launch: accessBusiness.LevelView},
	}
	projectsDecide(t, levels, map[string]string{launch: "Launch", hiring: "Hiring"})
	ctx := context.Background()
	day := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	for project, title := range map[string]string{launch: "Release", hiring: "Offer call"} {
		cal, err := h.s.projectCalendarFor(ctx, jonas, project)
		if err != nil {
			t.Fatal(err)
		}
		h.onCalendar(cal, jonas, title, day, "")
	}
	made, err := h.s.MakeFeed(ctx, maya, WorkspaceFeedID, models.FeedFull)
	if err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		b, err := h.s.FeedICS(ctx, tokenOf(t, made))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if body := read(); !strings.Contains(body, "SUMMARY:Release") || strings.Contains(body, "Offer call") || strings.Count(body, "BEGIN:VEVENT") != 1 {
		t.Fatalf("Maya's link to everything while she is on Launch, never on Hiring:\n%s", body)
	}
	delete(levels[personOf(maya).String()], launch)
	if body := read(); strings.Count(body, "BEGIN:VEVENT") != 0 {
		t.Fatalf("Maya's link to everything after she left Launch:\n%s", body)
	}
}

// basecamp-05: an admin's list with no live link is empty, not null; past
// maxListedFeeds it says there are more; a stop of an id no live link has,
// or of no id at all, is not found and stops nothing.
func TestAnAdminsListIsEmptySaysWhenThereIsMoreAndStopsOnlyALiveLink(t *testing.T) {
	h := newHarness(t)
	feedsOn(t, true)
	ada, maya := h.member("Ada"), h.member("Maya")
	ada.UserPostgresInfo.IsAdmin = true
	ctx := context.Background()
	list, err := h.s.Feeds(ctx, ada)
	if raw, _ := json.Marshal(list); err != nil || !strings.Contains(string(raw), `"feeds":[]`) || list.More {
		t.Fatalf("an empty list: %s %v", raw, err)
	}
	for _, id := range []string{"", "not-an-id", uuid.NewString()} {
		if err := h.s.StopAnyFeed(ctx, ada, id); !errors.Is(err, ErrEventNotFound) {
			t.Errorf("a stop of %q: %v", id, err)
		}
	}
	for i := 0; i <= maxListedFeeds; i++ {
		h.store.feeds = append(h.store.feeds, memFeed{Feed: models.Feed{ID: uuid.New(), CalendarID: uuid.New(), PersonID: personOf(maya), Scope: models.FeedFull, CreatedAt: time.Now()}})
	}
	if list, err = h.s.Feeds(ctx, ada); err != nil || len(list.Feeds) != maxListedFeeds || !list.More {
		t.Fatalf("a list past %d: %d listed, more %v, %v", maxListedFeeds, len(list.Feeds), list.More, err)
	}
}

// basecamp-05 after gcal-14: a channel's and a team's calendar are in Maya's
// link to everything through the same one rule as a project's. The link keeps
// no calendar list: one she joins after making it is in it at the next read,
// and one she leaves is gone at the next; a channel she was never in, and a
// calendar she reads from an address, are never in it.
func TestAWorkspaceFeedCarriesAChannelsAndATeamsCalendarOnlyWhileItsPersonIsInIt(t *testing.T) {
	h := newHarness(t)
	feedsOn(t, true)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	channel, team, other := uuid.NewString(), uuid.NewString(), uuid.NewString()
	levels := map[string]map[string]accessBusiness.Level{
		personOf(jonas).String(): {channel: accessBusiness.LevelEdit, team: accessBusiness.LevelFull, other: accessBusiness.LevelEdit},
		personOf(maya).String():  {},
	}
	projectsDecide(t, levels, map[string]string{channel: "launch", team: "Design", other: "hiring"})
	ctx := context.Background()
	day := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct{ kind, id, title string }{{containerChannel, channel, "Standup"}, {containerTeam, team, "Design crit"}, {containerChannel, other, "Offer call"}} {
		cal, err := h.s.ContainerCalendarFor(ctx, jonas, c.kind, c.id)
		if err != nil {
			t.Fatal(err)
		}
		h.onCalendar(cal, jonas, c.title, day, "")
	}
	_, _ = withAddresses(t, h, map[string]string{teamAddress: teamFile("20261020T100000Z", "Address review", false)})
	if _, err := h.s.Subscribe(ctx, maya, teamAddress, "", ""); err != nil {
		t.Fatal(err)
	}
	made, err := h.s.MakeFeed(ctx, maya, WorkspaceFeedID, models.FeedFull)
	if err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		b, err := h.s.FeedICS(ctx, tokenOf(t, made))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if body := read(); strings.Count(body, "BEGIN:VEVENT") != 0 {
		t.Fatalf("Maya's link to everything before she joined anything (an address's events are never in it):\n%s", body)
	}
	levels[personOf(maya).String()][channel] = accessBusiness.LevelView
	levels[personOf(maya).String()][team] = accessBusiness.LevelEdit
	if body := read(); !strings.Contains(body, "SUMMARY:Standup") || !strings.Contains(body, "SUMMARY:Design crit") || strings.Contains(body, "Offer call") || strings.Contains(body, "Address review") || strings.Count(body, "BEGIN:VEVENT") != 2 {
		t.Fatalf("Maya's link to everything once she joined the channel and the team:\n%s", body)
	}
	delete(levels[personOf(maya).String()], channel)
	if body := read(); strings.Contains(body, "Standup") || !strings.Contains(body, "SUMMARY:Design crit") || strings.Count(body, "BEGIN:VEVENT") != 1 {
		t.Fatalf("Maya's link to everything after she left the channel:\n%s", body)
	}
	delete(levels[personOf(maya).String()], team)
	if body := read(); strings.Count(body, "BEGIN:VEVENT") != 0 {
		t.Fatalf("Maya's link to everything after she left the team:\n%s", body)
	}
}

// basecamp-05 (CR-317): a person's own stop of their link to everything is
// told under the link's id, as its making was, never under the nil uuid.
func TestAPersonsOwnStopOfTheirLinkToEverythingIsToldUnderTheLink(t *testing.T) {
	h := newHarness(t)
	feedsOn(t, true)
	maya := h.member("Maya")
	ctx := context.Background()
	h.emitted = nil
	if _, err := h.s.MakeFeed(ctx, maya, WorkspaceFeedID, models.FeedFull); err != nil {
		t.Fatal(err)
	}
	if err := h.s.StopFeed(ctx, maya, WorkspaceFeedID); err != nil {
		t.Fatal(err)
	}
	var made, stopped *resourceEventBusiness.Event
	for i := range h.emitted {
		switch h.emitted[i].Kind {
		case resourceEventBusiness.CalendarFeedMade:
			made = &h.emitted[i]
		case resourceEventBusiness.CalendarFeedStopped:
			stopped = &h.emitted[i]
		}
	}
	if made == nil || stopped == nil {
		t.Fatalf("told: %+v", h.emitted)
	}
	if stopped.ResourceUUID == uuid.Nil.String() || stopped.ResourceUUID != made.ResourceUUID {
		t.Errorf("own stop told under %q; the link was made under %q", stopped.ResourceUUID, made.ResourceUUID)
	}
}
