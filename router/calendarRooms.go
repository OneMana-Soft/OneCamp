package router

import eventController "github.com/akashc777/OneCamp/controllers/Calendar"

// mountCalendarRooms mounts rooms (gcal-6 head 1): admins keep the directory
// behind the one admin gate (API tokens never pass it); members list rooms,
// find free ones and read a room's bookings.
func mountCalendarRooms(rt calendarRouters) {
	rt.Admin.Post("/calendar/rooms", eventController.AddRoom)
	rt.Admin.Patch("/calendar/rooms/{roomId}", eventController.ChangeRoom)
	rt.Admin.Post("/calendar/rooms/{roomId}/retire", eventController.RetireRoom)
	rt.Member.Get("/calendar/rooms", eventController.ListRooms)
	rt.Member.Get("/calendar/rooms/free", eventController.FreeRooms)
	rt.Member.Get("/calendar/rooms/{roomId}/bookings", eventController.RoomCalendar)
}
