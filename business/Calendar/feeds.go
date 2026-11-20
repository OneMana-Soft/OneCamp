package Calendar

// Secret feeds of calendars (calendar B8, SY-11, basecamp-05): a link of a
// person's own to any calendar they may see (theirs, one an import made, a
// project's, one shared with them) that Google Calendar, Apple Calendar or
// Outlook subscribes to.
//
// The link is a bearer secret. Only its hash is kept (helpers.LinkSecretHash,
// as for every link that is someone's credential), so it is shown once, when
// it is made; reset makes another and the old one answers nothing at once.
// Nothing here writes it, or its hash, to a log. Each read is the calendar as
// its person may see it at that moment, through A4's one writer (icsOf): a
// lost share or a left project answers nothing, a private event someone isn't
// on reads "Busy", and a busy feed carries only when. The workspace's
// calendar.feeds.allowed turns every link off at once.
//
// A person's link may instead be to everything they may see (basecamp-05,
// WorkspaceFeedID): each calendar they may see when it is read, shown as that
// calendar's own link shows it, and the events they are on elsewhere, as the
// event rule lets them read those. Only a member has one. A workspace admin
// sees every live link (whose, of what, since when; never its address) and
// may stop any, which the audit log records and its person is shown.

import (
	"context"
	"errors"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// FeedView is a person's secret feed of a calendar, as they see it: the
// link only when it has just been made.
type FeedView struct {
	// Live says they have a feed of it; URL is its link, given once.
	Live bool   `json:"live"`
	URL  string `json:"url,omitempty"`
	// Scope is "busy" (when only) or "full" (what too).
	Scope     string     `json:"scope,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	// Most is the most a feed of it may show this person: "busy" for a
	// calendar they see only as busy (owner decision O7).
	Most string `json:"most"`
	// StoppedBy is who stopped their last feed of it, when that was someone
	// else (a workspace admin) and they have none live now, and StoppedAt when.
	StoppedBy *Person    `json:"stopped_by,omitempty"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	// LinksAllowed is whether the workspace allows links to calendars now,
	// so a member knows before they make one (basecamp-05 H1): what a Make's
	// refusal would tell them, the same for every member. Set by feedView.
	LinksAllowed bool `json:"links_allowed"`
}

// WorkspaceFeedID names a person's feed of everything they may see, where a
// calendar's id names a feed of that calendar.
const WorkspaceFeedID = "workspace"

// workspaceFeedName is what a feed of everything is called where it is read.
const workspaceFeedName = "OneCamp: all calendars"

// feedRefresh is how often a feed asks to be read again.
const feedRefresh = time.Hour

// feedReadEvery is how often a link's last read is written at most: a
// calendar app asks every feedRefresh, so about once per read, and never
// more however often a link is asked for.
const feedReadEvery = time.Hour

// feedsAllowed is the workspace's calendar.feeds.allowed policy; tests swap it.
var feedsAllowed = func(ctx context.Context) bool {
	return policyBusiness.Policies.Bool(ctx, policyBusiness.CalendarFeedsAllowed)
}

// feedView is v as its person reads it, with whether the workspace allows
// links now: every feed read answers through it, so none leaves that out.
func feedView(ctx context.Context, v FeedView) *FeedView {
	v.LinksAllowed = feedsAllowed(ctx)
	return &v
}

// feedURL is the public address of a feed's token: the API's own, which a
// calendar app reads with no session.
func feedURL(raw string) string {
	return authService.BackendBaseURL() + "/public/calendar/" + raw + ".ics"
}

// FeedTokenOf is the token in a feed's last path element ("….ics"). Pure.
func FeedTokenOf(file string) string { return strings.TrimSuffix(strings.TrimSpace(file), ".ics") }

// feedCalendar is a calendar who may have a feed of, and the most it may
// show: everything for a calendar they see whole, only when for one they
// see as busy; with its row as the rule read it (cals).
func (s *service) feedCalendar(ctx context.Context, who accessBusiness.Subject, id string) (uuid.UUID, accessBusiness.Decision, []models.Calendar, error) {
	cal, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return uuid.Nil, accessBusiness.Decision{}, nil, ErrEventNotFound
	}
	decided, cals, err := s.decideCalendarRows(ctx, who, []string{cal.String()})
	if err != nil {
		return uuid.Nil, accessBusiness.Decision{}, nil, err
	}
	d, ok := decided[cal.String()]
	if !ok || d.Level < accessBusiness.LevelView {
		return uuid.Nil, accessBusiness.Decision{}, nil, ErrEventNotFound
	}
	if subscribedOnly(d) {
		// Someone else's calendar, read from its own address: that address
		// is what another app subscribes to, never a link of the person's.
		return uuid.Nil, accessBusiness.Decision{}, nil, errFeedOfSubscribed
	}
	return cal, d, cals, nil
}

