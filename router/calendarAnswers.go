package router

import eventController "github.com/akashc777/OneCamp/controllers/Calendar"

// mountCalendarAnswers is calendar A2's member routes: a guest answers an
// event (proposing another time, if they like), whoever may change it
// dismisses a proposal, and its organiser asks again those who haven't
// answered or copies the answer link of a guest invited by email. A guest's
// own page by that link is public, so its two routes are router.go's
// (publicSurface_test.go), each with its own rate limit.
func mountCalendarAnswers(rt calendarRouters) {
	rt.Member.Post("/calendar/events/{eventId}/answer", eventController.AnswerCalendarEvent)
	rt.Member.Post("/calendar/events/{eventId}/proposal/dismiss", eventController.DismissCalendarProposal)
	rt.Member.Post("/calendar/events/{eventId}/nudge", eventController.NudgeCalendarGuests)
	rt.Member.Post("/calendar/events/{eventId}/guest-link", eventController.CalendarGuestLink)
	// The event's own call (calendar B11): given and taken off by whoever may change the event.
	rt.Member.Post("/calendar/events/{eventId}/call", eventController.AddCalendarEventCall)
	rt.Member.Delete("/calendar/events/{eventId}/call", eventController.RemoveCalendarEventCall)
}
