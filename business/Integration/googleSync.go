package Integration

// Google Calendar as a calendar sync provider (calendar B6, #42): the
// calendars of a person's Google link, and each calendar's events read
// whole or since a sync token, each as the codec's event (business/ICS), so
// they come into OneCamp through the calendar's one import path, as a file
// does: a series with its rule, the instances it skips and adds, and each
// instance changed on its own; all-day dates and zones; guests with their
// answers; reminders; where it's joined; and its files.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	"github.com/google/uuid"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
)

func init() {
	calendarSyncBusiness.Adapters.Register(googleAdapter{})
}

// The most a pull reads of one calendar, and per page.
const (
	maxPullEvents = 50000
	pullPageSize  = 2500
)

// errNoGoogleLink is a sync asked of someone whose Google link is gone.
var errNoGoogleLink = errors.New("The Google account isn't linked any more: connect it again from Calendars.")

type googleAdapter struct{}

func (googleAdapter) Provider() string { return "google" }

// syncServiceFor is the person's Google Calendar for sync; swapped in tests.
var syncServiceFor = func(ctx context.Context, person string) (*calendar.Service, error) {
	id, err := uuid.Parse(person)
	if err != nil {
		return nil, errNoGoogleLink
	}
	srv, err := googleServiceFor(ctx, id)
	if err != nil {
		return nil, err
	}
	if srv == nil {
		return nil, errNoGoogleLink
	}
	return srv, nil
}

// Calendars are the calendars on the person's Google calendar list, theirs
// first, each with its zone and default reminders, and read-only when it is
// shared with them to see only. Calendars they only see free/busy of are
// left out: there is nothing in them to bring.
func (googleAdapter) Calendars(ctx context.Context, link calendarSyncBusiness.Link) ([]calendarSyncBusiness.Remote, error) {
	srv, err := syncServiceFor(ctx, link.PersonUUID)
	if err != nil {
		return nil, syncErr(err)
	}
	var out []calendarSyncBusiness.Remote
	call := srv.CalendarList.List().MinAccessRole("reader").ShowHidden(true).Context(ctx)
	err = call.Pages(ctx, func(page *calendar.CalendarList) error {
		for _, c := range page.Items {
			if c == nil || c.Deleted {
				continue
			}
			r := calendarSyncBusiness.Remote{Link: link, ID: c.Id, Name: c.Summary, Primary: c.Primary, Zone: c.TimeZone, ReadOnly: c.AccessRole == "reader"}
			if c.SummaryOverride != "" {
				r.Name = c.SummaryOverride
			}
			if c.Primary && link.Account != "" {
				r.Name = link.Account
			}
			for _, d := range c.DefaultReminders {
				if d != nil && d.Method == "popup" {
					r.Reminders = append(r.Reminders, int(d.Minutes))
				}
			}
			if r.Primary {
				out = append([]calendarSyncBusiness.Remote{r}, out...)
			} else {
				out = append(out, r)
			}
		}
		return nil
	})
	return out, syncErr(err)
}

// Pull is what changed in a calendar since cursor ("" for everything), each
// series as its own event and each instance changed on its own as one
// beside it, and the cursor to ask from next. A cursor Google no longer
// knows is ErrFullPull.
func (googleAdapter) Pull(ctx context.Context, cal calendarSyncBusiness.Remote, cursor string) ([]calendarSyncBusiness.Change, string, error) {
	srv, err := syncServiceFor(ctx, cal.Link.PersonUUID)
	if err != nil {
		return nil, "", syncErr(err)
	}
	call := srv.Events.List(cal.ID).SingleEvents(false).ShowDeleted(true).MaxResults(pullPageSize).Context(ctx)
	if cursor != "" {
		call = call.SyncToken(cursor)
	}
	var out []calendarSyncBusiness.Change
	next := ""
	err = call.Pages(ctx, func(page *calendar.Events) error {
		for _, ev := range page.Items {
			if ev == nil || len(out) >= maxPullEvents {
				continue
			}
			out = append(out, changeOf(ev, cal))
		}
		if page.NextSyncToken != "" {
			next = page.NextSyncToken
		}
		return nil
	})
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == http.StatusGone {
		return nil, "", calendarSyncBusiness.ErrFullPull
	}
	if err != nil {
		return nil, "", syncErr(err)
	}
	return out, next, nil
}