// mostOf is the most a feed of a calendar may show, given what its person
// may do with the calendar. Pure.
func mostOf(d accessBusiness.Decision) string {
	if busyOnly(d) {
		return models.FeedBusy
	}
	return models.FeedFull
}

// feedTarget is what a feed named by id is of, for who, and the most it may
// show: a calendar they may see (feedCalendar), or (WorkspaceFeedID, calendar
// uuid.Nil) everything they may see, which only a member now may have, each
// calendar in it masked as its own feed is.
func (s *service) feedTarget(ctx context.Context, who *model.UserInfo, id string) (uuid.UUID, string, error) {
	if strings.TrimSpace(id) != WorkspaceFeedID {
		cal, d, _, err := s.feedCalendar(ctx, subjectOf(who), id)
		return cal, mostOf(d), err
	}
	me := personOf(who)
	members, err := s.store.membersNow(ctx, []uuid.UUID{me})
	if err != nil {
		return uuid.Nil, "", err
	}
	if me == uuid.Nil || !members[me] {
		return uuid.Nil, "", refusal("Only members of the workspace can have a link to all their calendars.")
	}
	return uuid.Nil, models.FeedFull, nil
}

// Feed says whether who has a feed of a calendar they may see, and at what
// scope, and whether the workspace allows links now; its link was given
// once, when it was made.
func (s *service) Feed(ctx context.Context, who *model.UserInfo, id string) (*FeedView, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	cal, most, err := s.feedTarget(ctx, who, id)
	if err != nil {
		return nil, err
	}
	me := personOf(who)
	f, err := s.store.latestFeed(ctx, cal, me)
	if err != nil {
		return nil, err
	}
	v := FeedView{Most: most}
	switch {
	case f == nil:
	case f.RevokedAt != nil:
		if f.RevokedBy != nil && *f.RevokedBy != me {
			names := s.namesByID(ctx, []string{f.RevokedBy.String()})
			by := personNamed(*f.RevokedBy, names)
			v.StoppedBy, v.StoppedAt = &by, f.RevokedAt
		}
	default:
		at := f.CreatedAt
		v.Live, v.Scope, v.CreatedAt = true, f.Scope, &at
	}
	return feedView(ctx, v), nil
}

// MakeFeed makes who a new feed of a calendar they may see, showing when
// only ("busy") or what too ("full"), and gives its link this once; a feed
// they had stops at once.
func (s *service) MakeFeed(ctx context.Context, who *model.UserInfo, id, scope string) (*FeedView, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if !feedsAllowed(ctx) {
		return nil, inputError("Your workspace has turned off links to calendars.")
	}
	cal, most, err := s.feedTarget(ctx, who, id)
	if err != nil {
		return nil, err
	}
	switch {
	case scope != models.FeedBusy && scope != models.FeedFull:
		return nil, inputError("Choose when you're busy, or everything.")
	case scope == models.FeedFull && most == models.FeedBusy:
		return nil, inputError("You see only when they're busy, so the link can show only that.")
	}
	raw, hash, err := helpers.NewLinkSecret()
	if err != nil {
		return nil, err
	}
	f := models.Feed{ID: s.newID(), CalendarID: cal, PersonID: personOf(who), TokenHash: hash, Scope: scope, CreatedAt: s.now()}
	replaced, err := s.store.makeFeed(ctx, f)
	if err != nil {
		return nil, err
	}
	s.tellFeed(ctx, who, resourceEventBusiness.CalendarFeedMade, f, f.Scope, replaced)
	at := f.CreatedAt
	return feedView(ctx, FeedView{Live: true, URL: feedURL(raw), Scope: scope, CreatedAt: &at, Most: most}), nil
}

// StopFeed stops who's feed of a calendar: its link answers nothing at once.
// A stop with no live feed changes nothing and tells nothing.
func (s *service) StopFeed(ctx context.Context, who *model.UserInfo, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	var cal uuid.UUID
	if strings.TrimSpace(id) != WorkspaceFeedID {
		var err error
		if cal, err = uuid.Parse(strings.TrimSpace(id)); err != nil {
			return ErrEventNotFound
		}
	}
	me := personOf(who)
	stopped, err := s.store.revokeFeed(ctx, cal, me)
	if err != nil {
		return err
	}
	if stopped != nil {
		// Told under the stopped feed's own id, as its making was.
		s.tellFeed(ctx, who, resourceEventBusiness.CalendarFeedStopped, *stopped, stopped.Scope, "")
	}
	return nil
}

