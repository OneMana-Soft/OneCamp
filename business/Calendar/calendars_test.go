package Calendar

import (
	"context"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// projectsDecide answers the Access Resolver for projects: each person's
// level on each project, as the project's own rule would.
func projectsDecide(t *testing.T, levels map[string]map[string]accessBusiness.Level, titles map[string]string) {
	t.Helper()
	prevResolve, prevRead := resolveAllFn, readEntityFn
	resolveAllFn = func(_ context.Context, who accessBusiness.Subject, res []accessBusiness.Resource) (map[accessBusiness.Resource]accessBusiness.Decision, error) {
		out := map[accessBusiness.Resource]accessBusiness.Decision{}
		for _, r := range res {
			out[r] = accessBusiness.Decision{Level: levels[who.UserUUID][r.UUID]}
		}
		return out, nil
	}
	readEntityFn = func(_ context.Context, who accessBusiness.Subject, refs []entityLinkBusiness.Ref) ([]entityLinkBusiness.Entity, error) {
		var out []entityLinkBusiness.Entity
		for _, r := range refs {
			e := entityLinkBusiness.Entity{Kind: r.Kind, ID: r.ID, Access: entityLinkBusiness.AccessNone}
			if levels[who.UserUUID][r.ID] >= accessBusiness.LevelView {
				e.Access, e.Title = entityLinkBusiness.AccessOK, titles[r.ID]
			}
			out = append(out, e)
		}
		return out, nil
	}
	t.Cleanup(func() { resolveAllFn, readEntityFn = prevResolve, prevRead })
}

func (h *harness) onCalendar(calendar uuid.UUID, by *model.UserInfo, title string, start time.Time, rule string) *models.Event {
	h.t.Helper()
	e := blank("Europe/London")
	e.Title = title
	end := start.Add(time.Hour)
	e.StartAt, e.EndAt = &start, &end
	if rule != "" {
		if err := setRule(e, rule); err != nil {
			h.t.Fatal(err)
		}
	}
	e.ID, e.CalendarID, e.CreatedBy, e.ICalUID = uuid.New(), calendar, personOf(by), uuid.NewString()+"@onecamp"
	e.CreatedAt, e.UpdatedAt = h.clock, h.clock
	if err := h.store.create(context.Background(), e, nil); err != nil {
		h.t.Fatal(err)
	}
	return e
}

// A project has one calendar, made the first time, for those who may see
// the project; the calendar is as the project is to each person.
func TestAProjectsCalendarIsAsTheProjectIs(t *testing.T) {
	h := newHarness(t)
	jonas, maya, lee := h.member("Jonas"), h.member("Maya"), h.member("Lee")
	project := uuid.NewString()
	projectsDecide(t, map[string]map[string]accessBusiness.Level{
		personOf(jonas).String(): {project: accessBusiness.LevelFull},
		personOf(maya).String():  {project: accessBusiness.LevelView},
	}, map[string]string{project: "Launch"})

	cal, err := h.s.ProjectCalendarFor(context.Background(), jonas, project)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.s.ProjectCalendarFor(context.Background(), maya, strings.ToUpper(project))
	if err != nil || again != cal {
		t.Fatalf("one calendar a project: %s, then %s (%v)", cal, again, err)
	}
	if _, err := h.s.ProjectCalendarFor(context.Background(), lee, project); err != ErrEventNotFound {
		t.Fatalf("someone who can't see the project made its calendar: %v", err)
	}

	own, _ := h.store.personalCalendar(context.Background(), personOf(jonas), "UTC")
	for _, c := range []struct {
		who   *model.UserInfo
		id    uuid.UUID
		level accessBusiness.Level
		owner bool
	}{
		{jonas, cal, accessBusiness.LevelEdit, false},
		{maya, cal, accessBusiness.LevelView, false},
		{lee, cal, accessBusiness.LevelNone, false},
		{jonas, own, accessBusiness.LevelFull, true},
		// Another member's own calendar: when they're busy, and nothing
		// more (owner decision O7; TestAPersonsCalendarIsBusyUntilTheyShareIt).
		{maya, own, accessBusiness.LevelView, false},
	} {
		got, err := h.s.decideCalendars(context.Background(), subjectOf(c.who), []string{c.id.String()})
		if err != nil {
			t.Fatal(err)
		}
		d := got[c.id.String()]
		if d.Level != c.level || d.Owner != c.owner {
			t.Errorf("%s on %s: %+v, want %v (owner %v)", c.who.UserDgraphInfo.UserName, c.id, d, c.level, c.owner)
		}
	}
}

// A calendar's chip names it and its next three occurrences, for someone
// who may see it; someone who may not learns nothing of it.
func TestACalendarsChipNamesItsNextThreeEvents(t *testing.T) {
	h := newHarness(t)
	jonas, lee := h.member("Jonas"), h.member("Lee")
	project := uuid.NewString()
	projectsDecide(t, map[string]map[string]accessBusiness.Level{personOf(jonas).String(): {project: accessBusiness.LevelEdit}}, map[string]string{project: "Launch"})
	cal, err := h.s.ProjectCalendarFor(context.Background(), jonas, project)
	if err != nil {
		t.Fatal(err)
	}
	day := h.clock.Truncate(24 * time.Hour).Add(24 * time.Hour)
	h.onCalendar(cal, jonas, "Past review", h.clock.Add(-48*time.Hour), "")
	h.onCalendar(cal, jonas, "Standup", day.Add(9*time.Hour), "FREQ=DAILY;COUNT=10")
	h.onCalendar(cal, jonas, "Release", day.Add(9*time.Hour+30*time.Minute), "")

	chips, err := h.s.calendarChips(context.Background(), subjectOf(jonas), []string{cal.String()})
	if err != nil {
		t.Fatal(err)
	}
	chip := chips[cal.String()]
	next, _ := chip.Data["next"].([]ChipEvent)
	if chip.Access != entityLinkBusiness.AccessOK || chip.Data["name"] != "Launch" || len(next) != 3 {
		t.Fatalf("chip %+v", chip)
	}
	var titles []string
	for _, e := range next {
		titles = append(titles, e.Title)
	}
	if strings.Join(titles, ", ") != "Standup, Release, Standup" {
		t.Fatalf("next %v", titles)
	}
	hidden, err := h.s.calendarChips(context.Background(), subjectOf(lee), []string{cal.String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hidden[cal.String()]; ok {
		t.Fatalf("Lee learnt of the calendar: %+v", hidden)
	}
}

// Many calendars' chips read their events once between them, and each keeps
// its own next three: one calendar's daily standup never crowds out
// another's panel.
func TestChipsOfManyCalendarsReadTheirEventsOnce(t *testing.T) {
	h := newHarness(t)
	jonas := h.member("Jonas")
	launch, hiring := uuid.NewString(), uuid.NewString()
	projectsDecide(t, map[string]map[string]accessBusiness.Level{personOf(jonas).String(): {launch: accessBusiness.LevelEdit, hiring: accessBusiness.LevelView}},
		map[string]string{launch: "Launch", hiring: "Hiring"})
	ctx := context.Background()
	a, err := h.s.ProjectCalendarFor(ctx, jonas, launch)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.s.ProjectCalendarFor(ctx, jonas, hiring)
	if err != nil {
		t.Fatal(err)
	}
	day := h.clock.Truncate(24 * time.Hour).Add(24 * time.Hour)
	h.onCalendar(a, jonas, "Standup", day.Add(9*time.Hour), "FREQ=DAILY;COUNT=30")
	h.onCalendar(b, jonas, "Panel", day.AddDate(0, 0, 10).Add(14*time.Hour), "")

	h.store.resetReads()
	chips, err := h.s.calendarChips(ctx, subjectOf(jonas), []string{a.String(), b.String()})
	if err != nil {
		t.Fatal(err)
	}
	titles := func(id uuid.UUID) string {
		next, _ := chips[id.String()].Data["next"].([]ChipEvent)
		var out []string
		for _, e := range next {
			out = append(out, e.Title)
		}
		return strings.Join(out, ", ")
	}
	if got := titles(a); got != "Standup, Standup, Standup" {
		t.Fatalf("Launch's next: %q", got)
	}
	if got := titles(b); got != "Panel" {
		t.Fatalf("Hiring's next: %q", got)
	}
	if n := h.store.readsOf("nextOf"); n != 1 {
		t.Fatalf("two calendars' next events were read %d times, want once", n)
	}
}

// A project's members see its calendar in their list and its events; an
// editor puts events on it; someone outside it sees neither, and can't.
func TestAProjectCalendarsEventsAreItsMembers(t *testing.T) {
	h := newHarness(t)
	jonas, maya, lee := h.member("Jonas"), h.member("Maya"), h.member("Lee")
	project := uuid.NewString()
	projectsDecide(t, map[string]map[string]accessBusiness.Level{
		personOf(jonas).String(): {project: accessBusiness.LevelEdit},
		personOf(maya).String():  {project: accessBusiness.LevelView},
	}, map[string]string{project: "Launch"})
	cal, err := h.s.ProjectCalendarFor(context.Background(), jonas, project)
	if err != nil {
		t.Fatal(err)
	}
	start, end := timed(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), 60, "Europe/London")
	made, err := h.s.Create(context.Background(), jonas, EventInput{Title: "Release", Start: start, End: end, CalendarID: cal.String()})
	if err != nil {
		t.Fatal(err)
	}
	if h.store.events[uuid.MustParse(made.ID)].CalendarID != cal {
		t.Fatal("the event isn't on the project's calendar")
	}
	if _, err := h.s.Create(context.Background(), maya, EventInput{Title: "Mine", Start: start, End: end, CalendarID: cal.String()}); err == nil || !strings.Contains(err.Error(), "can't add events") {
		t.Fatalf("a viewer added an event: %v", err)
	}

	list, err := h.s.Calendars(context.Background(), maya)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range list {
		names = append(names, c.Name+"/"+c.Access)
	}
	if strings.Join(names, ",") != "Personal/manage,Launch/view" {
		t.Fatalf("Maya's calendars %v", names)
	}
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	evs, _, err := h.s.CalendarEvents(context.Background(), maya, []string{cal.String()}, from, to)
	if err != nil || len(evs) != 1 || evs[0].Title != "Release" {
		t.Fatalf("Maya's view of the calendar: %+v %v", evs, err)
	}
	if got, _ := h.s.decideMany(context.Background(), subjectOf(maya), []string{made.ID}); got[made.ID].Level != accessBusiness.LevelView {
		t.Fatalf("Maya can't open an event on the calendar: %+v", got)
	}
	if evs, _, _ := h.s.CalendarEvents(context.Background(), lee, []string{cal.String()}, from, to); len(evs) != 0 {
		t.Fatalf("Lee saw %+v", evs)
	}
	if got, _ := h.s.decideMany(context.Background(), subjectOf(lee), []string{made.ID}); got[made.ID].Level != accessBusiness.LevelNone {
		t.Fatalf("Lee can open the event: %+v", got)
	}
	if list, _ := h.s.Calendars(context.Background(), lee); len(list) != 1 {
		t.Fatalf("Lee's calendars %+v", list)
	}
}

// A6's acceptance: Maya adds Jonas and sees his busy blocks with no titles;
// Jonas shares details with her and titles appear, but his private event
// reads "Busy"; free time never shows as busy; only he shares his calendar.
func TestAPersonsCalendarIsBusyUntilTheyShareIt(t *testing.T) {
	h := newHarness(t)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	cal, err := h.s.PersonCalendar(ctx, maya, personOf(jonas).String())
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	mk := func(title string, h0 int, tweak func(*EventInput)) {
		start, end := timed(day.Add(time.Duration(h0)*time.Hour), 60, "Europe/London")
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
	read := func() []string {
		t.Helper()
		evs, _, err := h.s.CalendarEvents(ctx, maya, []string{cal.String()}, day, day.AddDate(0, 0, 1))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range evs {
			out = append(out, e.Title+"@"+e.Start.At.UTC().Format("15"))
		}
		return out
	}
	if got := strings.Join(read(), ","); got != "Busy@09,Busy@11" {
		t.Fatalf("Maya sees %s before a share", got)
	}
	if err := h.s.Share(ctx, maya, cal.String(), personOf(maya).String(), models.ShareDetails); err != ErrEventNotFound {
		t.Fatalf("Maya shared Jonas's calendar: %v", err)
	}
	if err := h.s.Share(ctx, jonas, cal.String(), personOf(maya).String(), models.ShareDetails); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(read(), ","); got != "Pricing review@09,Busy@11,Focus reading@14" {
		t.Fatalf("Maya sees %s with details", got)
	}
	// What she sees as Busy is named for her nowhere else: not by a link, a
	// chip or a notice (audit A12). An event she sees whole is.
	his, _, err := h.s.CalendarEvents(ctx, jonas, []string{cal.String()}, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]string{}
	for _, e := range his {
		byTitle[e.Title] = e.ID
	}
	chips, err := h.s.eventChips(ctx, subjectOf(maya), []string{byTitle["Doctor"], byTitle["Pricing review"]})
	if err != nil || chips[byTitle["Doctor"]].Title != "" || chips[byTitle["Pricing review"]].Title != "Pricing review" {
		t.Fatalf("the titles named for Maya: %v %v", chips, err)
	}
	// Someone who may change the calendar sees a private event on it, as in
	// Google Calendar.
	if err := h.s.Share(ctx, jonas, cal.String(), personOf(maya).String(), models.ShareEdit); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(read(), ","); got != "Pricing review@09,Doctor@11,Focus reading@14" {
		t.Fatalf("Maya sees %s when she may change it", got)
	}
	if err := h.s.Share(ctx, jonas, cal.String(), personOf(maya).String(), models.ShareDetails); err != nil {
		t.Fatal(err)
	}
	shares, err := h.s.Shares(ctx, jonas, cal.String())
	if err != nil || len(shares) != 1 || shares[0].Level != models.ShareDetails || shares[0].Person.UUID != personOf(maya).String() {
		t.Fatalf("shares %+v %v", shares, err)
	}
	list, _ := h.s.Calendars(ctx, maya)
	found := false
	for _, c := range list {
		found = found || (c.ID == cal.String() && c.Access == "view")
	}
	if !found {
		t.Fatalf("a calendar shared with Maya isn't in her list: %+v", list)
	}
	if err := h.s.Share(ctx, jonas, cal.String(), personOf(maya).String(), ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(read(), ","); got != "Busy@09,Busy@11" {
		t.Fatalf("Maya sees %s after the share ends", got)
	}
}

// A workspace that turns members' free and busy off shows a teammate's
// unshared calendar to nobody; one shared still shows as shared.
func TestAWorkspaceCanTurnFreeBusyOff(t *testing.T) {
	h := newHarness(t)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	prev := freeBusyPolicy
	freeBusyPolicy = func(context.Context) bool { return false }
	t.Cleanup(func() { freeBusyPolicy = prev })
	cal, err := h.s.PersonCalendar(ctx, maya, personOf(jonas).String())
	if err != nil {
		t.Fatal(err)
	}
	start, end := timed(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), 60, "Europe/London")
	if _, err := h.s.Create(ctx, jonas, EventInput{Title: "Pricing review", Start: start, End: end}); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	if evs, _, _ := h.s.CalendarEvents(ctx, maya, []string{cal.String()}, day, day.AddDate(0, 0, 1)); len(evs) != 0 {
		t.Fatalf("Maya saw %+v with free and busy off", evs)
	}
	if err := h.s.Share(ctx, jonas, cal.String(), personOf(maya).String(), models.ShareDetails); err != nil {
		t.Fatal(err)
	}
	if evs, _, _ := h.s.CalendarEvents(ctx, maya, []string{cal.String()}, day, day.AddDate(0, 0, 1)); len(evs) != 1 || evs[0].Title != "Pricing review" {
		t.Fatalf("a shared calendar with free and busy off: %+v", evs)
	}
}

// A calendar's manager renames and recolours it; nobody else can, and your
// own keeps its name.
func TestACalendarsManagerRenamesAndRecoloursIt(t *testing.T) {
	h := newHarness(t)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	own, _ := h.store.personalCalendar(ctx, personOf(jonas), "UTC")
	moss, name := "moss", "Work"
	if err := h.s.UpdateCalendar(ctx, jonas, own.String(), CalendarPatch{Hue: &moss}); err != nil {
		t.Fatal(err)
	}
	if got := h.store.calendarEdits[own]; got[1] != "moss" {
		t.Fatalf("colour %v", got)
	}
	if err := h.s.UpdateCalendar(ctx, jonas, own.String(), CalendarPatch{Name: &name}); err == nil || !strings.Contains(err.Error(), "keeps its name") {
		t.Fatalf("renamed your own: %v", err)
	}
	pink := "pink"
	if err := h.s.UpdateCalendar(ctx, jonas, own.String(), CalendarPatch{Hue: &pink}); err == nil {
		t.Fatal("a colour that isn't one of the calendar's was kept")
	}
	if err := h.s.UpdateCalendar(ctx, maya, own.String(), CalendarPatch{Hue: &moss}); err != ErrEventNotFound {
		t.Fatalf("Maya recoloured Jonas's calendar: %v", err)
	}
}

// A calendar shared with you is named for whose it is (Jonas's own reads
// "Jonas", never "Personal" or "Calendar"); one an import made keeps its
// name; your own are marked yours, so the web lists them under My
// calendars and the rest under Other calendars.
func TestACalendarSharedWithYouIsNamedForItsOwner(t *testing.T) {
	h := newHarness(t)
	jonas, maya := h.member("Jonas"), h.member("Maya")
	ctx := context.Background()
	jonasOwn, err := h.s.PersonCalendar(ctx, maya, personOf(jonas).String())
	if err != nil {
		t.Fatal(err)
	}
	family, err := h.store.importedCalendar(ctx, personOf(jonas), "Family", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	mayaFamily, err := h.store.importedCalendar(ctx, personOf(maya), "Holidays", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{jonasOwn, family} {
		if err := h.s.Share(ctx, jonas, id.String(), personOf(maya).String(), models.ShareDetails); err != nil {
			t.Fatal(err)
		}
	}
	list, err := h.s.Calendars(ctx, maya)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]CalendarView{}
	for _, c := range list {
		got[c.ID] = c
	}
	mayaOwn, _ := h.store.personalCalendar(ctx, personOf(maya), "UTC")
	want := map[uuid.UUID]struct {
		name string
		mine bool
	}{
		mayaOwn:    {"Personal", true},
		mayaFamily: {"Holidays", true},
		jonasOwn:   {h.people[personOf(jonas).String()].DisplayName(), false},
		family:     {"Family", false},
	}
	for id, w := range want {
		c, ok := got[id.String()]
		if !ok || c.Name != w.name || c.Mine != w.mine {
			t.Errorf("%s: %+v (listed %v), want %q mine %v", id, c, ok, w.name, w.mine)
		}
	}
}
