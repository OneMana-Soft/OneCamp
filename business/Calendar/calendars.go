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
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// KindCalendar is a calendar, by its id, as the Access Resolver, Resource
// Events and Live Entity Chips name it. Never renamed once shipped.
const KindCalendar = "calendar"

// The containers a calendar can belong to (migration 479's container_kind),
// each named as the Access Resolver names the kind of resource that decides
// it: a project's, a channel's (business/Conversation) and a team's
// (business/Team).
const (
	containerProject = "project"
	containerChannel = "channel"
	containerTeam    = "team"
)

// calendarContainers are the containers a calendar belongs to, in the order a
// list reads them.
var calendarContainers = []string{containerProject, containerChannel, containerTeam}

// isContainer is whether kind is a container a calendar can belong to. Pure.
func isContainer(kind string) bool {
	return slices.Contains(calendarContainers, kind)
}

// chipEvents is how many of its next events a calendar's chip names.
const chipEvents = 3

// Reasons a calendar decision gives: a project's calendar, one shared at a
// level, and one a member sees as free and busy alone (owner decision O7).
const (
	reasonProject  = "project"
	reasonChannel  = "channel"
	reasonTeam     = "team"
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

// freeTimeHidden is whether an event isn't shown at all to someone who
// isn't on it, given what they may do with its calendar: free time on a
// calendar they see only as busy, as free time isn't busy. The one rule for
// the grid (CalendarEvents) and the webhooks (webhookSees). Pure.
func freeTimeHidden(e *models.Event, calendar accessBusiness.Decision) bool {
	return busyOnly(calendar) && e.Transparency == transparencyTransparent
}

func init() {
	accessBusiness.RegisterKind(KindCalendar, calendarRules{})
	entityLinkBusiness.RegisterKind(KindCalendar, func(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]entityLinkBusiness.Entity, error) {
		return stored.calendarChips(ctx, who, ids)
	})
}

// projectCalendarFor is a project's calendar (ContainerCalendarFor).
func (s *service) projectCalendarFor(ctx context.Context, who *model.UserInfo, projectUUID string) (uuid.UUID, error) {
	return s.ContainerCalendarFor(ctx, who, containerProject, projectUUID)
}

// ContainerCalendarFor is a project's, a channel's or a team's calendar for
// someone who may see that container, made the first time: the calendar's
// id, or ErrEventNotFound when they can't see it (the container's own rule,
// through the Access Resolver).
func (s *service) ContainerCalendarFor(ctx context.Context, who *model.UserInfo, kind, containerUUID string) (uuid.UUID, error) {
	if !isContainer(kind) {
		return uuid.Nil, inputError("Only a project, a channel or a team has a calendar of its own.")
	}
	id := strings.ToLower(strings.TrimSpace(containerUUID))
	if _, err := uuid.Parse(id); err != nil {
		return uuid.Nil, ErrEventNotFound
	}
	res := accessBusiness.Resource{Kind: kind, UUID: id}
	decided, err := s.resolveAll(ctx, subjectOf(who), []accessBusiness.Resource{res})
	if err != nil {
		return uuid.Nil, err
	}
	if decided[res].Level < accessBusiness.LevelView {
		return uuid.Nil, ErrEventNotFound
	}
	return s.containerCalendar(ctx, kind, id, s.zoneOf(ctx, personOf(who)))
}

func (s *service) projectCalendar(ctx context.Context, projectUUID, tz string) (uuid.UUID, error) {
	return s.containerCalendar(ctx, containerProject, projectUUID, tz)
}

func (s *service) containerCalendar(ctx context.Context, kind, containerUUID, tz string) (uuid.UUID, error) {
	if err := s.ready(ctx); err != nil {
		return uuid.Nil, err
	}
	id := strings.ToLower(strings.TrimSpace(containerUUID))
	if _, err := uuid.Parse(id); err != nil || !isContainer(kind) {
		return uuid.Nil, inputError("That has no calendar of its own.")
	}
	return s.store.containerCalendar(ctx, kind, id, zoneOrUTC(tz))
}

type calendarRules struct{}

func (calendarRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	return stored.decideCalendars(ctx, who, ids)
}

