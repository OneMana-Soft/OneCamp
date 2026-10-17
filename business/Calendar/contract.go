package Calendar

// The calendar programme's contracts in this package
// (design/calendar-vs-gcal/PROGRAM.md §4.2 #38 and #41, §5): the event service
// A1 builds, and the status chat reads (StatusFor, which A3 builds and chat A7
// shows as "In focus until 15:00", "Away until Monday").
//
// DAY 0. Both answer with today's code. Events is the functions behind the
// /event routes (GetEvent, GetEventsList, CreateEvent, UpdateEvent,
// DeleteEvent, and LeaveEvent for a guest who declines), with their rules and
// errors. Statuses reads the person's events at a moment as the busy engine
// reads them (eventsOf) and says away or focus, and until when. Nothing calls
// either yet, so nothing changes by their existing. A1 replaces Events with the
// Postgres service (series, zones, sequences) behind the same name, and A3 adds
// "busy" from the availability engine behind Statuses.

import (
	"context"
	"errors"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Service is every read and write of an event, for the person asking; the
// rules (who may see, change or answer an event) are its own, and so are the
// errors (ErrEventNotFound, ErrOnlyCreatorChanges, ErrNotEventCreator,
// ErrCreatorCannotLeave, *EventInputError).
type Service interface {
	// Get is one event: a OneCamp event's uuid, or "gcal-<id>" for one in
	// the person's own Google calendar.
	Get(ctx context.Context, who *model.UserInfo, id string) (*dgraphStruct.DgraphEvent, error)
	// Range is the person's events between from and to, with their linked
	// Google calendar's.
	Range(ctx context.Context, who *model.UserInfo, from, to time.Time) ([]*dgraphStruct.DgraphEvent, error)
	Create(ctx context.Context, who *model.UserInfo, in adapter.CreateOrUpdateEventInput) (*adapter.OutputEvent, error)
	Update(ctx context.Context, who *model.UserInfo, id uuid.UUID, in adapter.CreateOrUpdateEventInput) error
	Delete(ctx context.Context, who *model.UserInfo, id uuid.UUID) error
	// Respond is a guest's answer to an event they are on.
	Respond(ctx context.Context, who *model.UserInfo, id uuid.UUID, answer Answer) error
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
var Events Service = todaysEvents{}

// todaysEvents is the /event routes' functions.
type todaysEvents struct{}

func (todaysEvents) Get(ctx context.Context, who *model.UserInfo, id string) (*dgraphStruct.DgraphEvent, error) {
	return GetEvent(ctx, who, id)
}

func (todaysEvents) Range(ctx context.Context, who *model.UserInfo, from, to time.Time) ([]*dgraphStruct.DgraphEvent, error) {
	return GetEventsList(ctx, who, &from, &to)
}

func (todaysEvents) Create(ctx context.Context, who *model.UserInfo, in adapter.CreateOrUpdateEventInput) (*adapter.OutputEvent, error) {
	return CreateEvent(ctx, who, in)
}

func (todaysEvents) Update(ctx context.Context, who *model.UserInfo, id uuid.UUID, in adapter.CreateOrUpdateEventInput) error {
	return UpdateEvent(ctx, id, in, who)
}

func (todaysEvents) Delete(ctx context.Context, who *model.UserInfo, id uuid.UUID) error {
	return DeleteEvent(ctx, id, who)
}

// Respond: a guest's one answer today is leaving the event (POST
// /event/leaveEvent); a yes or a maybe waits for A2's attendance.
func (todaysEvents) Respond(ctx context.Context, who *model.UserInfo, id uuid.UUID, answer Answer) error {
	if answer != Declined {
		return ErrUnavailable
	}
	return LeaveEvent(ctx, id, who)
}

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
// focus time. Both stores keep focus alike (CreateEvent and UpdateEvent write
// it to each); away lives only in the graph, which is read here.
type todaysStatuses struct{}

func (todaysStatuses) StatusFor(ctx context.Context, personUUID string, at time.Time) (Status, error) {
	events, err := eventsOf(ctx, personUUID, &at, &at)
	if err != nil {
		return Status{}, err
	}
	return statusAt(events, personUUID, at), nil
}

// statusAt is what events say of personUUID at at, as the push gate counts
// a running event (started, not yet ended) of theirs: time off before focus
// time, each until the last of its kind running then ends. Only the events
// they made count, as for the push gate (FocusRunning) and the workload: the
// events read include those they're invited to, and a guest of a teammate's
// holiday isn't away. A deleted event says nothing. Pure.
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
