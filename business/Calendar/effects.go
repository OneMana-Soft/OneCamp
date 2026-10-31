package Calendar

// What follows from a change to an event, once it is stored and announced: the
// same things the /event routes did after their two writes, now after the one
// (CAL1). Google's copy is written in the background, with a time limit, as it
// was; a booking follows its event and its guest is told (CAL-BUG-4). Who a
// change is for is told by the teller (notify.go, imip.go), guests taken off
// among them (CAL-BUG-1). An interface, so the service's tests record what
// would happen instead of calling Google.

import (
	"context"
	"errors"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

type effects interface {
	// created: a new event, with its guests' ids.
	created(ctx context.Context, who *model.UserInfo, e *models.Event, guests []string, syncToGoogle bool)
	// updated: before became after; guestsSent is whether the write named
	// the guest list, and removed is who it took off.
	updated(ctx context.Context, who *model.UserInfo, before, after *models.Event, guestsBefore, guestsAfter []models.Attendee, guestsSent bool, removed []string)
	// deleted: e went to the trash.
	deleted(ctx context.Context, who *model.UserInfo, e *models.Event, guests []models.Attendee)
	// left: who took themselves off e.
	left(ctx context.Context, who *model.UserInfo, e *models.Event)
}

// outboundTimeLimit bounds one call to Google.
const outboundTimeLimit = 30 * time.Second

// outbound is a context for work that outlives the request: its values, not
// its cancellation, and a time limit.
func outbound(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), outboundTimeLimit)
}

// legacyInput is an event as Google's writers take it: the /event routes'
// input. An all-day event goes as the midnights of its dates in its zone, as
// the web always sent it (whole-day Google events are B6's).
func legacyInput(e *models.Event, guests []string) adapter.CreateOrUpdateEventInput {
	start, end := spanOf(e)
	focus, away := e.Kind == string(EventFocus), e.Kind == string(EventAway)
	return adapter.CreateOrUpdateEventInput{Title: e.Title, Description: e.Notes,
		StartTime: start.Format(time.RFC3339), EndTime: end.Format(time.RFC3339),
		Participants: guests, IsFocus: &focus, IsAway: &away}
}

type liveEffects struct{ s *service }

func (l liveEffects) created(ctx context.Context, who *model.UserInfo, e *models.Event, guests []string, syncToGoogle bool) {
	if !syncToGoogle {
		return
	}
	in, user, id := legacyInput(e, guests), *who, e.ID
	in.Title, in.SyncToGoogleCalendar = l.s.titler(ctx, subjectOf(who), []*models.Event{e})(e), true
	helpers.GoSafeNamed("business/Calendar/googleCreate", func() {
		ctx, cancel := outbound(ctx)
		defer cancel()
		if err := integrationBusiness.SyncEventToGoogleCalendar(ctx, id, in, &user, true); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: event %s wasn't copied to Google: %v", id, err)
		}
	})
}

// updated follows a change: Google's copy, and the booking it may be. Guests
// taken off are told with everyone else a change is for (notify.go).
func (l liveEffects) updated(ctx context.Context, who *model.UserInfo, before, after *models.Event, guestsBefore, guestsAfter []models.Attendee, guestsSent bool, removed []string) {
	if gid := before.GoogleEventID; gid != "" {
		in, user, id := legacyInput(after, guestIDs(guestsAfter)), *who, after.ID
		in.Title = l.s.titler(ctx, subjectOf(who), []*models.Event{after})(after)
		helpers.GoSafeNamed("business/Calendar/googleUpdate", func() {
			ctx, cancel := outbound(ctx)
			defer cancel()
			err := integrationBusiness.UpdateGoogleCalendarEvent(ctx, gid, in, &user, guestsSent)
			var gone *integrationBusiness.GCalEventGoneError
			if errors.As(err, &gone) {
				// Deleted on Google: forget the copy, so later edits stop
				// failing against it.
				if err := l.s.store.setGoogleID(ctx, id, ""); err != nil {
					helpers.LogErrorWithContext(ctx, "business/Calendar: event %s's gone Google copy wasn't forgotten: %v", id, err)
				}
			}
		})
	}
	start, end := spanOf(after)
	bookingEventUpdated(ctx, l.s.legacyNamed(ctx, who, before, guestsBefore), start, end)
}

func (l liveEffects) deleted(ctx context.Context, who *model.UserInfo, e *models.Event, guests []models.Attendee) {
	if gid := e.GoogleEventID; gid != "" {
		user := *who
		helpers.GoSafeNamed("business/Calendar/googleDelete", func() {
			ctx, cancel := outbound(ctx)
			defer cancel()
			if err := integrationBusiness.DeleteGoogleCalendarEvent(ctx, gid, &user); err != nil {
				helpers.LogErrorWithContext(ctx, "business/Calendar: Google's copy of an event wasn't deleted: %v", err)
			}
		})
	}
	bookingEventDeleted(ctx, l.s.legacyNamed(ctx, who, e, guests))
}

func (l liveEffects) left(ctx context.Context, who *model.UserInfo, e *models.Event) {
	gid := e.GoogleEventID
	if gid == "" {
		return
	}
	leaver, organiser := who.UserPostgresInfo.EmailID, e.CreatedBy
	helpers.GoSafeNamed("business/Calendar/googleLeave", func() {
		ctx, cancel := outbound(ctx)
		defer cancel()
		// Google's copy is the organiser's: they take the guest off it.
		creator, err := userDomain.GetUserByUUID(ctx, organiser)
		if err != nil || creator == nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: a guest leaving wasn't sent to Google: the organiser couldn't be read: %v", err)
			return
		}
		if err := integrationBusiness.RemoveAttendeeFromGoogleCalendarEvent(ctx, gid, leaver, &model.UserInfo{UserPostgresInfo: *creator}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: a guest leaving wasn't sent to Google: %v", err)
		}
	})
}

// legacyNamed is one event in the /event routes' shape, everyone named, for
// the booking steps that speak of its host by name.
func (s *service) legacyNamed(ctx context.Context, reader *model.UserInfo, e *models.Event, guests []models.Attendee) *dgraphStruct.DgraphEvent {
	names := s.namesOf(ctx, []instance{single(e)}, map[uuid.UUID][]models.Attendee{e.ID: guests})
	return legacyOf(e, guests, names, personOf(reader), webDates)
}
