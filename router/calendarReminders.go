package router

import eventController "github.com/akashc777/OneCamp/controllers/Calendar"

// mountCalendarReminders is calendar A5's member routes: a person's own
// reminders for an event or series, and a reminder snoozed.
func mountCalendarReminders(rt calendarRouters) {
	rt.Member.Get("/calendar/events/{eventId}/reminders", eventController.GetCalendarReminders)
	rt.Member.Put("/calendar/events/{eventId}/reminders", eventController.SetCalendarReminders)
	rt.Member.Post("/calendar/events/{eventId}/snooze", eventController.SnoozeCalendarReminder)
}