// tellFeed tells a link to a calendar made or stopped once the store has it,
// for the audit log's "calendar" (V-398): the scope the store kept or
// stopped, and for a new link the scope of the live one it stopped. Never the
// link's address or token, which only its maker sees, once.
//
// A feed of everything is told under its own id, with target "workspace";
// one stopped by someone else (a workspace admin) names its person.
func (s *service) tellFeed(ctx context.Context, who *model.UserInfo, kind resourceEventBusiness.Kind, f models.Feed, scope, replaced string) {
	data := map[string]string{"scope": scope}
	if replaced != "" {
		data["replaced"] = replaced
	}
	resource := f.CalendarID
	if resource == uuid.Nil {
		resource, data["target"] = f.ID, WorkspaceFeedID
	}
	var subjects []string
	if f.PersonID != personOf(who) {
		subjects, data["by"] = []string{f.PersonID.String()}, "admin"
	}
	s.tellCalendar(ctx, who, kind, resource, subjects, data)
}

// FeedICS is the calendar a feed's token names, as its person may see it
// now. ErrEventNotFound, the same for each, for a token revoked or unknown,
// a workspace that turned links off, a person no longer a member, a calendar
// they may no longer see, and a full link to one they now see only as busy.
// A feed of everything is every calendar they may see now, each masked as
// its own feed is, and the events they are on elsewhere (everythingFor).
func (s *service) FeedICS(ctx context.Context, token string) ([]byte, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if token == "" || !feedsAllowed(ctx) {
		return nil, ErrEventNotFound
	}
	f, err := s.store.feedByHash(ctx, helpers.LinkSecretHash(token))
	if err != nil {
		return nil, err
	}
	if f == nil || !helpers.LinkSecretMatches(token, f.TokenHash) {
		return nil, ErrEventNotFound
	}
	members, err := s.store.membersNow(ctx, []uuid.UUID{f.PersonID})
	if err != nil {
		return nil, err
	}
	if !members[f.PersonID] {
		return nil, ErrEventNotFound
	}
	who := accessBusiness.Subject{UserUUID: f.PersonID.String()}
	// Their graph uid, read once here for every rule the read asks (the
	// calendar's container, its tasks) instead of once by each.
	if who.UserUID, err = uidOf(ctx, who); err != nil {
		return nil, err
	}
	var rows []*models.Event
	var cals []models.Calendar
	onCalendar := map[uuid.UUID]accessBusiness.Decision{}
	if f.CalendarID == uuid.Nil {
		if rows, onCalendar, err = s.everythingFor(ctx, who, f.PersonID); err != nil {
			return nil, err
		}
	} else {
		_, d, read, err := s.feedCalendar(ctx, who, f.CalendarID.String())
		if errors.Is(err, errFeedOfSubscribed) {
			return nil, ErrEventNotFound
		}
		if err != nil {
			return nil, err
		}
		// A link made to show what is on it answers nothing once its person may
		// see only when (a share stopped): never fewer details in its place.
		if f.Scope == models.FeedFull && mostOf(d) == models.FeedBusy {
			return nil, ErrEventNotFound
		}
		onCalendar[f.CalendarID] = d
		if rows, err = s.store.rangeOnCalendars(ctx, []uuid.UUID{f.CalendarID}, everything, maxExportEvents); err != nil {
			return nil, err
		}
		cals = read
	}
	busy := f.Scope == models.FeedBusy
	if busy {
		// Free time isn't busy: it isn't in a busy feed at all.
		kept := rows[:0:0]
		for _, e := range rows {
			if e.Transparency != transparencyTransparent {
				kept = append(kept, e)
			}
		}
		rows = kept
	}
	guests, err := s.store.attendees(ctx, idsOf(rows))
	if err != nil {
		return nil, err
	}
	each, err := s.icsOf(ctx, f.PersonID, rows)
	if err != nil {
		return nil, err
	}
	var evs []ICS.Event
	for i, e := range rows {
		hide := busy || hiddenFrom(e, guests[e.ID], f.PersonID, onCalendar[e.CalendarID])
		for _, ev := range each[i] {
			if hide {
				ev = busyICS(ev)
			}
			// A feed rings nothing: the calendar app that reads it keeps its own reminders.
			ev.Alarms = nil
			evs = append(evs, ev)
		}
	}
	if !busy {
		// A task is free time, so a busy link carries none.
		work, err := s.projectFeedTasks(ctx, who, cals)
		if err != nil {
			return nil, err
		}
		evs = append(evs, work...)
	}
	name, err := s.feedName(ctx, who, f, cals)
	if err != nil {
		return nil, err
	}
	// Only a read that answered is marked; one that can't be marked still
	// answers (FeedRead logs it).
	_ = s.store.feedRead(ctx, f.ID, feedReadEvery)
	return ICS.WriteMessage(ICS.Message{Method: "PUBLISH", Name: name, Refresh: feedRefresh, Events: evs}), nil
}

