package Calendar

// Rooms (gcal-6 head 1): the admin directory (/admin/calendar/rooms, behind
// the one admin gate) and members' reads (/calendar/rooms). The permission
// model is business/Calendar/rooms.go's.

import (
	"net/http"
	"strconv"
	"time"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// span reads from and to (RFC 3339) from the query, both required.
func span(w http.ResponseWriter, r *http.Request) (time.Time, time.Time, bool) {
	from, fok := queryTime(r, "from")
	to, tok := queryTime(r, "to")
	if !fok || !tok || from == nil || to == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say from and to as RFC 3339 times."})
		return time.Time{}, time.Time{}, false
	}
	return *from, *to, true
}

// AddRoom is POST /admin/calendar/rooms.
func AddRoom(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	var in business.RoomInput
	if !ok || !decodeBody(w, r, &in, "room") {
		return
	}
	room, err := business.AddRoom(r.Context(), who, in)
	if err != nil {
		writeCalendarError(w, r, "AddRoom", err)
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": room})
}

// ChangeRoom is PATCH /admin/calendar/rooms/{roomId}.
func ChangeRoom(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	var in business.RoomInput
	if !ok || !decodeBody(w, r, &in, "room") {
		return
	}
	room, err := business.ChangeRoom(r.Context(), who, chi.URLParam(r, "roomId"), in)
	if err != nil {
		writeCalendarError(w, r, "ChangeRoom", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": room})
}

// RetireRoom is POST /admin/calendar/rooms/{roomId}/retire.
func RetireRoom(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	room, err := business.RetireRoom(r.Context(), who, chi.URLParam(r, "roomId"))
	if err != nil {
		writeCalendarError(w, r, "RetireRoom", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": room})
}

// ListRooms is GET /calendar/rooms.
func ListRooms(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	rooms, err := business.ListRooms(r.Context(), who)
	if err != nil {
		writeCalendarError(w, r, "ListRooms", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rooms})
}

// FreeRooms is GET /calendar/rooms/free?from&to&capacity.
func FreeRooms(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	from, to, ok := span(w, r)
	if !ok {
		return
	}
	capacity, _ := strconv.Atoi(r.URL.Query().Get("capacity"))
	rooms, err := business.FreeRooms(r.Context(), who, from, to, capacity)
	if err != nil {
		writeCalendarError(w, r, "FreeRooms", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": rooms})
}

// RoomCalendar is GET /calendar/rooms/{roomId}/bookings?from&to.
func RoomCalendar(w http.ResponseWriter, r *http.Request) {
	who, ok := member(w, r)
	if !ok {
		return
	}
	from, to, ok := span(w, r)
	if !ok {
		return
	}
	room, bookings, err := business.RoomCalendar(r.Context(), who, chi.URLParam(r, "roomId"), from, to)
	if err != nil {
		writeCalendarError(w, r, "RoomCalendar", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"room": room, "bookings": bookings}})
}
