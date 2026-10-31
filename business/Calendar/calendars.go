package Calendar

// Calendars other than a person's own (calendar A6, #44), first step: a
// project's own calendar, one per project (the calendar_containers
// migration), whose members see it as the project decides; the calendar as a
// kind of the Access Resolver; and its Live Entity Chip (the project home's
// Calendar card): its name and its next three events, for one reader,
// decided by the one rule.
//
//   - A person's own calendar (and one an import made for them) is theirs:
//     they manage it, and nobody else sees it here. Sharing it at a level is
//     A6's next step.
//   - A project's calendar is as the project is: someone who can edit the
//     project edits its calendar, someone who can see it sees it, anyone
//     else learns nothing of it.

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// KindCalendar is a calendar, by its id, as the Access Resolver, Resource
// Events and Live Entity Chips name it. Never renamed once shipped.
const KindCalendar = "calendar"

// The containers a calendar can belong to, and the kinds of resource the
// Access Resolver decides them by.
const containerProject = "project"

// chipEvents is how many of its next events a calendar's chip names.
const chipEvents = 3

// Reasons a calendar decision gives: a project's calendar, one shared at a
// level, and one a member sees as free and busy alone (owner decision O7).
const (
	reasonProject  = "project"
	reasonShared   = "shared"
	reasonFreeBusy = "freebusy"
	// reasonSubscribed is a calendar read from an address, or an event on
	// one, to its person (calendar B8): seen whole, changed only there.
	reasonSubscribed = "subscribed"
)

// busyOnly is a decision that shows when someone is busy and nothing more.
// Pure.
func busyOnly(d accessBusiness.Decision) bool {
	return len(d.Reasons) == 1 && d.Reasons[0] == reasonFreeBusy
}

// hiddenFrom is whether person may know of an event only that its calendar
// is busy then, given what they may do with that calendar (calendar): never
// an event of theirs (its organiser, a guest, a reader); every event on a
// calendar they see only as busy; and a private event, unless they may
// change its calendar, as in Google Calendar. The one rule for the grid
// (CalendarEvents), feeds (FeedICS) and the event's decisions (decideMany),
// so what shows as Busy is named nowhere else. Pure.
func hiddenFrom(e *models.Event, guests []models.Attendee, person uuid.UUID, calendar accessBusiness.Decision) bool {
	if roleOf(e, guests, person) != roleNone {
		return false
	}
	return busyOnly(calendar) || e.Visibility == visibilityPrivate && calendar.Level < accessBusiness.LevelEdit
}

func init() {
	accessBusiness.RegisterKind(KindCalendar, calendarRules{})
	entityLinkBusiness.RegisterKind(KindCalendar, func(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]entityLinkBusiness.Entity, error) {
		return stored.calendarChips(ctx, who, ids)
	})
}

// ProjectCalendarFor is a project's calendar for someone who may see the
// project, made the first time: the calendar's id, or ErrEventNotFound when
// they can't see the project.
func (s *service) ProjectCalendarFor(ctx context.Context, who *model.UserInfo, projectUUID string) (uuid.UUID, error) {
	id := strings.ToLower(strings.TrimSpace(projectUUID))
	res := accessBusiness.Resource{Kind: containerProject, UUID: id}
	decided, err := s.resolveAll(ctx, subjectOf(who), []accessBusiness.Resource{res})
	if err != nil {
		return uuid.Nil, err
	}
	if decided[res].Level < accessBusiness.LevelView {
		return uuid.Nil, ErrEventNotFound
	}
	return s.projectCalendar(ctx, id, s.zoneOf(ctx, personOf(who)))
}

func (s *service) projectCalendar(ctx context.Context, projectUUID, tz string) (uuid.UUID, error) {
	if err := s.ready(ctx); err != nil {
		return uuid.Nil, err
	}
	id := strings.ToLower(strings.TrimSpace(projectUUID))
	if _, err := uuid.Parse(id); err != nil {
		return uuid.Nil, inputError("That isn't a project.")
	}
	return s.store.containerCalendar(ctx, containerProject, id, zoneOrUTC(tz))
}