// everythingFor is what a feed of everything carries for person, read now:
// the events on every calendar they may see (seenCalendars, the calendar
// list's one rule, less those read from an address, which no link of theirs
// carries), with each calendar's decision for the one mask (hiddenFrom); and
// the events they are on whose calendar they don't see, kept as the event
// rule (decideMany) lets them read each.
func (s *service) everythingFor(ctx context.Context, who accessBusiness.Subject, person uuid.UUID) ([]*models.Event, map[uuid.UUID]accessBusiness.Decision, error) {
	seen, decided, err := s.seenCalendars(ctx, who, person)
	if err != nil {
		return nil, nil, err
	}
	onCalendar := map[uuid.UUID]accessBusiness.Decision{}
	var ids []uuid.UUID
	for _, c := range seen {
		if d := decided[c.ID.String()]; !subscribedOnly(d) {
			onCalendar[c.ID] = d
			ids = append(ids, c.ID)
		}
	}
	rows, err := s.store.rangeOnCalendars(ctx, ids, everything, maxExportEvents)
	if err != nil {
		return nil, nil, err
	}
	mine, err := s.store.rangeOf(ctx, person, everything, maxExportEvents)
	if err != nil {
		return nil, nil, err
	}
	have := map[uuid.UUID]bool{}
	for _, e := range rows {
		have[e.ID] = true
	}
	var elsewhere []*models.Event
	var refs []string
	for _, e := range mine {
		if _, onSeen := onCalendar[e.CalendarID]; !onSeen && !have[e.ID] && e.Source != sourceSubscribed {
			elsewhere = append(elsewhere, e)
			refs = append(refs, e.ID.String())
		}
	}
	if len(elsewhere) == 0 {
		return rows, onCalendar, nil
	}
	readable, err := s.decideMany(ctx, who, refs)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range elsewhere {
		if readable[e.ID.String()].Level >= accessBusiness.LevelView {
			rows = append(rows, e)
		}
	}
	return rows, onCalendar, nil
}

// feedTaskLookback is how far into the past a link's tasks reach: a
// quarter, so the project's recent work stays on the calendar while the read's
// cap (MaxOpenTasks, soonest due first) goes on what is coming, not on years
// of finished work. Its far end is the feed's own (everything).
const feedTaskLookback = 90 * 24 * time.Hour

// projectFeedTasks are the dated tasks a link to a project's calendar
// carries (cals, the link's calendar; none for any other calendar): the
// project calendar's own read (containerTasks, as the app shows it), read as
// the link's person who may read each now, each all day over its days under
// a UID that stays the task's. A person's link and the workspace's carry
// what they did.
func (s *service) projectFeedTasks(ctx context.Context, who accessBusiness.Subject, cals []models.Calendar) ([]ICS.Event, error) {
	on, ids := containersOf(cals)
	from := s.now().UTC().Truncate(24 * time.Hour).Add(-feedTaskLookback)
	entries, err := s.containerTasks(taskread.ActingFor(ctx, who.UserUUID), who, ids[containerProject], nil, on, from, everything.To)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	now := s.now()
	out := make([]ICS.Event, 0, len(entries))
	for _, e := range entries {
		out = append(out, ICS.Event{UID: e.Source.Kind + "-" + e.Source.ID + "@onecamp", Stamp: now, Summary: e.Title,
			StartDate: e.Start.Date, EndDate: e.End.Date, Transparent: e.Transparency == transparencyTransparent})
	}
	return out, nil
}

// feedName is what a feed's calendar (cals, as read) is called where it is
// read: its name, a project's, or its owner's; the person's own is
// "OneCamp", and a feed of everything workspaceFeedName.
func (s *service) feedName(ctx context.Context, who accessBusiness.Subject, f *models.Feed, cals []models.Calendar) (string, error) {
	if f.CalendarID == uuid.Nil {
		return workspaceFeedName, nil
	}
	if len(cals) == 1 && cals[0].Kind == "personal" && cals[0].OwnerID != nil && *cals[0].OwnerID == f.PersonID {
		return "OneCamp", nil
	}
	names, err := s.calendarNames(ctx, who, cals)
	if err != nil {
		return "", err
	}
	return names[f.CalendarID], nil
}

