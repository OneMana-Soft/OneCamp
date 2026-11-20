package Calendar

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	milestoneModel "github.com/akashc777/OneCamp/models/postgres/Milestone"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A team's calendar fills itself with its members' time off (R-TEAM): each
// member's Away shows as "Away" with its span, read only and linked to the
// person, and nothing else of theirs (no title, note or other event); only
// for members whose busy times the reader may see by the one rule (their
// own always; another's while the workspace shows them and that person
// hasn't hidden theirs); and no write path takes a derived entry's id.
func TestATeamCalendarShowsItsMembersTimeOffAsTheOneRuleLets(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jonas, maya := h.member("Jonas"), h.member("Maya")
	j, m := personOf(jonas).String(), personOf(maya).String()
	team := uuid.NewString()
	projectsDecide(t, map[string]map[string]accessBusiness.Level{
		j: {team: accessBusiness.LevelFull},
		m: {team: accessBusiness.LevelEdit},
	}, map[string]string{team: "Design"})
	prev := teamMembersOf
	t.Cleanup(func() { teamMembersOf = prev })
	teamMembersOf = func(_ context.Context, teams []string) (map[string][]string, error) {
		if len(teams) != 1 || teams[0] != strings.ToLower(team) {
			t.Fatalf("the teams asked: %v", teams)
		}
		return map[string][]string{strings.ToLower(team): {j, m}}, nil
	}
	cal, err := h.s.ContainerCalendarFor(ctx, jonas, containerTeam, team)
	if err != nil {
		t.Fatal(err)
	}
	from, to := oooAt(20, 0), oooAt(23, 0)
	for _, who := range []*model.UserInfo{jonas, maya} {
		if _, err := h.s.Create(ctx, who, EventInput{Kind: EventAway, Title: "Leave: private reason", Notes: "a note",
			Start: When{At: &from, Zone: "UTC"}, End: When{At: &to}}); err != nil {
			t.Fatal(err)
		}
	}
	h.create(maya, "Dentist")
	derived := func(reader *model.UserInfo) map[string]Event {
		t.Helper()
		got, _, err := h.s.CalendarEvents(ctx, reader, []string{cal.String()}, from.Add(-48*time.Hour), to.Add(48*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]Event{}
		for _, e := range got {
			if e.Source == nil {
				t.Errorf("an entry on the team calendar that no one made on it isn't marked derived: %+v", e)
				continue
			}
			out[e.Source.ID] = e
		}
		return out
	}

	freeBusyChoices(t, true, nil)
	got := derived(jonas)
	if len(got) != 2 {
		t.Fatalf("Jonas sees his own and Maya's time off: %v", got)
	}
	for who, e := range got {
		if !e.ReadOnly || e.Title != "Away" || e.Notes != "" || e.Kind != EventAway || e.Source.Kind != sourcePerson ||
			!strings.HasPrefix(e.ID, "away:") || e.CalendarID != cal.String() || e.Start.At == nil || !e.Start.At.Equal(from) || e.End.At == nil || !e.End.At.Equal(to) {
			t.Errorf("%s's time off as derived: %+v", who, e)
		}
	}

	// Maya hides when she's busy: Jonas sees only his own; Maya her own and his.
	freeBusyChoices(t, true, map[string]bool{m: false})
	if got := derived(jonas); len(got) != 1 || got[j].ID == "" {
		t.Errorf("Maya hides, yet Jonas sees: %v", got)
	}
	if got := derived(maya); len(got) != 2 {
		t.Errorf("Maya sees her own and Jonas's: %v", got)
	}
	// The workspace keeps everyone's: each sees their own alone, whatever
	// Jonas chose.
	freeBusyChoices(t, false, map[string]bool{j: true})
	if got := derived(maya); len(got) != 1 || got[m].ID == "" {
		t.Errorf("with the workspace off Maya sees: %v", got)
	}

	// No write path takes a derived id.
	id := derived(jonas)[j].ID
	title := "Mine now"
	if _, err := h.s.Update(ctx, jonas, id, EventPatch{Title: &title}); err == nil {
		t.Error("a derived entry was changed")
	}
	if err := h.s.Delete(ctx, jonas, id, "", nil); err == nil {
		t.Error("a derived entry was deleted")
	}
	if _, err := h.s.Answer(ctx, jonas, id, AnswerInput{}); err == nil {
		t.Error("a derived entry was answered")
	}

	// When who is in the team can't be read, it answers that.
	teamMembersOf = func(context.Context, []string) (map[string][]string, error) {
		return nil, context.DeadlineExceeded
	}
	h.unreadFails(jonas, cal, from, to, "its members")
}

// derivedOn is what reader sees on calendar between from and to that no one
// made on it, by its source ("task:<uuid>", "milestone:<uuid>"), and how many
// such entries there are.
func (h *harness) derivedOn(reader *model.UserInfo, calendar uuid.UUID, from, to time.Time) (map[string]Event, int) {
	h.t.Helper()
	got, _, err := h.s.CalendarEvents(context.Background(), reader, []string{calendar.String()}, from, to)
	if err != nil {
		h.t.Fatal(err)
	}
	out, n := map[string]Event{}, 0
	for _, e := range got {
		if e.Source != nil {
			out[e.Source.Kind+":"+e.Source.ID] = e
			n++
		}
	}
	return out, n
}

// unreadFails fails unless reader's read of calendar between from and to
// answers an error, and nothing of it, while what it shows (what) can't be
// read: never a calendar that looks empty, or part of one.
func (h *harness) unreadFails(reader *model.UserInfo, calendar uuid.UUID, from, to time.Time, what string) {
	h.t.Helper()
	if got, _, err := h.s.CalendarEvents(context.Background(), reader, []string{calendar.String()}, from, to); err == nil || len(got) != 0 {
		h.t.Errorf("%s couldn't be read, yet the calendar answered %d entries (%v)", what, len(got), err)
	}
}

// refusesWrites fails unless every write path refuses the derived entry id.
func (h *harness) refusesWrites(who *model.UserInfo, id string) {
	h.t.Helper()
	ctx := context.Background()
	title := "Mine now"
	if _, err := h.s.Update(ctx, who, id, EventPatch{Title: &title}); err == nil {
		h.t.Errorf("%s was changed", id)
	}
	if err := h.s.Delete(ctx, who, id, "", nil); err == nil {
		h.t.Errorf("%s was deleted", id)
	}
	if _, err := h.s.Answer(ctx, who, id, AnswerInput{}); err == nil {
		h.t.Errorf("%s was answered", id)
	}
}

// windowClause is the task list's own condition for tasks dated in the
// window: the one both task sources must ask with.
func windowClause(t *testing.T, from, to time.Time) string {
	t.Helper()
	c, err := taskDomain.TaskListFilter{From: &from, To: &to}.Clause()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A project's calendar fills itself from its work: each task dated in the
// window that the reader may read (the Access Resolver's task kind) as an
// all-day entry over its days, start to due, and each milestone on its
// target day, read only and linked to its source. A task the reader may not
// read, an undated task and a milestone with no day or outside the window
// show nothing, and no write path takes their ids.
func TestAProjectCalendarShowsTheTasksAndMilestonesItsReaderMayRead(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jonas, maya := h.member("Jonas"), h.member("Maya")
	j, m := personOf(jonas).String(), personOf(maya).String()
	project := uuid.NewString()
	spanned, restricted, undated := uuid.NewString(), uuid.NewString(), uuid.NewString()
	projectsDecide(t, map[string]map[string]accessBusiness.Level{
		j: {project: accessBusiness.LevelFull, spanned: accessBusiness.LevelEdit, restricted: accessBusiness.LevelView, undated: accessBusiness.LevelEdit},
		m: {project: accessBusiness.LevelView, spanned: accessBusiness.LevelView},
	}, map[string]string{project: "Launch"})
	from, to := oooAt(1, 0), oooAt(31, 0)
	start, due := oooAt(20, 9), oooAt(22, 17)
	prevTasks, prevMilestones := projectTasksIn, milestonesIn
	t.Cleanup(func() { projectTasksIn, milestonesIn = prevTasks, prevMilestones })
	projectTasksIn = func(_ context.Context, projects []string, cond string) (map[string][]*dgraphStruct.DgraphTask, error) {
		if len(projects) != 1 || projects[0] != strings.ToLower(project) || cond != windowClause(t, from, to) {
			t.Errorf("the tasks asked for: %v %q", projects, cond)
		}
		return map[string][]*dgraphStruct.DgraphTask{strings.ToLower(project): {
			{Uuid: spanned, Name: "Press kit", StartDate: &start, DueDate: &due},
			{Uuid: restricted, Name: "Salaries", DueDate: &due},
			{Uuid: undated, Name: "Someday"},
		}}, nil
	}
	beta, late, someday := uuid.New(), uuid.New(), uuid.New()
	onDay, after := "2026-10-25", "2026-12-01"
	milestonesIn = func(_ context.Context, projects []uuid.UUID) (map[uuid.UUID][]milestoneModel.Milestone, error) {
		p := uuid.MustParse(project)
		if len(projects) != 1 || projects[0] != p {
			t.Errorf("the milestones asked for: %v", projects)
		}
		return map[uuid.UUID][]milestoneModel.Milestone{p: {
			{Id: beta, ProjectUUID: p, Name: "Beta", TargetDate: &onDay},
			{Id: late, ProjectUUID: p, Name: "GA", TargetDate: &after},
			{Id: someday, ProjectUUID: p, Name: "Someday"},
		}}, nil
	}
	cal, err := h.s.ContainerCalendarFor(ctx, jonas, containerProject, project)
	if err != nil {
		t.Fatal(err)
	}

	got, n := h.derivedOn(jonas, cal, from, to)
	if n != 3 || got["task:"+spanned].ID == "" || got["task:"+restricted].ID == "" || got["milestone:"+beta.String()].ID == "" {
		t.Fatalf("Jonas sees both dated tasks and the milestone in the window: %v", got)
	}
	day := func(e Event, kind, title, first, after string) {
		t.Helper()
		if e.ID != e.Source.Kind+":"+e.Source.ID || e.Source.Kind != kind || !e.ReadOnly || !e.AllDay || e.Kind != EventPlain || e.Title != title ||
			e.CalendarID != cal.String() || e.Start.At != nil || e.Start.Date != first || e.End.Date != after || e.Transparency != transparencyTransparent {
			t.Errorf("%s as derived: %+v", title, e)
		}
	}
	day(got["task:"+spanned], sourceTask, "Press kit", "2026-10-20", "2026-10-23")
	day(got["task:"+restricted], sourceTask, "Salaries", "2026-10-22", "2026-10-23")
	day(got["milestone:"+beta.String()], sourceMilestone, "Beta", "2026-10-25", "2026-10-26")

	// Maya may not read the restricted task: she sees nothing of it.
	if got, n := h.derivedOn(maya, cal, from, to); n != 2 || got["task:"+spanned].ID == "" || got["milestone:"+beta.String()].ID == "" {
		t.Errorf("Maya sees: %v", got)
	}
	h.refusesWrites(jonas, "task:"+spanned)
	h.refusesWrites(jonas, "milestone:"+beta.String())

	// When its tasks or its milestones can't be read, it answers that.
	readTasks := projectTasksIn
	projectTasksIn = func(context.Context, []string, string) (map[string][]*dgraphStruct.DgraphTask, error) {
		return nil, context.DeadlineExceeded
	}
	h.unreadFails(jonas, cal, from, to, "its tasks")
	projectTasksIn = readTasks
	milestonesIn = func(context.Context, []uuid.UUID) (map[uuid.UUID][]milestoneModel.Milestone, error) {
		return nil, context.DeadlineExceeded
	}
	h.unreadFails(jonas, cal, from, to, "its milestones")
}

// A channel's calendar fills itself with the tasks its messages link to
// (R-CHANNEL): each once however many messages link it, as the same entry a
// project's calendar shows, and only to a reader who may read the task.
func TestAChannelCalendarShowsTheTasksItsMessagesLinkTo(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jonas, maya := h.member("Jonas"), h.member("Maya")
	j, m := personOf(jonas).String(), personOf(maya).String()
	channel := uuid.NewString()
	linked, restricted := uuid.NewString(), uuid.NewString()
	projectsDecide(t, map[string]map[string]accessBusiness.Level{
		j: {channel: accessBusiness.LevelEdit, linked: accessBusiness.LevelView, restricted: accessBusiness.LevelView},
		m: {channel: accessBusiness.LevelView, linked: accessBusiness.LevelView},
	}, map[string]string{channel: "General"})
	from, to := oooAt(1, 0), oooAt(31, 0)
	due := oooAt(14, 12)
	prev := channelTasksIn
	t.Cleanup(func() { channelTasksIn = prev })
	channelTasksIn = func(_ context.Context, channels []string, cond string) (map[string][]*dgraphStruct.DgraphTask, error) {
		if len(channels) != 1 || channels[0] != strings.ToLower(channel) || cond != windowClause(t, from, to) {
			t.Errorf("the links asked for: %v %q", channels, cond)
		}
		task := dgraphStruct.DgraphTask{Uuid: linked, Name: "Press kit", DueDate: &due}
		again := task
		return map[string][]*dgraphStruct.DgraphTask{strings.ToLower(channel): {&task, {Uuid: restricted, Name: "Salaries", DueDate: &due}, &again}}, nil
	}
	cal, err := h.s.ContainerCalendarFor(ctx, jonas, containerChannel, channel)
	if err != nil {
		t.Fatal(err)
	}
	got, n := h.derivedOn(jonas, cal, from, to)
	e := got["task:"+linked]
	if n != 2 || got["task:"+restricted].ID == "" || e.ID != "task:"+linked || !e.ReadOnly || !e.AllDay || e.Title != "Press kit" ||
		e.CalendarID != cal.String() || e.Start.Date != "2026-10-14" || e.End.Date != "2026-10-15" {
		t.Fatalf("Jonas sees each linked task once: %v", got)
	}
	if got, n := h.derivedOn(maya, cal, from, to); n != 1 || got["task:"+linked].ID == "" {
		t.Errorf("Maya, who may not read one of them, sees: %v", got)
	}
	h.refusesWrites(maya, "task:"+linked)

	// When its messages' links can't be read, it answers that.
	channelTasksIn = func(context.Context, []string, string) (map[string][]*dgraphStruct.DgraphTask, error) {
		return nil, context.DeadlineExceeded
	}
	h.unreadFails(jonas, cal, from, to, "its links")
}

// A token (an API token, or an MCP agent on one) reads a calendar by
// calendar:read, but a project's and a channel's tasks and milestones only
// with the scopes that read those on every other route (main 13 Nov, CR-B84
// F1): calendar:read alone is answered neither, and neither is read;
// tasks:read adds the tasks its member may read, projects:read the
// milestones. Work a token queued holds no scope, so it gets neither. A
// team's time off stays under calendar:read, by the one free/busy rule, and
// a person in the app is answered all three.
func TestATokenSeesTasksAndMilestonesOnlyWithTheScopesThatReadThem(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jonas, maya := h.member("Jonas"), h.member("Maya")
	j, m := personOf(jonas).String(), personOf(maya).String()
	project, channel, team := uuid.NewString(), uuid.NewString(), uuid.NewString()
	open, restricted, linked := uuid.NewString(), uuid.NewString(), uuid.NewString()
	view := accessBusiness.LevelView
	projectsDecide(t, map[string]map[string]accessBusiness.Level{
		j: {project: accessBusiness.LevelFull, channel: accessBusiness.LevelEdit, team: accessBusiness.LevelFull, open: view, restricted: view, linked: view},
		m: {project: view, channel: view, team: accessBusiness.LevelEdit, open: view, linked: view},
	}, map[string]string{project: "Launch", channel: "General", team: "Design"})
	from, to := oooAt(1, 0), oooAt(31, 0)
	due, onDay, beta := oooAt(14, 12), "2026-10-25", uuid.New()
	reads := 0
	prevTasks, prevMarks, prevLinks, prevTeams := projectTasksIn, milestonesIn, channelTasksIn, teamMembersOf
	t.Cleanup(func() {
		projectTasksIn, milestonesIn, channelTasksIn, teamMembersOf = prevTasks, prevMarks, prevLinks, prevTeams
	})
	projectTasksIn = func(context.Context, []string, string) (map[string][]*dgraphStruct.DgraphTask, error) {
		reads++
		return map[string][]*dgraphStruct.DgraphTask{strings.ToLower(project): {{Uuid: open, Name: "Press kit", DueDate: &due}, {Uuid: restricted, Name: "Salaries", DueDate: &due}}}, nil
	}
	milestonesIn = func(context.Context, []uuid.UUID) (map[uuid.UUID][]milestoneModel.Milestone, error) {
		reads++
		p := uuid.MustParse(project)
		return map[uuid.UUID][]milestoneModel.Milestone{p: {{Id: beta, ProjectUUID: p, Name: "Beta", TargetDate: &onDay}}}, nil
	}
	channelTasksIn = func(context.Context, []string, string) (map[string][]*dgraphStruct.DgraphTask, error) {
		reads++
		return map[string][]*dgraphStruct.DgraphTask{strings.ToLower(channel): {{Uuid: linked, Name: "Launch post", DueDate: &due}}}, nil
	}
	teamMembersOf = func(context.Context, []string) (map[string][]string, error) {
		return map[string][]string{strings.ToLower(team): {j, m}}, nil
	}
	freeBusyChoices(t, true, nil)
	kinds := []string{containerProject, containerChannel, containerTeam}
	cals := map[string]string{}
	for i, id := range []string{project, channel, team} {
		cal, err := h.s.ContainerCalendarFor(ctx, jonas, kinds[i], id)
		if err != nil {
			t.Fatal(err)
		}
		cals[kinds[i]] = cal.String()
	}
	away, back := oooAt(20, 0), oooAt(21, 0)
	if _, err := h.s.Create(ctx, maya, EventInput{Kind: EventAway, Title: "Leave", Start: When{At: &away, Zone: "UTC"}, End: When{At: &back}}); err != nil {
		t.Fatal(err)
	}

	token := func(scopes ...string) context.Context {
		c := context.WithValue(ctx, helpers.ApiTokenIDContextKey, uuid.NewString())
		return context.WithValue(c, helpers.ApiScopesContextKey, scopes)
	}
	// seen is every derived entry reader sees on the three calendars, asked
	// in c, as "<calendar kind> <source kind>:<source id>", sorted.
	seen := func(c context.Context, reader *model.UserInfo) string {
		t.Helper()
		var got []string
		for _, kind := range kinds {
			es, _, err := h.s.CalendarEvents(c, reader, []string{cals[kind]}, from, to)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range es {
				if e.Source != nil {
					got = append(got, kind+" "+e.Source.Kind+":"+e.Source.ID)
				}
			}
		}
		sort.Strings(got)
		return strings.Join(got, ", ")
	}
	list := func(entries ...string) string {
		sort.Strings(entries)
		return strings.Join(entries, ", ")
	}
	task := func(kind, id string) string { return kind + " " + sourceTask + ":" + id }
	mark, off := containerProject+" "+sourceMilestone+":"+beta.String(), containerTeam+" "+sourcePerson+":"+m
	read, tasks, projects := "calendar:read", "tasks:read", "projects:read"
	for _, c := range []struct {
		what   string
		ctx    context.Context
		reader *model.UserInfo
		want   string
		reads  int
	}{
		{"Jonas in the app", ctx, jonas, list(task(containerProject, open), task(containerProject, restricted), mark, task(containerChannel, linked), off), 3},
		{"Jonas's calendar:read token", token(read), jonas, off, 0},
		{"Maya's calendar:read token", token(read), maya, off, 0},
		{"Jonas's token with tasks:read", token(read, tasks), jonas, list(task(containerProject, open), task(containerProject, restricted), task(containerChannel, linked), off), 2},
		{"Maya's token with tasks:read", token(read, tasks), maya, list(task(containerProject, open), task(containerChannel, linked), off), 2},
		{"Jonas's token with projects:read", token(read, projects), jonas, list(mark, off), 1},
		{"Maya's token with all three", token(read, tasks, projects), maya, list(task(containerProject, open), mark, task(containerChannel, linked), off), 3},
		{"work Jonas's token queued", helpers.WithActsThroughToken(ctx), jonas, off, 0},
	} {
		reads = 0
		if got := seen(c.ctx, c.reader); got != c.want || reads != c.reads {
			t.Errorf("%s sees %q in %d reads, want %q in %d", c.what, got, reads, c.want, c.reads)
		}
	}
}

// A task's and a time off's entries send rooms as [], never null, as every
// other event does.
func TestDerivedEntriesSendNoRoomsAsAnEmptyList(t *testing.T) {
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for name, e := range map[string]Event{
		"a task":     dayEntry("cal", "task", "t1", "Ship it", "2026-11-16", "2026-11-17"),
		"a time off": awayEntry("cal", "p1", start, start.Add(24*time.Hour)),
	} {
		raw, err := json.Marshal(e)
		if err != nil || !strings.Contains(string(raw), `"rooms":[]`) {
			t.Errorf("%s's entry: %s (%v), want rooms []", name, raw, err)
		}
	}
}