type calendarRules struct{}

func (calendarRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	return stored.decideCalendars(ctx, who, ids)
}

// decideCalendars is the calendar rule for many calendars at once: one read
// of the calendars, one decision of the projects they belong to.
func (s *service) decideCalendars(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	asked := map[uuid.UUID][]string{}
	var keys []uuid.UUID
	for _, id := range ids {
		if u, err := uuid.Parse(id); err == nil {
			if asked[u] == nil {
				keys = append(keys, u)
			}
			asked[u] = append(asked[u], id)
		}
	}
	cals, err := s.store.calendarsByID(ctx, keys)
	if err != nil {
		return nil, err
	}
	var projects []accessBusiness.Resource
	for _, c := range cals {
		if c.ContainerKind == containerProject {
			projects = append(projects, accessBusiness.Resource{Kind: containerProject, UUID: c.ContainerID})
		}
	}
	decided := map[accessBusiness.Resource]accessBusiness.Decision{}
	if len(projects) > 0 {
		if decided, err = s.resolveAll(ctx, who, projects); err != nil {
			return nil, err
		}
	}
	person, _ := uuid.Parse(who.UserUUID)
	// Someone else's calendar: as they shared it, else (to a member) when
	// they are busy and nothing more.
	var others []uuid.UUID
	for _, c := range cals {
		if c.OwnerID != nil && *c.OwnerID != person && c.ContainerKind == "" {
			others = append(others, c.ID)
		}
	}
	shares := map[uuid.UUID]string{}
	member := false
	if len(others) > 0 && person != uuid.Nil {
		if shares, err = s.store.sharesFor(ctx, person, others); err != nil {
			return nil, err
		}
		if member, err = s.seesWhenOthersAreBusy(ctx, person); err != nil {
			return nil, err
		}
	}
	for _, c := range cals {
		var d accessBusiness.Decision
		switch {
		case c.Kind == kindSubscribed:
			// A calendar read from an address is its person's alone, and read
			// only: they see it and keep the subscription; nobody else sees
			// it at all, not even as busy.
			if c.OwnerID != nil && *c.OwnerID == person && person != uuid.Nil {
				d.Level, d.Owner, d.Reasons = accessBusiness.LevelView, true, []string{reasonSubscribed}
			}
		case c.ContainerKind == containerProject:
			p := decided[accessBusiness.Resource{Kind: containerProject, UUID: c.ContainerID}]
			d.Level, d.Archived = calendarLevelOf(p.Level), p.Archived
			if d.Level > accessBusiness.LevelNone {
				d.Reasons = []string{reasonProject}
			}
		case c.OwnerID != nil && *c.OwnerID == person && person != uuid.Nil:
			d.Level, d.Owner, d.Reasons = accessBusiness.LevelFull, true, []string{reasonOwner}
		case c.OwnerID != nil && c.ContainerKind == "":
			switch shares[c.ID] {
			case models.ShareManage:
				d.Level, d.Reasons = accessBusiness.LevelFull, []string{reasonShared}
			case models.ShareEdit:
				d.Level, d.Reasons = accessBusiness.LevelEdit, []string{reasonShared}
			case models.ShareDetails:
				d.Level, d.Reasons = accessBusiness.LevelView, []string{reasonShared}
			default:
				if member {
					d.Level, d.Reasons = accessBusiness.LevelView, []string{reasonFreeBusy}
				}
			}
		}
		d.Was = d.Level
		if c.Deleted {
			d.Deleted, d.Level = true, accessBusiness.LevelNone
		}
		for _, id := range asked[c.ID] {
			out[id] = d
		}
	}
	return out, nil
}

