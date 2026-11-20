package router

import (
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
)

// mountCalendarFeeds is a workspace admin's view of the calendar's secret
// links (basecamp-05): every live one, never its address, and any one
// stopped by its id. A member's own links, of a calendar or of everything
// they may see ("workspace"), stay on /calendar/calendars/{calendarId}/feed
// (calendarEvents.go).
func mountCalendarFeeds(rt calendarRouters) {
	rt.Admin.Get("/calendar/feeds", eventController.CalendarFeeds)
	rt.Admin.Delete("/calendar/feeds/{feedId}", eventController.StopAnyCalendarFeed)
}

// mountCalendarSyncLedger is a workspace admin's ledger of every Google and
// Outlook sync link (gcal-8 H1): link metadata and counts, a page at a time,
// and a break of any one by its id. A person's own link stays on
// /calendar/sync/{provider} (calendarEvents.go).
func mountCalendarSyncLedger(rt calendarRouters) {
	rt.Admin.Get("/calendar/sync", eventController.CalendarSyncLedger)
	rt.Admin.Post("/calendar/sync/break", eventController.BreakCalendarSyncLink)
}
