package Calendar

// The calendar programme's contracts in this package
// (design/calendar-vs-gcal/PROGRAM.md §4.2 #38 and #41, §5): the event service
// A1 built, and the status chat reads (StatusFor, which A3 builds and chat A7
// shows as "In focus until 15:00", "Away until Monday").
//
// Events is the calendar in its one store (service.go): every read and write
// of an event for the person asking, with sequences, zones and the trash. On a
// server that keeps the two old stores (CALENDAR_STORE=graph) it answers
// ErrStoreOff, and the /event routes keep them (legacy.go). Statuses reads the person's events at a moment as the busy
// engine reads them (eventsOf), from whichever store is on; A3 adds "busy".

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Service is every read and write of an event, for the person asking; the
// rules (who may see, change or answer an event) are its own, and so are the
// errors (ErrEventNotFound, ErrOnlyCreatorChanges, ErrNotEventCreator,
// ErrCreatorCannotLeave, *EventInputError, *StaleError, ErrStoreOff,
// ErrMoving).
type Service interface {
	// Get is one event the person is on: an event, a series by its own id,
	// or one instance of a series by the instance's ("<series id>_<start>").
	Get(ctx context.Context, who *model.UserInfo, id string) (*Event, error)
	// Range is the person's events between from and to (at most 400 days),
	// each series as its instances, and whether there were more than one
	// answer holds.
	Range(ctx context.Context, who *model.UserInfo, from, to time.Time) ([]Event, bool, error)
	Create(ctx context.Context, who *model.UserInfo, in EventInput) (*Event, error)
	// Update changes an event at the sequence the writer read (CAL2); for an
	// instance, for the scope the patch says.
	Update(ctx context.Context, who *model.UserInfo, id string, p EventPatch) (*Event, error)
	// Delete puts an event in its organiser's trash, or, for an instance,
	// this event, this and following, or all events (scope); a sequence, when
	// given, must be what the writer read.
	Delete(ctx context.Context, who *model.UserInfo, id, scope string, sequence *int) error
	// Restore brings one back from the trash.
	Restore(ctx context.Context, who *model.UserInfo, id string) (*Event, error)
	// Trash is the person's deleted events, newest first.
	Trash(ctx context.Context, who *model.UserInfo, before *time.Time) ([]Event, error)
	// Respond is a guest's answer to an event they are on, for all of it.
	Respond(ctx context.Context, who *model.UserInfo, id string, answer Answer) error
	// Answer is a guest's answer with a note, and another time proposed, for
	// all of an event or series, or for one instance alone (Scope "this").
	Answer(ctx context.Context, who *model.UserInfo, id string, in AnswerInput) (*Event, error)
	// DismissProposal sets aside a guest's proposal of another time (for
	// whoever may change the event).
	DismissProposal(ctx context.Context, who *model.UserInfo, id string, in DismissInput) (*Event, error)
	// Nudge asks again every guest who hasn't answered (the organiser's).
	Nudge(ctx context.Context, who *model.UserInfo, id string) (int, error)
	// GuestLink is a guest by email's answer link (the organiser's).
	GuestLink(ctx context.Context, who *model.UserInfo, id, email string) (string, error)
	// AddCall gives an event (a series, from any of its instances) its own
	// call, or answers the one it has; RemoveCall takes it off and ends it.
	// Both for whoever may change the event (calendar B11, eventCall.go).
	AddCall(ctx context.Context, who *model.UserInfo, id string) (*dgraphStruct.EventCall, error)
	RemoveCall(ctx context.Context, who *model.UserInfo, id string) error
	// Reminders is the person's own reminders for an event or series (an
	// instance's are its series'), and whether they are the default's.
	Reminders(ctx context.Context, who *model.UserInfo, id string) (Reminders, error)
	// SetReminders sets them (nil goes back to the default; empty is none).
	SetReminders(ctx context.Context, who *model.UserInfo, id string, minutes []int) (Reminders, error)
	// Snooze brings a reminder of an occurrence back: in five minutes, when
	// it starts, or a minute before. It says when.
	Snooze(ctx context.Context, who *model.UserInfo, id string, until SnoozeFor) (time.Time, error)
	// Export writes every event the person is on as one iCalendar file, and
	// says how many events it holds.
	Export(ctx context.Context, who *model.UserInfo, to io.Writer) (int, error)
	// MoveFromGoogle starts moving every calendar of the person's Google link
	// into OneCamp, and SyncStatus says where it stands.
	MoveFromGoogle(ctx context.Context, who *model.UserInfo) (*SyncStatus, error)
	// ProjectCalendarFor is a project's own calendar, made the first time,
	// for someone who may see the project.
	ProjectCalendarFor(ctx context.Context, who *model.UserInfo, projectUUID string) (uuid.UUID, error)
	// Calendars are the calendars the person may see; CalendarEvents the
	// events on those of them asked for, between from and to.
	Calendars(ctx context.Context, who *model.UserInfo) ([]CalendarView, error)
	CalendarEvents(ctx context.Context, who *model.UserInfo, ids []string, from, to time.Time) ([]Event, bool, error)
	// PersonCalendar is a member's own calendar, to lay over the asker's;
	// Shares and Share are who a calendar the asker manages is shared with.
	PersonCalendar(ctx context.Context, who *model.UserInfo, personUUID string) (uuid.UUID, error)
	Shares(ctx context.Context, who *model.UserInfo, id string) ([]CalendarShare, error)
	Share(ctx context.Context, who *model.UserInfo, id, personUUID, level string) error
	// UpdateCalendar renames or recolours a calendar the asker manages.
	UpdateCalendar(ctx context.Context, who *model.UserInfo, id string, p CalendarPatch) error
	// Secret feeds (calendar B8): whether who has one of a calendar, a new
	// one (its link given once), none, and the calendar a link names.
	Feed(ctx context.Context, who *model.UserInfo, id string) (*FeedView, error)
	MakeFeed(ctx context.Context, who *model.UserInfo, id, scope string) (*FeedView, error)
	StopFeed(ctx context.Context, who *model.UserInfo, id string) error
	FeedICS(ctx context.Context, token string) ([]byte, error)
	// Calendars read from an address (calendar B8): who's, one added (read
	// once first), one read now, and one removed with everything on it.
	Subscriptions(ctx context.Context, who *model.UserInfo) ([]SubscriptionView, error)
	Subscribe(ctx context.Context, who *model.UserInfo, address, name, hue string) (*SubscriptionView, error)
	RefreshSubscription(ctx context.Context, who *model.UserInfo, calendarID string) (*SubscriptionView, error)
	Unsubscribe(ctx context.Context, who *model.UserInfo, calendarID string) error
	SyncStatus(ctx context.Context, who *model.UserInfo) (*SyncStatus, error)
}