// decideCalendars is the calendar rule for many calendars at once: one read
// of the calendars, one decision of the projects, channels and teams they
// belong to (each by its own rule, through the Access Resolver).
func (s *service) decideCalendars(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out, _, err := s.decideCalendarRows(ctx, who, ids)
	return out, err
}

// decideCalendarRows is decideCalendars with the calendars it read, for a
// caller that needs them too (a feed: its name and its tasks), so none reads
// them again.
func (s *service) decideCalendarRows(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, []models.Calendar, error) {
	out := map[string]accessBusiness.Decision{}
	if err := s.ready(ctx); err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	byCalendar, err := s.decideRows(ctx, who, cals, nil)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range cals {
		for _, id := range asked[c.ID] {
			out[id] = byCalendar[c.ID]
		}
	}
	return out, cals, nil
}

// decideRows decides each of cals for who, keyed by calendar: a project's,
// channel's or team's as who may see the container (read fresh every time);
// someone else's as they shared it with who (shares, when the caller read
// them already; else read here), else, when no share decides it, as busy and
// nothing more while who may see them busy (busySeenOf).
func (s *service) decideRows(ctx context.Context, who accessBusiness.Subject, cals []models.Calendar, shares map[uuid.UUID]string) (map[uuid.UUID]accessBusiness.Decision, error) {
	var containers []accessBusiness.Resource
	for _, c := range cals {
		if isContainer(c.ContainerKind) {
			containers = append(containers, accessBusiness.Resource{Kind: c.ContainerKind, UUID: c.ContainerID})
		}
	}
	decided := map[accessBusiness.Resource]accessBusiness.Decision{}
	var err error
	if len(containers) > 0 {
		if decided, err = s.resolveAll(ctx, who, containers); err != nil {
			return nil, err
		}
	}
	person, _ := uuid.Parse(who.UserUUID)
	// Someone else's calendar: as they shared it, else (to a member) when
	// they are busy and nothing more, unless they keep that to themselves.
	others := othersCalendars(cals, person)
	if shares == nil {
		shares = map[uuid.UUID]string{}
		if len(others) > 0 && person != uuid.Nil {
			if shares, err = s.store.sharesFor(ctx, person, others); err != nil {
				return nil, err
			}
		}
	}
	// Whose busy times who sees is asked only of the owners no share decides.
	var owners []string
	for _, c := range cals {
		if c.OwnerID != nil && *c.OwnerID != person && c.ContainerKind == "" && !shareDecides(shares[c.ID]) {
			owners = append(owners, c.OwnerID.String())
		}
	}
	busySeen := map[string]bool{}
	if len(owners) > 0 && person != uuid.Nil {
		if busySeen, err = s.busySeenOf(ctx, person, owners); err != nil {
			return nil, err
		}
	}
	out := make(map[uuid.UUID]accessBusiness.Decision, len(cals))
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
		case isContainer(c.ContainerKind):
			p := decided[accessBusiness.Resource{Kind: c.ContainerKind, UUID: c.ContainerID}]
			d.Level, d.Archived = calendarLevelOf(p.Level), p.Archived
			if d.Level > accessBusiness.LevelNone {
				d.Reasons = []string{reasonOfContainer(c.ContainerKind)}
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
				if busySeen[c.OwnerID.String()] {
					d.Level, d.Reasons = accessBusiness.LevelView, []string{reasonFreeBusy}
				}
			}
		}
		d.Was = d.Level
		if c.Deleted {
			d.Deleted, d.Level = true, accessBusiness.LevelNone
		}
		out[c.ID] = d
	}
	return out, nil
}

// othersCalendars are the calendars of cals that are someone's other than
// person's own and no container's: those a share with person decides.
// Pure.
func othersCalendars(cals []models.Calendar, person uuid.UUID) []uuid.UUID {
	var out []uuid.UUID
	for _, c := range cals {
		if c.OwnerID != nil && *c.OwnerID != person && c.ContainerKind == "" {
			out = append(out, c.ID)
		}
	}
	return out
}

// shareDecides is whether a share at level decides its calendar (else the
// owner's busy times may). Pure.
func shareDecides(level string) bool {
	switch level {
	case models.ShareManage, models.ShareEdit, models.ShareDetails:
		return true
	}
	return false
}

// reasonOfContainer is the reason a container's calendar is decided by: its
// project's, channel's or team's. Pure.
func reasonOfContainer(kind string) string {
	switch kind {
	case containerChannel:
		return reasonChannel
	case containerTeam:
		return reasonTeam
	}
	return reasonProject
}