// Push makes one of OneCamp's changes at Google: an event made, changed or
// deleted, or one instance of a series changed or cancelled (its id is the
// series' with the instance's original start). Guests are never emailed by
// Google: OneCamp tells them itself. A failure says what kind it is
// (syncErr).
func (googleAdapter) Push(ctx context.Context, op calendarSyncBusiness.Op) (calendarSyncBusiness.Ref, error) {
	ref, err := push(ctx, op)
	return ref, syncErr(err)
}

func push(ctx context.Context, op calendarSyncBusiness.Op) (calendarSyncBusiness.Ref, error) {
	srv, err := syncServiceFor(ctx, op.Remote.Link.PersonUUID)
	if err != nil {
		return calendarSyncBusiness.Ref{}, err
	}
	id := op.ExternalID
	if rid := op.Event.RecurrenceID; rid != nil && id != "" {
		id = instanceID(id, *rid, op.Event.StartDate != "")
	}
	switch op.Kind {
	case "delete":
		err := srv.Events.Delete(op.Remote.ID, id).SendUpdates("none").Context(ctx).Do()
		var gerr *googleapi.Error
		if errors.As(err, &gerr) && (gerr.Code == http.StatusGone || gerr.Code == http.StatusNotFound) {
			err = nil // gone there already
		}
		return calendarSyncBusiness.Ref{ExternalID: op.ExternalID}, err
	case "create":
		// Imported, not inserted: Google keeps the event's UID, so the same
		// event is one event on both sides, and nobody is emailed.
		made, err := srv.Events.Import(op.Remote.ID, googleEventOf(op.Event)).Context(ctx).Do()
		if err != nil {
			return calendarSyncBusiness.Ref{}, err
		}
		return calendarSyncBusiness.Ref{ExternalID: made.Id, ETag: made.Etag}, nil
	case "update":
		ev := googleEventOf(op.Event)
		if op.Event.RecurrenceID != nil {
			ev.Recurrence = nil
		}
		changed, err := srv.Events.Patch(op.Remote.ID, id, ev).SendUpdates("none").Context(ctx).Do()
		if err != nil {
			return calendarSyncBusiness.Ref{}, err
		}
		return calendarSyncBusiness.Ref{ExternalID: op.ExternalID, ETag: changed.Etag}, nil
	}
	return calendarSyncBusiness.Ref{}, fmt.Errorf("calendar sync: no such change as %q", op.Kind)
}

// syncErr is a Google error as calendar sync tells them apart, sorted by
// connectorBusiness.ClassifyAPIError: an account Google no longer answers
// for (revoked, expired, or disconnected here), a calendar the link may only
// read, and a change Google won't take as it is. Anything else, such as a
// rate limit or an outage, stays as it is and is tried again later. Pure.
func syncErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errNoGoogleLink) {
		return fmt.Errorf("%w: %w", calendarSyncBusiness.ErrRevoked, err)
	}
	var gerr *googleapi.Error
	switch connectorBusiness.ClassifyAPIError(err) {
	case connectorBusiness.ProblemExpired:
		return fmt.Errorf("%w: %w", calendarSyncBusiness.ErrRevoked, err)
	case connectorBusiness.ProblemPermissions:
		if errors.As(err, &gerr) && slices.ContainsFunc(gerr.Errors, func(e googleapi.ErrorItem) bool { return e.Reason == "requiredAccessLevel" }) {
			return fmt.Errorf("%w: %w", calendarSyncBusiness.ErrReadOnly, err)
		}
		return fmt.Errorf("%w: %w", calendarSyncBusiness.ErrRefused, err)
	case connectorBusiness.ProblemNotFound:
		return fmt.Errorf("%w: %w", calendarSyncBusiness.ErrRefused, err)
	}
	if errors.As(err, &gerr) && gerr.Code == http.StatusBadRequest {
		return fmt.Errorf("%w: %w", calendarSyncBusiness.ErrRefused, err)
	}
	return err
}