// DontSendIf is ctx for a write whose writer answered "Tell guests?" with
// notify: "none" is Don't send (D-CAL-5), and guests are told in their inbox
// alone; anything else sends.
func DontSendIf(ctx context.Context, notify string) context.Context {
	return withoutSending(ctx, notify)
}

// Answer is a guest's answer to an invitation, named as iCalendar's PARTSTAT.
type Answer string

const (
	Accepted  Answer = "accepted"
	Declined  Answer = "declined"
	Tentative Answer = "tentative"
)

// ErrUnavailable is something the calendar can't do yet.
var ErrUnavailable = errors.New("calendar: that isn't available yet")

// Events is the event service everything asks.
var Events Service = stored

// StatusKind is what someone's calendar says of them at a moment.
type StatusKind string

const (
	StatusAway  StatusKind = "away"  // time off
	StatusFocus StatusKind = "focus" // focus time
	StatusBusy  StatusKind = "busy"  // in an event: A3 answers it
)

// Status is what someone's calendar says of them at a moment, and until when.
// Never what an event is called.
type Status struct {
	Kind  StatusKind `json:"kind,omitempty"`
	Until *time.Time `json:"until,omitempty"`
}

// StatusReader is the calendar's answer to chat's presence (chat PROGRAM.md
// A7, calendar PROGRAM.md §1 #13).
type StatusReader interface {
	// StatusFor is what personUUID's calendar says of them at at: an empty
	// Status when it says nothing.
	StatusFor(ctx context.Context, personUUID string, at time.Time) (Status, error)
}

// Statuses is the status everything asks.
var Statuses StatusReader = todaysStatuses{}

// todaysStatuses is the person's events running at the moment: time off, and
// focus time, read as the busy engine reads them (eventsOf): from the one
// store when it is on, else from the graph, where away lives alone and focus
// as in Postgres.
type todaysStatuses struct{}

func (todaysStatuses) StatusFor(ctx context.Context, personUUID string, at time.Time) (Status, error) {
	events, err := eventsOf(ctx, personUUID, &at, &at)
	if err != nil {
		return Status{}, err
	}
	// Away and focus first (their own events, as the push gate counts them),
	// then a meeting they're in (calendar A3).
	return meetingStatus(ctx, personUUID, at, statusAt(events, personUUID, at))
}

// statusAt is what events say of personUUID at at, as the push gate counts
// a running event (started, not yet ended) of theirs: time off before focus
// time, each until the last of its kind running then ends. Only the events
// they made count, as for the push gate (FocusStore's Running) and the
// workload: the events read include those they're invited to, and a guest of
// a teammate's holiday isn't away. A deleted event says nothing. Pure.
func statusAt(events []*dgraphStruct.DgraphEvent, personUUID string, at time.Time) Status {
	var away, focus *time.Time
	later := func(was, end *time.Time) *time.Time {
		if was == nil || end.After(*was) {
			return end
		}
		return was
	}
	for _, e := range events {
		if e == nil || e.StartTime == nil || e.EndTime == nil || e.StartTime.After(at) || !e.EndTime.After(at) {
			continue
		}
		if e.DeletedAt != nil && e.DeletedAt.Year() > 1970 {
			continue
		}
		if e.CreatedBy == nil || !strings.EqualFold(e.CreatedBy.Uuid, personUUID) {
			continue
		}
		switch {
		case e.IsAway != nil && *e.IsAway:
			away = later(away, e.EndTime)
		case e.IsFocus != nil && *e.IsFocus:
			focus = later(focus, e.EndTime)
		}
	}
	switch {
	case away != nil:
		return Status{Kind: StatusAway, Until: away}
	case focus != nil:
		return Status{Kind: StatusFocus, Until: focus}
	}
	return Status{}
}