// calendarLevelOf is what a level on a project gives on its calendar: edit
// or more edits its events (managing the project isn't managing its
// calendar's every event), a view sees them, and a comment sees them. Pure.
func calendarLevelOf(project accessBusiness.Level) accessBusiness.Level {
	switch {
	case project >= accessBusiness.LevelEdit:
		return accessBusiness.LevelEdit
	case project >= accessBusiness.LevelView:
		return accessBusiness.LevelView
	}
	return accessBusiness.LevelNone
}

// calendarChips is each calendar as its chip shows it to who: its name and
// its next events (id, title, start), when who may see it; "none" when not,
// and "gone" when it was deleted.
func (s *service) calendarChips(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]entityLinkBusiness.Entity, error) {
	decided, err := s.decideCalendars(ctx, who, ids)
	if err != nil {
		return nil, err
	}
	out := map[string]entityLinkBusiness.Entity{}
	var keys []uuid.UUID
	for id, d := range decided {
		switch {
		case d.Deleted:
			out[id] = entityLinkBusiness.Entity{Kind: KindCalendar, ID: id, Access: entityLinkBusiness.AccessGone}
		case d.Level >= accessBusiness.LevelView && !busyOnly(d):
			keys = append(keys, uuid.MustParse(id))
		}
	}
	cals, err := s.store.calendarsByID(ctx, keys)
	if err != nil {
		return nil, err
	}
	names, err := s.calendarNames(ctx, who, cals)
	if err != nil {
		return nil, err
	}
	shown := make([]uuid.UUID, len(cals))
	for i, c := range cals {
		shown[i] = c.ID
	}
	next, err := s.nextEvents(ctx, who, shown, s.now(), chipEvents)
	if err != nil {
		return nil, err
	}
	for _, c := range cals {
		id := c.ID.String()
		out[id] = entityLinkBusiness.Entity{Kind: KindCalendar, ID: id, Access: entityLinkBusiness.AccessOK, Title: names[c.ID],
			Data: map[string]any{"name": names[c.ID], "hue": c.Hue, "container_kind": c.ContainerKind, "container_id": c.ContainerID, "next": next[c.ID]}}
	}
	return out, nil
}

// calendarNames is what each calendar is called for who: its own name, its
// project's (read as its chip names it), or, for someone else's own
// calendar, its owner's name; else "Calendar".
func (s *service) calendarNames(ctx context.Context, who accessBusiness.Subject, cals []models.Calendar) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	var refs []entityLinkBusiness.Ref
	var owners []string
	for _, c := range cals {
		out[c.ID] = c.Name
		if c.Name == "" && c.ContainerKind == containerProject {
			refs = append(refs, entityLinkBusiness.Ref{Kind: containerProject, ID: c.ContainerID})
		}
		if c.Name == "" && c.Kind == "personal" && c.OwnerID != nil && !strings.EqualFold(c.OwnerID.String(), who.UserUUID) {
			owners = append(owners, c.OwnerID.String())
		}
	}
	if len(owners) > 0 {
		people, err := s.people(ctx, owners)
		if err != nil {
			return nil, err
		}
		named := map[string]string{}
		for _, p := range people {
			if p != nil {
				named[strings.ToLower(p.Uuid)] = p.DisplayName()
			}
		}
		for _, c := range cals {
			if out[c.ID] == "" && c.Kind == "personal" && c.OwnerID != nil {
				out[c.ID] = named[c.OwnerID.String()]
			}
		}
	}
	if len(refs) > 0 {
		got, err := s.readEntities(ctx, who, refs)
		if err != nil {
			return nil, err
		}
		byProject := map[string]string{}
		for _, e := range got {
			if e.Access == entityLinkBusiness.AccessOK {
				byProject[strings.ToLower(e.ID)] = e.Title
			}
		}
		for _, c := range cals {
			if out[c.ID] == "" {
				out[c.ID] = byProject[strings.ToLower(c.ContainerID)]
			}
		}
	}
	for id, n := range out {
		if n == "" {
			out[id] = "Calendar"
		}
	}
	return out, nil
}