// maxListedFeeds is how many live links an admin's list reads.
const maxListedFeeds = 500

// FeedListing is a live link as a workspace admin sees it: whose, of what
// ("calendar" or "workspace"), showing what, made when. Never its address,
// its token or its hash.
type FeedListing struct {
	ID        string        `json:"id"`
	Person    Person        `json:"person"`
	Target    string        `json:"target"`
	Calendar  *FeedCalendar `json:"calendar,omitempty"`
	Scope     string        `json:"scope"`
	CreatedAt time.Time     `json:"created_at"`
	// LastReadAt is when the link was last read, to the hour; none when never.
	LastReadAt *time.Time `json:"last_read_at,omitempty"`
}

// FeedCalendar is the calendar a listed link is of: its name only when the
// admin may see that calendar themselves.
type FeedCalendar struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

// FeedList is the workspace's live links, newest first, and whether there
// were more than it holds.
type FeedList struct {
	Feeds []FeedListing `json:"feeds"`
	More  bool          `json:"more"`
}

// Feeds are the workspace's live links, for a workspace admin (the /admin
// gate is the check: VerifyAdminAuthOnlyPostgres).
func (s *service) Feeds(ctx context.Context, admin *model.UserInfo) (*FeedList, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	live, err := s.store.liveFeeds(ctx, maxListedFeeds+1)
	if err != nil {
		return nil, err
	}
	out := &FeedList{Feeds: []FeedListing{}, More: len(live) > maxListedFeeds}
	if out.More {
		live = live[:maxListedFeeds]
	}
	var people, calIDs []string
	var cals []uuid.UUID
	for _, f := range live {
		people = append(people, f.PersonID.String())
		if f.CalendarID != uuid.Nil {
			cals, calIDs = append(cals, f.CalendarID), append(calIDs, f.CalendarID.String())
		}
	}
	names := s.namesByID(ctx, people)
	byID := map[uuid.UUID]models.Calendar{}
	calNames := map[uuid.UUID]string{}
	if len(cals) > 0 {
		got, err := s.store.calendarsByID(ctx, cals)
		if err != nil {
			return nil, err
		}
		decided, err := s.decideCalendars(ctx, subjectOf(admin), calIDs)
		if err != nil {
			return nil, err
		}
		var visible []models.Calendar
		for _, c := range got {
			byID[c.ID] = c
			if decided[c.ID.String()].Level >= accessBusiness.LevelView {
				visible = append(visible, c)
			}
		}
		if calNames, err = s.calendarNames(ctx, subjectOf(admin), visible); err != nil {
			return nil, err
		}
	}
	for _, f := range live {
		l := FeedListing{ID: f.ID.String(), Person: personNamed(f.PersonID, names), Target: WorkspaceFeedID, Scope: f.Scope, CreatedAt: f.CreatedAt, LastReadAt: f.LastReadAt}
		if f.CalendarID != uuid.Nil {
			l.Target = KindCalendar
			l.Calendar = &FeedCalendar{ID: f.CalendarID.String(), Kind: byID[f.CalendarID].Kind, Name: calNames[f.CalendarID]}
		}
		out.Feeds = append(out.Feeds, l)
	}
	return out, nil
}

// StopAnyFeed stops one live link by its id, for a workspace admin: it
// answers nothing at once, the audit log records who stopped whose, and its
// person sees who stopped it (Feed). ErrEventNotFound for an id no live link
// has.
func (s *service) StopAnyFeed(ctx context.Context, admin *model.UserInfo, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	feed, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return ErrEventNotFound
	}
	f, err := s.store.revokeFeedByID(ctx, feed, personOf(admin))
	if err != nil {
		return err
	}
	if f == nil {
		return ErrEventNotFound
	}
	s.tellFeed(ctx, admin, resourceEventBusiness.CalendarFeedStopped, *f, f.Scope, "")
	return nil
}

// busyICS is an event as a busy feed carries it: when, how it repeats and
// whether it was called off, and "Busy". Pure.
func busyICS(ev ICS.Event) ICS.Event {
	return ICS.Event{UID: ev.UID, Sequence: ev.Sequence, Stamp: ev.Stamp, Start: ev.Start, End: ev.End, StartDate: ev.StartDate, EndDate: ev.EndDate,
		Zone: ev.Zone, RRule: ev.RRule, ExDates: ev.ExDates, RDates: ev.RDates, RecurrenceID: ev.RecurrenceID, Status: ev.Status, Summary: "Busy"}
}
