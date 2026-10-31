package router

import (
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// mountCalendarEvents is the calendar's own API over its one store (calendar
// programme A1): events by range, one by id, made, changed at a sequence, put
// in the trash and brought back. Members only. The /event routes stay as they
// are, answering from the same store when it is on (business/Calendar
// legacy.go), until the web moves onto these (A7) and the integrator deletes
// them at A7's wiring.
func mountCalendarEvents(rt calendarRouters) {
	rt.Member.Get("/calendar/events", eventController.ListCalendarEvents)
	rt.Member.Post("/calendar/events", eventController.CreateCalendarEvent)
	rt.Member.Get("/calendar/events/trash", eventController.CalendarTrash)
	rt.Member.Get("/calendar/events/export", eventController.ExportCalendar)
	// What repeats beside the events (head 9): check-ins and tasks ahead.
	rt.Member.Get("/calendar/repeats", eventController.ListCalendarRepeats)
	// Your week (B3): where the asker's own time went.
	rt.Member.Get("/calendar/insights", eventController.YourWeek)
	rt.Member.Get("/calendar/sync/google", eventController.GoogleSyncStatus)
	rt.Member.Post("/calendar/sync/google/move", eventController.MoveFromGoogle)
	rt.Member.Post("/calendar/projects/{projectId}/calendar", eventController.ProjectCalendar)
	rt.Member.Get("/calendar/calendars", eventController.ListCalendars)
	rt.Member.Get("/calendar/calendars/events", eventController.ListEventsOnCalendars)
	rt.Member.Get("/calendar/people/{userId}/calendar", eventController.PersonCalendar)
	rt.Member.Get("/calendar/calendars/{calendarId}/shares", eventController.CalendarShares)
	rt.Member.Patch("/calendar/calendars/{calendarId}", eventController.UpdateCalendar)
	rt.Member.Get("/calendar/calendars/{calendarId}/feed", eventController.CalendarFeed)
	rt.Member.Put("/calendar/calendars/{calendarId}/feed", eventController.MakeCalendarFeed)
	rt.Member.Delete("/calendar/calendars/{calendarId}/feed", eventController.StopCalendarFeed)
	rt.Member.Put("/calendar/calendars/{calendarId}/shares/{userId}", eventController.ShareCalendar)
	// Calendars read from an address (B8): adding one reads it, from the
	// server, so adding is limited per address asking.
	rt.Member.Get("/calendar/subscriptions", eventController.ListSubscriptions)
	rt.Member.With(customMiddleware.IPRateLimit("calendar-subscribe", 30, "Too many calendars added from here. Try again in a few minutes.")).
		Post("/calendar/subscriptions", eventController.Subscribe)
	rt.Member.Post("/calendar/subscriptions/{calendarId}/refresh", eventController.RefreshSubscription)
	rt.Member.Delete("/calendar/subscriptions/{calendarId}", eventController.Unsubscribe)
	rt.Member.Get("/calendar/events/{eventId}", eventController.GetCalendarEvent)
	rt.Member.Patch("/calendar/events/{eventId}", eventController.UpdateCalendarEvent)
	rt.Member.Delete("/calendar/events/{eventId}", eventController.DeleteCalendarEvent)
	rt.Member.Post("/calendar/events/{eventId}/restore", eventController.RestoreCalendarEvent)
}