// instanceID is Google's id for one instance of a series: the series' id and
// the instance's original start, in UTC (a date for an all-day series).
// Pure.
func instanceID(series string, rid time.Time, allDay bool) string {
	if allDay {
		return series + "_" + rid.UTC().Format("20060102")
	}
	return series + "_" + rid.UTC().Format("20060102T150405Z")
}

// googleEventOf is the codec's event as Google takes it. Pure.
func googleEventOf(ev ICS.Event) *calendar.Event {
	out := &calendar.Event{Summary: ev.Summary, Description: ev.Description, Location: ev.Location, ICalUID: ev.UID,
		Recurrence: ICS.RecurrenceLines(ev), Reminders: &calendar.EventReminders{UseDefault: false, ForceSendFields: []string{"UseDefault"}}}
	if ev.Transparent {
		out.Transparency = "transparent"
	} else {
		out.Transparency = "opaque"
	}
	if ev.StartDate != "" {
		out.Start, out.End = &calendar.EventDateTime{Date: ev.StartDate}, &calendar.EventDateTime{Date: ev.EndDate}
	} else {
		zone := ev.Zone
		if zone == "" {
			zone = "UTC"
		}
		out.Start = &calendar.EventDateTime{DateTime: ev.Start.UTC().Format(time.RFC3339), TimeZone: zone}
		out.End = &calendar.EventDateTime{DateTime: ev.End.UTC().Format(time.RFC3339), TimeZone: zone}
	}
	if strings.EqualFold(ev.Status, "CANCELLED") {
		out.Status = "cancelled"
	}
	for _, a := range ev.Attendees {
		status := "needsAction"
		for g, i := range googleAnswers {
			if i == a.PartStat {
				status = g
			}
		}
		out.Attendees = append(out.Attendees, &calendar.EventAttendee{Email: a.Email, DisplayName: a.Name, Optional: a.Optional, ResponseStatus: status})
	}
	for _, al := range ev.Alarms {
		out.Reminders.Overrides = append(out.Reminders.Overrides, &calendar.EventReminder{Method: "popup", Minutes: int64(al.Before / time.Minute)})
	}
	return out
}

// Watch isn't asked of Google yet: a channel needs an address Google can
// reach, which a server behind a firewall hasn't, so the syncer asks each
// calendar for its changes every minute instead (business/Calendar
// syncGoogle.go), which every server can.
func (googleAdapter) Watch(context.Context, calendarSyncBusiness.Remote) (calendarSyncBusiness.Watch, error) {
	return calendarSyncBusiness.Watch{}, errors.ErrUnsupported
}

func (googleAdapter) Stop(context.Context, calendarSyncBusiness.Watch) error {
	return errors.ErrUnsupported
}

// changeOf is one Google event as a change. Pure.
func changeOf(ev *calendar.Event, cal calendarSyncBusiness.Remote) calendarSyncBusiness.Change {
	c := calendarSyncBusiness.Change{ExternalID: ev.Id, ETag: ev.Etag, SeriesID: ev.RecurringEventId, Event: eventOf(ev, cal)}
	// An instance cancelled on its own is a skipped instance of its series,
	// not a deletion of the series.
	c.Deleted = ev.Status == "cancelled" && ev.RecurringEventId == ""
	return c
}

// googleAnswers are Google's responseStatus as iCalendar says them.
var googleAnswers = map[string]string{"accepted": ICS.Accepted, "declined": ICS.Declined, "tentative": ICS.Tentative, "needsAction": ICS.NeedsAction}

