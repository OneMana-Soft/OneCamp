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

import (
	"context"
	"errors"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
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
}

// feedRefresh is how often a feed asks to be read again.
const feedRefresh = time.Hour

// feedsAllowed is the workspace's calendar.feeds.allowed policy; tests swap it.
var feedsAllowed = func(ctx context.Context) bool {
	return policyBusiness.Policies.Bool(ctx, policyBusiness.CalendarFeedsAllowed)
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
// see as busy.
func (s *service) feedCalendar(ctx context.Context, who accessBusiness.Subject, id string) (uuid.UUID, accessBusiness.Decision, error) {
	cal, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return uuid.Nil, accessBusiness.Decision{}, ErrEventNotFound
	}
	decided, err := s.decideCalendars(ctx, who, []string{cal.String()})
	if err != nil {
		return uuid.Nil, accessBusiness.Decision{}, err
	}
	d, ok := decided[cal.String()]
	if !ok || d.Level < accessBusiness.LevelView {
		return uuid.Nil, accessBusiness.Decision{}, ErrEventNotFound
	}
	if subscribedOnly(d) {
		// Someone else's calendar, read from its own address: that address
		// is what another app subscribes to, never a link of the person's.
		return uuid.Nil, accessBusiness.Decision{}, errFeedOfSubscribed
	}
	return cal, d, nil
}

// mostOf is the most a feed of a calendar may show, given what its person
// may do with the calendar. Pure.
func mostOf(d accessBusiness.Decision) string {
	if busyOnly(d) {
		return models.FeedBusy
	}
	return models.FeedFull
}

// Feed says whether who has a feed of a calendar they may see, and at what
// scope; its link was given once, when it was made.
func (s *service) Feed(ctx context.Context, who *model.UserInfo, id string) (*FeedView, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	cal, d, err := s.feedCalendar(ctx, subjectOf(who), id)
	if err != nil {
		return nil, err
	}
	most := mostOf(d)
	f, err := s.store.liveFeed(ctx, cal, personOf(who))
	if err != nil {
		return nil, err
	}
	if f == nil {
		return &FeedView{Most: most}, nil
	}
	at := f.CreatedAt
	return &FeedView{Live: true, Scope: f.Scope, CreatedAt: &at, Most: most}, nil
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
	cal, d, err := s.feedCalendar(ctx, subjectOf(who), id)
	if err != nil {
		return nil, err
	}
	most := mostOf(d)
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
	if err := s.store.makeFeed(ctx, f); err != nil {
		return nil, err
	}
	at := f.CreatedAt
	return &FeedView{Live: true, URL: feedURL(raw), Scope: scope, CreatedAt: &at, Most: most}, nil
}

// StopFeed stops who's feed of a calendar: its link answers nothing at once.
func (s *service) StopFeed(ctx context.Context, who *model.UserInfo, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	cal, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return ErrEventNotFound
	}
	return s.store.revokeFeed(ctx, cal, personOf(who))
}

// FeedICS is the calendar a feed's token names, as its person may see it
// now. ErrEventNotFound, the same for each, for a token revoked or unknown,
// a workspace that turned links off, a person no longer a member, a calendar
// they may no longer see, and a full link to one they now see only as busy.
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
	if f == nil {
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
	_, onCalendar, err := s.feedCalendar(ctx, who, f.CalendarID.String())
	if errors.Is(err, errFeedOfSubscribed) {
		return nil, ErrEventNotFound
	}
	if err != nil {
		return nil, err
	}
	most := mostOf(onCalendar)
	// A link made to show what is on it answers nothing once its person may
	// see only when (a share stopped): never fewer details in its place.
	if f.Scope == models.FeedFull && most == models.FeedBusy {
		return nil, ErrEventNotFound
	}
	busy := f.Scope == models.FeedBusy
	rows, err := s.store.rangeOnCalendars(ctx, []uuid.UUID{f.CalendarID}, everything, maxExportEvents)
	if err != nil {
		return nil, err
	}
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
		hide := busy || hiddenFrom(e, guests[e.ID], f.PersonID, onCalendar)
		for _, ev := range each[i] {
			if hide {
				ev = busyICS(ev)
			}
			// A feed rings nothing: the calendar app that reads it keeps its own reminders.
			ev.Alarms = nil
			evs = append(evs, ev)
		}
	}
	name, err := s.feedName(ctx, who, f)
	if err != nil {
		return nil, err
	}
	return ICS.WriteMessage(ICS.Message{Method: "PUBLISH", Name: name, Refresh: feedRefresh, Events: evs}), nil
}

// feedName is what a feed's calendar is called where it is read: its name,
// a project's, or its owner's; the person's own is "OneCamp".
func (s *service) feedName(ctx context.Context, who accessBusiness.Subject, f *models.Feed) (string, error) {
	cals, err := s.store.calendarsByID(ctx, []uuid.UUID{f.CalendarID})
	if err != nil {
		return "", err
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

// busyICS is an event as a busy feed carries it: when, how it repeats and
// whether it was called off, and "Busy". Pure.
func busyICS(ev ICS.Event) ICS.Event {
	return ICS.Event{UID: ev.UID, Sequence: ev.Sequence, Stamp: ev.Stamp, Start: ev.Start, End: ev.End, StartDate: ev.StartDate, EndDate: ev.EndDate,
		Zone: ev.Zone, RRule: ev.RRule, ExDates: ev.ExDates, RDates: ev.RDates, RecurrenceID: ev.RecurrenceID, Status: ev.Status, Summary: "Busy"}
}