// ChipEvent is one of a calendar's next events, as its chip names it.
type ChipEvent struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Start  When   `json:"start"`
	AllDay bool   `json:"all_day"`
}

// nextEvents are each calendar's next n occurrences from now: single events
// and each series' next instances, in order, each titled as reader reads it
// (titleOf). One read of the calendars' events and one of their series'
// overrides; then each calendar is worked out on its own, so a busy one never
// crowds out another.
func (s *service) nextEvents(ctx context.Context, reader accessBusiness.Subject, calendars []uuid.UUID, now time.Time, n int) (map[uuid.UUID][]ChipEvent, error) {
	out := make(map[uuid.UUID][]ChipEvent, len(calendars))
	for _, c := range calendars {
		out[c] = make([]ChipEvent, 0, n)
	}
	if len(calendars) == 0 {
		return out, nil
	}
	rows, err := s.store.nextOf(ctx, calendars, now, n*4)
	if err != nil {
		return nil, err
	}
	w := models.Window{From: now, To: now.AddDate(0, 0, maxRangeDays), FromDate: now.Format(dateLayout), ToDate: now.AddDate(0, 0, maxRangeDays).Format(dateLayout)}
	overrides, err := s.overridesFor(ctx, rows, w)
	if err != nil {
		return nil, err
	}
	byCalendar := map[uuid.UUID][]*models.Event{}
	for _, e := range rows {
		byCalendar[e.CalendarID] = append(byCalendar[e.CalendarID], e)
	}
	title := s.titler(ctx, reader, rows)
	for c, rows := range byCalendar {
		for _, inst := range expandInstances(ctx, rows, overrides, w, n*4) {
			if len(out[c]) == n {
				break
			}
			if !live(inst.Event) {
				continue
			}
			start, _ := whenOf(inst.Event)
			if _, end := spanOf(inst.Event); end.Before(now) {
				continue
			}
			out[c] = append(out[c], ChipEvent{ID: inst.ref, Title: title(inst.Event), Start: start, AllDay: inst.AllDay()})
		}
	}
	return out, nil
}

// resolveAll and readEntities are the Access Resolver's and the chips'
// reads; tests swap them.
var (
	resolveAllFn = accessBusiness.ResolveAll
	readEntityFn = entityLinkBusiness.ReadEntities
)

func (s *service) resolveAll(ctx context.Context, who accessBusiness.Subject, res []accessBusiness.Resource) (map[accessBusiness.Resource]accessBusiness.Decision, error) {
	return resolveAllFn(ctx, who, res)
}

func (s *service) readEntities(ctx context.Context, who accessBusiness.Subject, refs []entityLinkBusiness.Ref) ([]entityLinkBusiness.Entity, error) {
	return readEntityFn(ctx, who, refs)
}

// subjectOf is a member as the Access Resolver names them.
func subjectOf(who *model.UserInfo) accessBusiness.Subject {
	return accessBusiness.Subject{UserUID: who.UserDgraphInfo.Uid, UserUUID: personOf(who).String()}
}

// writableCalendar is a calendar who may put events on, or why not.
func (s *service) writableCalendar(ctx context.Context, who *model.UserInfo, id string) (uuid.UUID, error) {
	cal, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return uuid.Nil, inputError("That isn't a calendar.")
	}
	decided, err := s.decideCalendars(ctx, subjectOf(who), []string{cal.String()})
	if err != nil {
		return uuid.Nil, err
	}
	if decided[cal.String()].Level < accessBusiness.LevelEdit {
		return uuid.Nil, inputError("You can't add events to that calendar.")
	}
	return cal, nil
}

