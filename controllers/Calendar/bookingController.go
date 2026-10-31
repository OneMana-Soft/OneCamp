package Calendar

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	availabilityBusiness "github.com/akashc777/OneCamp/business/Availability"
	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/controllers/httpx"
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
// that says so, a clash (an address another page uses) with a 409 that names
// it, and anything else with a retry.
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
		helpers.WriteRetryLater(w, "Couldn't check the calendar.")
	default:
		helpers.WriteWriteFailure(w, r, err, "Something went wrong.", "controllers/Calendar/"+where)
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

// DeleteMyBookingPage deletes one of the person's booking pages; bookings
// already made stay on the calendar, and their guests' links keep working. It
// answers how many are still to come. POST /event/bookingPages/delete {id}
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
	ok, upcoming, err := business.DeleteBookingPage(r.Context(), userInfo.UserPostgresInfo.Id, id)
	if err != nil {
		writeAvailabilityError(w, r, "DeleteMyBookingPage", err)
		return
	}
	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That isn't one of your pages."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted", "data": map[string]int{"upcoming_bookings": upcoming}})
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

// SuggestTimes ranks times for the signed-in person and the people listed,
// each in their own working hours and zone, with the reason each is offered
// (calendar A3). POST /event/suggestTimes
func SuggestTimes(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	var in business.FindTimeInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	res, err := availabilityBusiness.Suggestions(r.Context(), userInfo.UserDgraphInfo.Uuid, in, time.Now())
	var refusal *availabilityBusiness.Refusal
	if errors.As(err, &refusal) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": refusal.Msg})
		return
	}
	if err != nil {
		writeAvailabilityError(w, r, "SuggestTimes", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// PeopleHours answers the working hours, in their zone, of each of the people
// asked about who keeps some (calendar A3): for the week's drawn hours and
// the event form's "outside their hours". Only those the asker may see when
// they're busy are answered; the rest are named private. Given a day, it also
// answers where each works that day, by their working location (head 5d).
// POST /event/hours {"people": [...], "day": "2026-10-29"}
func PeopleHours(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	var in struct {
		People []string `json:"people"`
		Day    string   `json:"day"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil || len(in.People) > 50 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Ask about up to 50 people."})
		return
	}
	ids := make([]uuid.UUID, 0, len(in.People))
	for _, p := range in.People {
		id, err := uuid.Parse(p)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "One of those people isn't in this workspace."})
			return
		}
		ids = append(ids, id)
	}
	hours, private, err := business.HoursSeenBy(r.Context(), userInfo.UserDgraphInfo.Uuid, ids)
	if err != nil {
		writeAvailabilityError(w, r, "PeopleHours", err)
		return
	}
	answer := helpers.Envolope{"data": hours, "private": private}
	if in.Day != "" {
		day, err := time.Parse("2006-01-02", in.Day)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a day."})
			return
		}
		where, err := business.WhereSeenBy(r.Context(), userInfo.UserDgraphInfo.Uuid, ids, day)
		if err != nil {
			writeAvailabilityError(w, r, "PeopleHours", err)
			return
		}
		answer["where"] = where
	}
	helpers.WriteJSON(w, http.StatusOK, answer)
}

// ExplainTime says why a time isn't free for someone, in words, never a
// title (calendar A3), when the asker may see when they're busy.
// POST /event/explain {"person", "start", "end"}
func ExplainTime(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	var in struct {
		Person string    `json:"person"`
		Start  time.Time `json:"start"`
		End    time.Time `json:"end"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil || !in.End.After(in.Start) || in.End.Sub(in.Start) > 24*time.Hour {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Pick a time that ends after it starts, within a day."})
		return
	}
	words, err := availabilityBusiness.Times.Explain(r.Context(), userInfo.UserDgraphInfo.Uuid, in.Person, business.Interval{Start: in.Start, End: in.End})
	var refusal *availabilityBusiness.Refusal
	if errors.As(err, &refusal) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": refusal.Msg})
		return
	}
	if errors.Is(err, availabilityBusiness.ErrPrivate) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Your workspace keeps when people are busy private."})
		return
	}
	if err != nil {
		writeAvailabilityError(w, r, "ExplainTime", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"words": words}})
}

// PersonStatus is what someone's calendar says of them now: away, in focus
// time or in a meeting, until when, in words on the reader's clock and in
// their zone; never a title (calendar A3). Someone the asker may not see
// when they're busy says nothing, and is marked private.
// GET /event/status/{user_uuid}
func PersonStatus(w http.ResponseWriter, r *http.Request) {
	userInfo, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	person := chi.URLParam(r, "user_uuid")
	if _, err := uuid.Parse(person); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That person isn't in this workspace."})
		return
	}
	now := time.Now()
	st, private, err := business.StatusSeenBy(r.Context(), userInfo.UserDgraphInfo.Uuid, person, now)
	if err != nil {
		writeAvailabilityError(w, r, "PersonStatus", err)
		return
	}
	loc, twelve := business.ReaderClock(r.Context(), userInfo.UserPostgresInfo.Id)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"kind": st.Kind, "until": st.Until, "words": business.StatusWords(st, now, loc, twelve), "private": private}})
}