// calendarLevelOf is what a level on a project, a channel or a team gives on
// its calendar: edit or more (posting in the channel, being in the team)
// edits its events (managing the container isn't managing its calendar's
// every event), a view sees them (a public channel read by someone not in
// it), and a comment (replying in an announcement channel) sees them. Pure.
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
// project's, channel's or team's (read as its chip names it), or, for someone else's own
// calendar, its owner's name; else "Calendar".
func (s *service) calendarNames(ctx context.Context, who accessBusiness.Subject, cals []models.Calendar) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	var refs []entityLinkBusiness.Ref
	var owners []string
	for _, c := range cals {
		out[c.ID] = c.Name
		if c.Name == "" && isContainer(c.ContainerKind) {
			refs = append(refs, entityLinkBusiness.Ref{Kind: c.ContainerKind, ID: c.ContainerID})
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
		byContainer := map[string]string{}
		for _, e := range got {
			if e.Access == entityLinkBusiness.AccessOK {
				byContainer[e.Kind+":"+strings.ToLower(e.ID)] = e.Title
			}
		}
		for _, c := range cals {
			if out[c.ID] == "" {
				out[c.ID] = byContainer[c.ContainerKind+":"+strings.ToLower(c.ContainerID)]
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
	if err := s.mayAddTo(ctx, cal); err != nil {
		return uuid.Nil, err
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
	// Source is where its events come from: "google" or "microsoft" (moved
	// from there and kept in step), "ics" (read from an address) or
	// "onecamp" (origin.go).
	Source string `json:"source"`
	// ReadOnly marks one its provider lets OneCamp only read (see-only
	// there, or read from an address): no new event goes on it.
	ReadOnly bool `json:"read_only,omitempty"`
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
	origins, err := s.originsOf(ctx, seen)
	if err != nil {
		return nil, err
	}
	out := make([]CalendarView, 0, len(seen))
	for _, c := range seen {
		v := CalendarView{ID: c.ID.String(), Name: names[c.ID], Kind: c.Kind, Hue: c.Hue, ContainerKind: c.ContainerKind, ContainerID: c.ContainerID, Mine: decided[c.ID.String()].Owner,
			Source: origins[c.ID].Source, ReadOnly: origins[c.ID].ReadOnly}
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
// their own, those shared with them and each project's, channel's and team's they may see, never
// one they see only as busy. The one list for the calendar list (Calendars)
// and search's (SearchCalendars), which reads its rows through searchReach.
func (s *service) seenCalendars(ctx context.Context, who accessBusiness.Subject, me uuid.UUID) ([]models.Calendar, map[string]accessBusiness.Decision, error) {
	mine, err := s.readMine(ctx, me)
	if err != nil {
		return nil, nil, err
	}
	containers, err := s.readContainers(ctx)
	if err != nil {
		return nil, nil, err
	}
	return s.seenOf(ctx, who, mine, containers)
}

// seenOf is seenCalendars from its rows as read: the calendars are decided
// as they were read, not read again, and every container's decision is
// asked fresh.
func (s *service) seenOf(ctx context.Context, who accessBusiness.Subject, mine, containers reachRows) ([]models.Calendar, map[string]accessBusiness.Decision, error) {
	all := append(append([]models.Calendar{}, mine.rows...), containers.rows...)
	shares := mine.shares
	if person, _ := uuid.Parse(who.UserUUID); person != mine.reader {
		shares = nil // read for someone else: decideRows reads who's own
	}
	byCalendar, err := s.decideRows(ctx, who, all, shares)
	if err != nil {
		return nil, nil, err
	}
	decided := make(map[string]accessBusiness.Decision, len(all))
	var seen []models.Calendar
	for _, c := range all {
		d := byCalendar[c.ID]
		decided[c.ID.String()] = d
		if d.Level >= accessBusiness.LevelView && !busyOnly(d) {
			seen = append(seen, c)
		}
	}
	return seen, decided, nil
}

// reachRows are calendars as Postgres has them, with the level each of
// someone else's is shared with reader at (none for containers').
type reachRows struct {
	reader uuid.UUID
	rows   []models.Calendar
	shares map[uuid.UUID]string
}

// readMine are me's own calendars and those shared with them, with the
// level of each share.
func (s *service) readMine(ctx context.Context, me uuid.UUID) (reachRows, error) {
	own, err := s.store.ownCalendars(ctx, me)
	if err != nil {
		return reachRows{}, err
	}
	shared, err := s.store.sharedWith(ctx, me)
	if err != nil {
		return reachRows{}, err
	}
	r := reachRows{reader: me, rows: append(own, shared...), shares: map[uuid.UUID]string{}}
	if others := othersCalendars(r.rows, me); len(others) > 0 && me != uuid.Nil {
		if r.shares, err = s.store.sharesFor(ctx, me, others); err != nil {
			return reachRows{}, err
		}
	}
	return r, nil
}

// readContainers are every project's, channel's and team's calendar, at
// most maxListedContainers of each kind.
func (s *service) readContainers(ctx context.Context) (reachRows, error) {
	rows, err := s.store.containerCalendars(ctx, calendarContainers, maxListedContainers)
	return reachRows{rows: rows}, err
}

// searchReachMaxAge is how long search keeps the calendars a person finds
// events on (searchReach). A write on this instance ends it at once
// (models.ReachWrites); one on another instance only by this age, while each
// event found is still decided fresh (readableEvents), so a calendar lost
// costs its events nothing but the search, and one gained is missed until
// then.
const searchReachMaxAge = 30 * time.Second

// searchReaches keep reachRows per reader (their uuid; their own and shared
// calendars) and once for everyone (containersReach: every container's
// calendar), each with the generation it was read at.
var searchReaches = helpers.NewTTLCache[keptReach](searchReachMaxAge)

const containersReach = "containers"

type keptReach struct {
	generation uint64
	reach      reachRows
}

// reachGenerations is a store that says when the rows a reach is read from
// were last written (models.ReachWrites); search keeps reaches only for one.
type reachGenerations interface {
	reachWrites() uint64
}

// searchReach is readMine and readContainers for search: kept between
// searches while nothing wrote calendars or calendar_shares since they were
// read and for at most searchReachMaxAge. The generation is read before the
// rows, so a write during the read leaves them stale at once. Only the rows
// are kept: who may see each container is asked every search.
func (s *service) searchReach(ctx context.Context, me uuid.UUID) (mine, containers reachRows, err error) {
	gens, ok := s.store.(reachGenerations)
	if !ok {
		if mine, err = s.readMine(ctx, me); err == nil {
			containers, err = s.readContainers(ctx)
		}
		return mine, containers, err
	}
	generation := gens.reachWrites()
	kept := func(key string, read func() (reachRows, error)) (reachRows, error) {
		if k, ok := searchReaches.Get(key); ok && k.generation == generation {
			return k.reach, nil
		}
		r, err := read()
		if err == nil {
			searchReaches.Set(key, keptReach{generation: generation, reach: r})
		}
		return r, err
	}
	if mine, err = kept(me.String(), func() (reachRows, error) { return s.readMine(ctx, me) }); err != nil {
		return reachRows{}, reachRows{}, err
	}
	containers, err = kept(containersReach, func() (reachRows, error) { return s.readContainers(ctx) })
	return mine, containers, err
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
		case freeTimeHidden(inst[i].Event, cal):
			continue
		default:
			v = busyView(v)
		}
		out = append(out, v)
	}
	derived, err := s.derivedEntries(ctx, who, allowed, from, to)
	if err != nil {
		return nil, false, err
	}
	return append(out, derived...), cut, nil
}

// busyView is an event as someone who may see only that its owner is busy
// sees it: "Busy", when, and nothing else (no notes, no files). Free time
// isn't shown. Pure.
func busyView(e Event) Event {
	return Event{ID: e.ID, CalendarID: e.CalendarID, SeriesID: e.SeriesID, RecurrenceID: e.RecurrenceID, Kind: EventPlain, Title: "Busy",
		Files: []FileRef{}, Rooms: []Room{}, Start: e.Start, End: e.End, AllDay: e.AllDay, Transparency: e.Transparency, Visibility: visibilityPrivate, Status: e.Status,
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
	had, err := s.store.sharesFor(ctx, person, []uuid.UUID{cal})
	if err != nil {
		return err
	}
	if err := s.store.setShare(ctx, cal, person, personOf(who), level); err != nil {
		return err
	}
	s.tellShare(ctx, who, cal, person, had[cal], level)
	return nil
}

// tellShare tells a share's change once it's made, for the audit log's
// "calendar": given, raised or lowered (calendar.shared, with the level it
// had) or stopped (calendar.unshared). A share set to the level it has
// changes nothing and tells nothing. A stopped share keeps its row, marked
// by whom (V-184 head 3b), so unlike a doc's or a board's taking, which
// leaves nothing behind, it isn't recorded before it's made.
func (s *service) tellShare(ctx context.Context, who *model.UserInfo, cal, person uuid.UUID, from, to string) {
	if from == to {
		return
	}
	kind, data := resourceEventBusiness.CalendarShared, map[string]string{"level": to}
	if to == "" {
		kind, data = resourceEventBusiness.CalendarUnshared, map[string]string{}
	}
	if from != "" {
		data["from"] = from
	}
	s.tellCalendar(ctx, who, kind, cal, []string{person.String()}, data)
}

// tellCalendar emits one Resource Event for a change to a calendar itself
// (not to its events). data never carries the calendar's name or an address.
func (s *service) tellCalendar(ctx context.Context, who *model.UserInfo, kind resourceEventBusiness.Kind, cal uuid.UUID, subjects []string, data map[string]string) {
	s.emit(ctx, resourceEventBusiness.Event{Kind: kind, ResourceKind: KindCalendar, ResourceUUID: cal.String(),
		Actor: resourceEventBusiness.Actor{UserUUID: personOf(who).String()}, At: s.now(), Subjects: subjects, Data: data})
}

// freeBusyPolicy is the workspace's calendar.freebusy.members policy (members
// see when others are busy); tests swap it.
var freeBusyPolicy = func(ctx context.Context) bool {
	return policyBusiness.Policies.Bool(ctx, policyBusiness.CalendarFreeBusy)
}

// freeBusyShownBy is whether each of people (lower-case uuids) lets other
// members see when they're busy: their own value of
// calendar.freebusy.members (the Policy Registry's person scope), else the
// workspace's, all in one read; an error when their choices couldn't be
// read. Tests swap it.
var freeBusyShownBy = func(ctx context.Context, people []string) (map[string]bool, error) {
	return policyBusiness.Policies.BoolsIn(ctx, policyBusiness.CalendarFreeBusy, policyBusiness.ScopePerson, people)
}

// busySeenOf is the one rule for whose busy times reader may see, of people:
// their own always; anyone else's while reader sees when others are busy
// (seesWhenOthersAreBusy) and that person hasn't kept it to themselves
// (freeBusyShownBy). The workspace's "off" wins over anyone's own "on", and
// choices that can't be read show nobody's. The calendar's decisions, Find a
// time, the team calendar's time off and everything that tells when someone
// is busy (PeopleSeenBy) ask it. Keyed by people as asked.
func (s *service) busySeenOf(ctx context.Context, reader uuid.UUID, people []string) (map[string]bool, error) {
	out := make(map[string]bool, len(people))
	var others, ids []string
	for _, p := range people {
		id, err := uuid.Parse(p)
		switch {
		case err != nil:
			out[p] = false
		case id == reader && reader != uuid.Nil:
			out[p] = true
		default:
			others, ids = append(others, p), append(ids, id.String())
		}
	}
	if len(others) == 0 {
		return out, nil
	}
	members, err := s.seesWhenOthersAreBusy(ctx, reader)
	if err != nil || !members {
		return out, err
	}
	shown, err := freeBusyShownBy(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i, p := range others {
		out[p] = shown[ids[i]]
	}
	return out, nil
}

// seesWhenOthersAreBusy is whether person may see when other members are
// busy at all: a member now, while the workspace lets members see it (O7).
// busySeenOf then asks each person's own choice.
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
	if err := s.store.updateCalendar(ctx, cal, p.Name, p.Hue); err != nil {
		return err
	}
	var changed []string
	if p.Name != nil {
		changed = append(changed, "name")
	}
	if p.Hue != nil {
		changed = append(changed, "hue")
	}
	if len(changed) > 0 {
		s.tellCalendar(ctx, who, resourceEventBusiness.CalendarUpdated, cal, nil, map[string]string{"changed": strings.Join(changed, ",")})
	}
	return nil
}