// CalendarView is a calendar as its list shows it to one person.
type CalendarView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"` // "personal", "external", "subscribed" or "project"
	Hue           string `json:"hue,omitempty"`
	ContainerKind string `json:"container_kind,omitempty"`
	ContainerID   string `json:"container_id,omitempty"`
	// Access is "manage", "edit" or "view".
	Access string `json:"access"`
	// Mine marks the asker's own: theirs and those an import made them.
	Mine bool `json:"mine,omitempty"`
}

// maxListedContainers is how many container calendars a list reads.
const maxListedContainers = 2000

// Calendars are the calendars who may see: their own first (made the first
// time), then each project's they may see, by name.
func (s *service) Calendars(ctx context.Context, who *model.UserInfo) ([]CalendarView, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	me := personOf(who)
	if me == uuid.Nil {
		return nil, ErrEventNotFound
	}
	if _, err := s.store.personalCalendar(ctx, me, s.zoneOf(ctx, me)); err != nil {
		return nil, err
	}
	seen, decided, err := s.seenCalendars(ctx, subjectOf(who), me)
	if err != nil {
		return nil, err
	}
	names, err := s.calendarNames(ctx, subjectOf(who), seen)
	if err != nil {
		return nil, err
	}
	out := make([]CalendarView, 0, len(seen))
	for _, c := range seen {
		v := CalendarView{ID: c.ID.String(), Name: names[c.ID], Kind: c.Kind, Hue: c.Hue, ContainerKind: c.ContainerKind, ContainerID: c.ContainerID, Mine: decided[c.ID.String()].Owner}
		switch d := decided[c.ID.String()]; {
		case c.Kind == kindSubscribed:
			v.Access = "view" // its events change where it's published
		case d.Owner || d.Level >= accessBusiness.LevelFull:
			v.Access = "manage"
		case d.Level >= accessBusiness.LevelEdit:
			v.Access = "edit"
		default:
			v.Access = "view"
		}
		if c.Kind == "personal" && v.Mine {
			v.Name = "Personal"
		}
		out = append(out, v)
	}
	return out, nil
}

// seenCalendars are the calendars who (me) may see, each with its decision:
// their own, those shared with them and each project's they may see, never
// one they see only as busy. The one list for the calendar list (Calendars)
// and search's (SearchCalendars).
func (s *service) seenCalendars(ctx context.Context, who accessBusiness.Subject, me uuid.UUID) ([]models.Calendar, map[string]accessBusiness.Decision, error) {
	own, err := s.store.ownCalendars(ctx, me)
	if err != nil {
		return nil, nil, err
	}
	shared, err := s.store.sharedWith(ctx, me)
	if err != nil {
		return nil, nil, err
	}
	projects, err := s.store.containerCalendars(ctx, containerProject, maxListedContainers)
	if err != nil {
		return nil, nil, err
	}
	all := append(append(append([]models.Calendar{}, own...), shared...), projects...)
	ids := make([]string, 0, len(all))
	for _, c := range all {
		ids = append(ids, c.ID.String())
	}
	decided, err := s.decideCalendars(ctx, who, ids)
	if err != nil {
		return nil, nil, err
	}
	var seen []models.Calendar
	for _, c := range all {
		if d := decided[c.ID.String()]; d.Level >= accessBusiness.LevelView && !busyOnly(d) {
			seen = append(seen, c)
		}
	}
	return seen, decided, nil
}

