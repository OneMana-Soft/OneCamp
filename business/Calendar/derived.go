package Calendar

// Calendars that fill themselves (gcal-14 (d)): entries a container's
// calendar shows that nobody made on it, worked out at read time from their
// one source and never stored. Each is read only, names its source (Source),
// and shows only to a reader who may read that source. Their ids are
// "<what>:<source id>:<start>" (time off: "away:<person>:<unix start>"), never an event's uuid, so no write path
// takes one (parseRef refuses them).
//
// A team calendar's are its members' time off (R-TEAM, CAL-LEAD-71): each
// member's Away as "Away" with its span, only for members whose busy times
// the reader may see by the one rule (busySeenOf), and never any other
// event, title or note of theirs. Events made on the team calendar itself
// are its own, as any calendar's are.
//
// A project's are its work: each task dated in the window and each milestone
// with a day in it. A channel's are the tasks its live messages link to (the
// work graph's refers_to; R-CHANNEL, CAL-LEAD-72); the docs and projects they
// link to add nothing. A task shows only to a reader who may read it (the
// Access Resolver's task kind), by the one function every source turns tasks
// into entries through (taskEntries); an undated or deleted one shows nothing.

import (
	"context"
	"strconv"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	teamDomain "github.com/akashc777/OneCamp/domain/Team"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	milestoneModel "github.com/akashc777/OneCamp/models/postgres/Milestone"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// What a derived entry comes from: a person (their time off), a task (also
// the Access Resolver's kind for one, business/WorkAccess KindTask) or a
// milestone.
const (
	sourcePerson    = "person"
	sourceTask      = "task"
	sourceMilestone = "milestone"
)

// EventSource is where a derived entry comes from, to link to it.
type EventSource struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// teamMembersOf is each of teams' (lower-case uuids) members' user uuids,
// in one read of the live teams; tests swap it.
var teamMembersOf = func(ctx context.Context, teams []string) (map[string][]string, error) {
	got, err := teamDomain.TeamsWithMembers(ctx, teams)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, tm := range got {
		if tm == nil {
			continue
		}
		key := strings.ToLower(tm.Uuid)
		for _, m := range tm.Members {
			if m != nil && m.Uuid != "" {
				out[key] = append(out[key], strings.ToLower(m.Uuid))
			}
		}
	}
	return out, nil
}

// The work a project's and a channel's calendars show, each in one read for
// all of them, by lower-case project or channel uuid; tests swap them.
// Restricted tasks the reader of ctx may not see are left out of both
// (models/taskread), and taskEntries still asks of each task.
var (
	projectTasksIn = func(ctx context.Context, projects []string, cond string) (map[string][]*dgraphStruct.DgraphTask, error) {
		return taskDomain.TasksIn(ctx, projects, cond, taskDomain.MaxOpenTasks)
	}
	channelTasksIn = channelDomain.LinkedTasks
	milestonesIn   = milestoneModel.In
)

// derivedKinds is whether a call may be answered a project's and a
// channel's tasks, and a project's milestones, by the credential it came with
// (main 13 Nov, CR-B84 F1): the one rule, asked by derivedEntries for every
// surface a credential reaches (a calendar link carries none, so its tasks
// skip it: projectFeedTasks). A person in the app is answered
// both, each only as they may read its source. An API token, or an MCP agent
// on one (helpers.ActsThroughToken), reads a calendar by calendar:read, but a
// task and a milestone are what tasks:read and projects:read gate on every
// other route: it is answered tasks only when it also holds tasks:read, and
// milestones only with projects:read, then only those its member may read.
// Work a token queued holds no scope, so it is answered neither. Time off is
// not asked here: it is when someone is away, which busySeenOf decides for
// the token's member as for anyone, under calendar:read.
func derivedKinds(ctx context.Context) (tasks, milestones bool) {
	if !helpers.ActsThroughToken(ctx) {
		return true, true
	}
	granted := helpers.GetApiScopes(ctx)
	return apiTokenBusiness.HasScope(granted, apiTokenBusiness.ScopeTasksRead), apiTokenBusiness.HasScope(granted, apiTokenBusiness.ScopeProjectsRead)
}

// derivedEntries are the derived entries between from and to on calendars
// (those who may already see) for who, of the kinds its credential reads
// (derivedKinds): no source it may not be answered is read.
func (s *service) derivedEntries(ctx context.Context, who *model.UserInfo, calendars []uuid.UUID, from, to time.Time) ([]Event, error) {
	if len(calendars) == 0 {
		return nil, nil
	}
	cals, err := s.store.calendarsByID(ctx, calendars)
	if err != nil {
		return nil, err
	}
	on, ids := containersOf(cals)
	var out []Event
	if teams := ids[containerTeam]; len(teams) > 0 {
		away, err := s.teamTimeOff(ctx, personOf(who), teams, on[containerTeam], from, to)
		if err != nil {
			return nil, err
		}
		out = append(out, away...)
	}
	projects := ids[containerProject]
	readTasks, readMilestones := derivedKinds(ctx)
	if len(projects) > 0 && readMilestones {
		marks, err := milestoneEntries(ctx, projects, on[containerProject], from, to)
		if err != nil {
			return nil, err
		}
		out = append(out, marks...)
	}
	if !readTasks {
		return out, nil
	}
	tasks, err := s.containerTasks(ctx, subjectOf(who), projects, ids[containerChannel], on, from, to)
	if err != nil {
		return nil, err
	}
	return append(out, tasks...), nil
}

// containersOf is the live containers of cals: on, each one's calendars by
// its kind and lower-case uuid, and ids, each kind's containers in the order
// first met. Pure.
func containersOf(cals []models.Calendar) (on map[string]map[string][]string, ids map[string][]string) {
	on, ids = map[string]map[string][]string{}, map[string][]string{}
	for _, c := range cals {
		if !isContainer(c.ContainerKind) || c.Deleted {
			continue
		}
		kind, key := c.ContainerKind, strings.ToLower(c.ContainerID)
		if on[kind] == nil {
			on[kind] = map[string][]string{}
		}
		if on[kind][key] == nil {
			ids[kind] = append(ids[kind], key)
		}
		on[kind][key] = append(on[kind][key], c.ID.String())
	}
	return on, ids
}

// containerTasks are the tasks with a day between from and to of projects
// and channels (lower-case uuids, in order) on each of their calendars (on),
// as who may read each: each kind's in one read for them all, then the task
// rule asked once for them all (taskEntries). The one way a calendar, in the
// app or through a link, reads the work it shows.
func (s *service) containerTasks(ctx context.Context, who accessBusiness.Subject, projects, channels []string, on map[string]map[string][]string, from, to time.Time) ([]Event, error) {
	if len(projects) == 0 && len(channels) == 0 {
		return nil, nil
	}
	cond, err := taskDomain.TaskListFilter{From: &from, To: &to}.Clause()
	if err != nil {
		return nil, err
	}
	var linked []taskOn
	if len(projects) > 0 {
		tasks, err := projectTasksIn(ctx, projects, cond)
		if err != nil {
			return nil, err
		}
		linked = append(linked, tasksOn(projects, tasks, on[containerProject])...)
	}
	if len(channels) > 0 {
		tasks, err := channelTasksIn(ctx, channels, cond)
		if err != nil {
			return nil, err
		}
		linked = append(linked, tasksOn(channels, tasks, on[containerChannel])...)
	}
	return s.taskEntries(ctx, who, linked)
}

// taskOn is a task a calendar shows, if its reader may read it.
type taskOn struct {
	calendar string
	task     *dgraphStruct.DgraphTask
}

// tasksOn is the tasks of each of containers (lower-case uuids, in order)
// on each of its calendars (on). Pure.
func tasksOn(containers []string, tasks map[string][]*dgraphStruct.DgraphTask, on map[string][]string) []taskOn {
	byKey := make(map[string][]*dgraphStruct.DgraphTask, len(tasks))
	for container, list := range tasks {
		key := strings.ToLower(container)
		byKey[key] = append(byKey[key], list...)
	}
	var out []taskOn
	for _, container := range containers {
		for _, cal := range on[container] {
			for _, t := range byKey[container] {
				if t != nil && t.Uuid != "" {
					out = append(out, taskOn{calendar: cal, task: t})
				}
			}
		}
	}
	return out
}

// taskEntries are the tasks of linked who may read (the Access Resolver's
// task kind, in one read for them all) as derived entries, each dated one
// once on each of its calendars: the one way a task becomes a derived entry,
// whichever source shows it.
func (s *service) taskEntries(ctx context.Context, who accessBusiness.Subject, linked []taskOn) ([]Event, error) {
	var res []accessBusiness.Resource
	asked := map[string]bool{}
	for _, l := range linked {
		if id := strings.ToLower(l.task.Uuid); !asked[id] {
			asked[id] = true
			res = append(res, accessBusiness.Resource{Kind: sourceTask, UUID: id})
		}
	}
	if len(res) == 0 {
		return nil, nil
	}
	decided, err := s.resolveAll(ctx, who, res)
	if err != nil {
		return nil, err
	}
	shown := map[string]bool{}
	var out []Event
	for _, l := range linked {
		id := strings.ToLower(l.task.Uuid)
		if decided[accessBusiness.Resource{Kind: sourceTask, UUID: id}].Level < accessBusiness.LevelView || shown[l.calendar+" "+id] {
			continue
		}
		if e, ok := taskEntry(l.calendar, l.task); ok {
			shown[l.calendar+" "+id] = true
			out = append(out, e)
		}
	}
	return out, nil
}

// taskEntry is a task as a calendar shows it: all day over its days, start to
// due (one of them alone when it has one), or nothing when it has no date.
// Pure.
func taskEntry(calendar string, t *dgraphStruct.DgraphTask) (Event, bool) {
	first, last := t.StartDate, t.DueDate
	if first == nil {
		first = last
	}
	if last == nil {
		last = first
	}
	if first == nil {
		return Event{}, false
	}
	a, b := first.UTC(), last.UTC()
	if b.Before(a) {
		a, b = b, a
	}
	return dayEntry(calendar, sourceTask, strings.ToLower(t.Uuid), t.Name, a.Format(dateLayout), b.AddDate(0, 0, 1).Format(dateLayout)), true
}

// milestoneEntries are projects' milestones with a day between from and to,
// each on its day on each of its project's calendars (onProject), in one read.
func milestoneEntries(ctx context.Context, projects []string, onProject map[string][]string, from, to time.Time) ([]Event, error) {
	ids := make([]uuid.UUID, 0, len(projects))
	for _, p := range projects {
		if id, err := uuid.Parse(p); err == nil {
			ids = append(ids, id)
		}
	}
	got, err := milestonesIn(ctx, ids)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, p := range ids {
		for _, m := range got[p] {
			if m.TargetDate == nil || len(*m.TargetDate) < len(dateLayout) {
				continue
			}
			day, err := time.Parse(dateLayout, (*m.TargetDate)[:len(dateLayout)])
			if err != nil || !day.Before(to) || !day.AddDate(0, 0, 1).After(from) {
				continue
			}
			for _, cal := range onProject[p.String()] {
				out = append(out, dayEntry(cal, sourceMilestone, m.Id.String(), m.Name, day.Format(dateLayout), day.AddDate(0, 0, 1).Format(dateLayout)))
			}
		}
	}
	return out, nil
}

// dayEntry is a task or a milestone as a calendar shows it: its name, all
// day from first to the day after its last (after), free time, read only and
// linked to it. Pure.
func dayEntry(calendar, kind, id, title, first, after string) Event {
	return Event{ID: kind + ":" + id, CalendarID: calendar, ReadOnly: true, Kind: EventPlain, Title: title, Files: []FileRef{}, Rooms: []Room{},
		Start: When{Date: first}, End: When{Date: after}, AllDay: true, Transparency: transparencyTransparent,
		Visibility: visibilityDefault, Status: statusConfirmed, Attendees: []Attendee{}, Source: &EventSource{Kind: kind, ID: id}}
}

// teamTimeOff is teams' members' time off on each team's calendars
// (onTeam), for the members whose busy times reader may see.
func (s *service) teamTimeOff(ctx context.Context, reader uuid.UUID, teams []string, onTeam map[string][]string, from, to time.Time) ([]Event, error) {
	members, err := teamMembersOf(ctx, teams)
	if err != nil {
		return nil, err
	}
	var people []string
	asked := map[string]bool{}
	for _, team := range teams {
		for _, p := range members[team] {
			if !asked[p] {
				asked[p] = true
				people = append(people, p)
			}
		}
	}
	seen, err := s.busySeenOf(ctx, reader, people)
	if err != nil {
		return nil, err
	}
	var shown []string
	for _, p := range people {
		if seen[p] {
			shown = append(shown, p)
		}
	}
	if len(shown) == 0 {
		return nil, nil
	}
	spans, err := s.awaySpans(ctx, shown, from, to)
	if err != nil {
		return nil, err
	}
	// Only the people shown were read: nobody hidden from reader is.
	byPerson := map[string][]PersonSpan{}
	for _, sp := range spans {
		p := strings.ToLower(sp.Person)
		byPerson[p] = append(byPerson[p], sp)
	}
	var out []Event
	for _, team := range teams {
		for _, p := range members[team] {
			for _, sp := range byPerson[p] {
				for _, cal := range onTeam[team] {
					out = append(out, awayEntry(cal, p, sp.Start, sp.End))
				}
			}
		}
	}
	return out, nil
}

// awayEntry is a person's time off as a team calendar shows it: "Away", when,
// and who, nothing more. Pure.
func awayEntry(calendar, person string, start, end time.Time) Event {
	start, end = start.UTC(), end.UTC()
	return Event{ID: "away:" + person + ":" + strconv.FormatInt(start.Unix(), 10), CalendarID: calendar, ReadOnly: true,
		Kind: EventAway, Title: "Away", Files: []FileRef{}, Rooms: []Room{}, Start: When{At: &start, Zone: "UTC"}, End: When{At: &end, Zone: "UTC"},
		Transparency: transparencyOpaque, Visibility: visibilityPrivate, Status: statusConfirmed,
		Organiser: Person{UUID: person}, Attendees: []Attendee{}, Source: &EventSource{Kind: sourcePerson, ID: person}}
}