// eventOf is a Google event as the codec's event. Pure.
func eventOf(ev *calendar.Event, cal calendarSyncBusiness.Remote) ICS.Event {
	out := ICS.Event{UID: ev.ICalUID, Sequence: int(ev.Sequence), Summary: ev.Summary, Description: googleNotes(ev.Description),
		Location: ev.Location, Transparent: ev.Transparency == "transparent", Status: strings.ToUpper(ev.Status)}
	if out.UID == "" {
		out.UID = ev.Id + "@google.com"
	}
	if t, err := time.Parse(time.RFC3339, ev.Updated); err == nil {
		out.Stamp = t.UTC()
	}
	zone := cal.Zone
	if ev.Start != nil {
		if ev.Start.Date != "" {
			out.StartDate = ev.Start.Date
		} else if t, err := time.Parse(time.RFC3339, ev.Start.DateTime); err == nil {
			out.Start = t.UTC()
		}
		if ev.Start.TimeZone != "" {
			zone = ev.Start.TimeZone
		}
	}
	if ev.End != nil {
		if ev.End.Date != "" {
			out.EndDate = ev.End.Date
		} else if t, err := time.Parse(time.RFC3339, ev.End.DateTime); err == nil {
			out.End = t.UTC()
		}
	}
	if out.StartDate == "" {
		out.Zone = zone
	}
	if len(ev.Recurrence) > 0 {
		out.RRule, out.ExDates, out.RDates = ICS.ReadRecurrence(ev.Recurrence, zone)
	}
	if o := ev.OriginalStartTime; o != nil && ev.RecurringEventId != "" {
		if o.Date != "" {
			if d, err := time.Parse("2006-01-02", o.Date); err == nil {
				out.RecurrenceID = &d
			}
		} else if t, err := time.Parse(time.RFC3339, o.DateTime); err == nil {
			u := t.UTC()
			out.RecurrenceID = &u
		}
	}
	if o := ev.Organizer; o != nil {
		out.Organizer = ICS.Address{Name: o.DisplayName, Email: o.Email}
	}
	for _, a := range ev.Attendees {
		if a == nil || a.Resource {
			continue
		}
		out.Attendees = append(out.Attendees, ICS.Attendee{Address: ICS.Address{Name: a.DisplayName, Email: a.Email},
			Optional: a.Optional, PartStat: googleAnswers[a.ResponseStatus]})
	}
	if r := ev.Reminders; r != nil {
		minutes := []int{}
		if r.UseDefault {
			minutes = cal.Reminders
		}
		for _, o := range r.Overrides {
			if o != nil && o.Method == "popup" {
				minutes = append(minutes, int(o.Minutes))
			}
		}
		for _, m := range minutes {
			out.Alarms = append(out.Alarms, ICS.Alarm{Before: time.Duration(m) * time.Minute})
		}
	}
	out.Conference = ev.HangoutLink
	if cd := ev.ConferenceData; cd != nil {
		for _, ep := range cd.EntryPoints {
			if ep != nil && ep.EntryPointType == "video" && ep.Uri != "" {
				out.Conference = ep.Uri
				break
			}
		}
	}
	for _, a := range ev.Attachments {
		if a != nil && a.FileUrl != "" {
			out.Attachments = append(out.Attachments, ICS.Attachment{Name: a.Title, URL: a.FileUrl})
		}
	}
	return out
}

// googleNotes is an event's description as text: Google keeps it as HTML
// when it was written in Google Calendar.
func googleNotes(s string) string {
	if !strings.ContainsAny(s, "<&") {
		return s
	}
	doc, err := contentcodec.FromHTML(s, contentcodec.HTMLOptions{Foreign: true})
	if err != nil {
		return s
	}
	return strings.TrimSpace(contentcodec.PlainText(doc))
}