// CalendarEvents are the events between from and to on the calendars of ids
// that who may see (others are left out), each series as its instances, and
// whether there were more than one answer holds.
func (s *service) CalendarEvents(ctx context.Context, who *model.UserInfo, ids []string, from, to time.Time) ([]Event, bool, error) {
	if err := s.ready(ctx); err != nil {
		return nil, false, err
	}
	decided, err := s.decideCalendars(ctx, subjectOf(who), ids)
	if err != nil {
		return nil, false, err
	}
	var allowed []uuid.UUID
	onCalendar := map[uuid.UUID]accessBusiness.Decision{}
	for id, d := range decided {
		if d.Level >= accessBusiness.LevelView {
			cal := uuid.MustParse(id)
			allowed = append(allowed, cal)
			onCalendar[cal] = d
		}
	}
	w, err := window(from, to)
	if err != nil {
		return nil, false, err
	}
	rows, err := s.store.rangeOnCalendars(ctx, allowed, w, maxInstances+1)
	if err != nil {
		return nil, false, err
	}
	cut := len(rows) > maxInstances
	if cut {
		rows = rows[:maxInstances]
	}
	inst, err := s.instancesOf(ctx, rows, w, maxInstances+1)
	if err != nil {
		return nil, false, err
	}
	if len(inst) > maxInstances {
		inst, cut = inst[:maxInstances], true
	}
	guests, err := s.store.attendees(ctx, idsOf(rows))
	if err != nil {
		return nil, false, err
	}
	views := s.views(ctx, who, inst, guests)
	me := personOf(who)
	out := make([]Event, 0, len(views))
	for i, v := range views {
		row := inst[i].owner()
		cal := onCalendar[row.CalendarID]
		switch {
		case !hiddenFrom(row, guests[row.ID], me, cal):
		case busyOnly(cal) && inst[i].Transparency == transparencyTransparent:
			continue // free time isn't shown to someone who sees only busy
		default:
			v = busyView(v)
		}
		out = append(out, v)
	}
	return out, cut, nil
}

// busyView is an event as someone who may see only that its owner is busy
// sees it: "Busy", when, and nothing else. Free time isn't shown. Pure.
func busyView(e Event) Event {
	return Event{ID: e.ID, CalendarID: e.CalendarID, SeriesID: e.SeriesID, RecurrenceID: e.RecurrenceID, Kind: EventPlain, Title: "Busy",
		Start: e.Start, End: e.End, AllDay: e.AllDay, Transparency: e.Transparency, Visibility: visibilityPrivate, Status: e.Status,
		Organiser: Person{}, Attendees: []Attendee{}}
}

// PersonCalendar is a member's own calendar, for another member to lay over
// theirs: its id (made the first time), shown as they shared it with the
// asker, else as free and busy alone.
func (s *service) PersonCalendar(ctx context.Context, who *model.UserInfo, personUUID string) (uuid.UUID, error) {
	if err := s.ready(ctx); err != nil {
		return uuid.Nil, err
	}
	person, err := uuid.Parse(strings.TrimSpace(personUUID))
	if err != nil {
		return uuid.Nil, ErrEventNotFound
	}
	members, err := s.store.membersNow(ctx, []uuid.UUID{personOf(who), person})
	if err != nil {
		return uuid.Nil, err
	}
	if !members[personOf(who)] || !members[person] {
		return uuid.Nil, ErrEventNotFound
	}
	return s.store.personalCalendar(ctx, person, s.zoneOf(ctx, person))
}

// CalendarShare is who a calendar is shared with, and at what level.
type CalendarShare struct {
	Person Person `json:"person"`
	Level  string `json:"level"` // "details", "edit" or "manage"
}

// manageable is a calendar who may share, or ErrEventNotFound.
func (s *service) manageable(ctx context.Context, who *model.UserInfo, id string) (uuid.UUID, error) {
	cal, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return uuid.Nil, ErrEventNotFound
	}
	decided, err := s.decideCalendars(ctx, subjectOf(who), []string{cal.String()})
	if err != nil {
		return uuid.Nil, err
	}
	d, ok := decided[cal.String()]
	if !ok || d.Level < accessBusiness.LevelFull {
		return uuid.Nil, ErrEventNotFound
	}
	return cal, nil
}

