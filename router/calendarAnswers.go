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
	// The event's own call (calendar B11): given and taken off by whoever may change the event,
	// who also says whether its invitees knock (meet-02).
	rt.Member.Post("/calendar/events/{eventId}/call", eventController.AddCalendarEventCall)
	rt.Member.Delete("/calendar/events/{eventId}/call", eventController.RemoveCalendarEventCall)
	rt.Member.Post("/calendar/events/{eventId}/call/lobby", eventController.SetCalendarEventCallLobby)
	// Every event's call room (gcal-15 head B): read and joined by whoever may read its details.
	rt.Member.Get("/calendar/events/{eventId}/room", eventController.GetCalendarEventRoom)
	rt.Member.Post("/calendar/events/{eventId}/room/join", eventController.JoinCalendarEventRoom)
	rt.Member.Get("/calendar/events/{eventId}/call/artefacts", eventController.GetCalendarEventCallArtefacts)
	rt.Member.Get("/calendar/events/{eventId}/call/artefacts/{key}/recording", eventController.GetCalendarEventCallRecording)
	rt.Member.Get("/calendar/events/{eventId}/call/artefacts/{key}/transcript", eventController.GetCalendarEventCallTranscript)
	// What the event's call made is deleted, and brought back, by whoever may change the event.
	rt.Member.Delete("/calendar/events/{eventId}/call/artefacts/{key}", eventController.DeleteCalendarEventCallArtefact)
	rt.Member.Post("/calendar/events/{eventId}/call/artefacts/{key}/restore", eventController.RestoreCalendarEventCallArtefact)
}
