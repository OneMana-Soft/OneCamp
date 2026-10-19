package router

import eventController "github.com/akashc777/OneCamp/controllers/Calendar"

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
	rt.Member.Get("/calendar/events/{eventId}", eventController.GetCalendarEvent)
	rt.Member.Patch("/calendar/events/{eventId}", eventController.UpdateCalendarEvent)
	rt.Member.Delete("/calendar/events/{eventId}", eventController.DeleteCalendarEvent)
	rt.Member.Post("/calendar/events/{eventId}/restore", eventController.RestoreCalendarEvent)
}