// Shares are who a calendar who manages is shared with.
func (s *service) Shares(ctx context.Context, who *model.UserInfo, id string) ([]CalendarShare, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	cal, err := s.manageable(ctx, who, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.sharesOf(ctx, cal)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.PersonID.String())
	}
	people, err := s.people(ctx, ids)
	if err != nil {
		return nil, err
	}
	names := map[string]*dgraphStruct.DgraphUser{}
	for _, p := range people {
		if p != nil {
			names[p.Uuid] = p
		}
	}
	out := make([]CalendarShare, 0, len(rows))
	for _, r := range rows {
		out = append(out, CalendarShare{Person: personNamed(r.PersonID, names), Level: r.Level})
	}
	return out, nil
}

// Share shares a calendar who manages with a member at a level ("details",
// "edit" or "manage"), or stops sharing it ("").
func (s *service) Share(ctx context.Context, who *model.UserInfo, id, personUUID, level string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	switch level {
	case "", models.ShareDetails, models.ShareEdit, models.ShareManage:
	default:
		return inputError("Share it to see details, to edit, or to manage.")
	}
	cal, err := s.manageable(ctx, who, id)
	if err != nil {
		return err
	}
	person, err := uuid.Parse(strings.TrimSpace(personUUID))
	if err != nil || person == personOf(who) {
		return inputError("Pick someone else in the workspace.")
	}
	// A share is given only to a member now; one given is taken back from
	// anyone, someone deactivated since included.
	in := s.store.membersNow
	if level == "" {
		in = s.store.known
	}
	people, err := in(ctx, []uuid.UUID{person})
	if err != nil {
		return err
	}
	if !people[person] {
		return inputError("Pick someone in the workspace.")
	}
	return s.store.setShare(ctx, cal, person, personOf(who), level)
}

// freeBusyPolicy is the workspace's calendar.freebusy.members policy (members
// see when others are busy); tests swap it.
var freeBusyPolicy = func(ctx context.Context) bool {
	return policyBusiness.Policies.Bool(ctx, policyBusiness.CalendarFreeBusy)
}

// seesWhenOthersAreBusy is the one rule for whether person sees when others
// are busy: a member now, while the workspace lets members see it (O7). The
// calendar's views and Find a time both ask it.
func (s *service) seesWhenOthersAreBusy(ctx context.Context, person uuid.UUID) (bool, error) {
	if person == uuid.Nil || !freeBusyPolicy(ctx) {
		return false, nil
	}
	members, err := s.store.membersNow(ctx, []uuid.UUID{person})
	if err != nil {
		return false, err
	}
	return members[person], nil
}

// CalendarPatch is a change to a calendar: its name (not your own's, which is
// "Personal") and its colour, one of the camp hues ("" for none). A field
// left out stays.
type CalendarPatch struct {
	Name *string `json:"name"`
	Hue  *string `json:"hue"`
}

// UpdateCalendar renames or recolours a calendar who manages.
func (s *service) UpdateCalendar(ctx context.Context, who *model.UserInfo, id string, p CalendarPatch) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	cal, err := s.manageable(ctx, who, id)
	if errors.Is(err, ErrEventNotFound) {
		// A subscribed calendar is its person's to rename and recolour.
		cal, err = s.ownSubscribed(ctx, who, id)
	}
	if err != nil {
		return err
	}
	if p.Hue != nil && *p.Hue != "" && !helpers.IsCampHue(*p.Hue) {
		return inputError("Pick one of the calendar's colours.")
	}
	if p.Name != nil {
		cals, err := s.store.calendarsByID(ctx, []uuid.UUID{cal})
		if err != nil {
			return err
		}
		name := strings.TrimSpace(*p.Name)
		switch {
		case len(cals) == 1 && cals[0].Kind == "personal":
			return inputError("Your own calendar keeps its name.")
		case name == "":
			return inputError("Give the calendar a name.")
		case utf8.RuneCountInString(name) > maxCalendarNameRunes:
			return inputError("A calendar's name can be up to 120 characters.")
		}
		p.Name = &name
	}
	return s.store.updateCalendar(ctx, cal, p.Name, p.Hue)
}
