package Calendar

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// writeAvailabilityError answers a request the person can fix with its text,
// a missing page with 404, a calendar that couldn't be checked with a retry
// that says so, and anything else with a retry.
func writeAvailabilityError(w http.ResponseWriter, r *http.Request, where string, err error) {
	var ae *business.AvailabilityError
	var busy *business.PageBusyError
	switch {
	case errors.As(err, &busy):
		customMiddleware.WriteRateLimited(w, busy.Limit, registry.PublicReadRate, "This page is very busy right now. Wait a minute and try again.")
	case errors.As(err, &ae):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": ae.Error()})
	case errors.Is(err, business.ErrNoPage):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "This booking page isn't available."})
	case errors.Is(err, business.ErrCalendarUnread):
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't check the calendar. Try again in a moment."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Calendar/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Something went wrong. Try again in a moment."})
	}
}

func timeParam(r *http.Request, key string, fallback time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, r.URL.Query().Get(key)); err == nil {
		return t
	}
	return fallback
}

// FindTime suggests times when the signed-in person and the people listed
// are all free. POST /event/findTime
func FindTime(w http.ResponseWriter, r *http.Request) {
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(model.UserInfo)
	var in business.FindTimeInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	res, err := business.FindTime(r.Context(), userInfo.UserDgraphInfo.Uuid, in, time.Now())
	if err != nil {
		writeAvailabilityError(w, r, "FindTime", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// GetMyBookingPages lists the signed-in person's booking pages.
// GET /event/bookingPages
func GetMyBookingPages(w http.ResponseWriter, r *http.Request) {
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(model.UserInfo)
	pages, err := bookingModel.ListPages(userInfo.UserPostgresInfo.Id)
	if err != nil {
		writeAvailabilityError(w, r, "GetMyBookingPages", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": pages})
}

// SaveMyBookingPage creates or updates one of the person's booking pages.
// POST /event/bookingPages
func SaveMyBookingPage(w http.ResponseWriter, r *http.Request) {
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(model.UserInfo)
	var in business.PageInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	page, err := business.SaveBookingPage(r.Context(), userInfo.UserPostgresInfo.Id, in)
	if err != nil {
		writeAvailabilityError(w, r, "SaveMyBookingPage", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": page})
}

// DeleteMyBookingPage deletes one of the person's booking pages. While
// bookings made through it are still to come it answers 409 and keeps it:
// switching it off keeps them and their guests' links.
// POST /event/bookingPages/delete {id}
func DeleteMyBookingPage(w http.ResponseWriter, r *http.Request) {
	userInfo := r.Context().Value(helpers.UserInfoContextKey).(model.UserInfo)
	var in struct {
		Id string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	id, err := uuid.Parse(in.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't one of your pages."})
		return
	}
	ok, err := business.DeleteBookingPage(r.Context(), userInfo.UserPostgresInfo.Id, id)
	var held *business.PageHasBookingsError
	if errors.As(err, &held) {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": held.Error()})
		return
	}
	if err != nil {
		writeAvailabilityError(w, r, "DeleteMyBookingPage", err)
		return
	}
	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That isn't one of your pages."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted", "data": map[string]int{"upcoming_bookings": 0}})
}

// GetPublicBookingPage is a booking page and its free slots, for anyone: from
// the start of the day `from` falls on, in the owner's zone, for 31 days or to
// the page's horizon (`to` is no longer read). Limited per address and per
// page (business, when an answer is worked out afresh), and kept a minute per
// page and day (business).
// GET /public/book/{slug}?from=
func GetPublicBookingPage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	page, err := business.GetPublicPage(r.Context(), chi.URLParam(r, "slug"), timeParam(r, "from", now), now)
	if err != nil {
		writeAvailabilityError(w, r, "GetPublicBookingPage", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": page})
}

type publicBookInput struct {
	business.BookInput
	// Website is a field people never see: a form that fills it is a bot.
	Website string `json:"website"`
}

// BookPublicSlot books a slot for a visitor.
// POST /public/book/{slug} {start, name, email, note, tz}
func BookPublicSlot(w http.ResponseWriter, r *http.Request) {
	var in publicBookInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	if in.Website != "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't book that time."})
		return
	}
	booked, err := business.Book(r.Context(), chi.URLParam(r, "slug"), in.BookInput, time.Now())
	if err != nil {
		writeAvailabilityError(w, r, "BookPublicSlot", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": booked})
}

// GetPublicBooking is the booking behind a guest's cancel link.
// GET /public/booking/{token}
func GetPublicBooking(w http.ResponseWriter, r *http.Request) {
	view, err := business.GetBooking(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		writeAvailabilityError(w, r, "GetPublicBooking", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": view})
}

// CancelPublicBooking cancels through a guest's link.
// POST /public/booking/{token}/cancel
func CancelPublicBooking(w http.ResponseWriter, r *http.Request) {
	view, err := business.CancelBooking(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		writeAvailabilityError(w, r, "CancelPublicBooking", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": view})
}
