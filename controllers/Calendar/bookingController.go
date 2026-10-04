package Calendar

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// writeAvailabilityError answers a request the person can fix with its text,
// a missing page with 404, and anything else with a retry.
func writeAvailabilityError(w http.ResponseWriter, r *http.Request, where string, err error) {
	var ae *business.AvailabilityError
	switch {
	case errors.As(err, &ae):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": ae.Error()})
	case errors.Is(err, business.ErrNoPage):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "This booking page isn't available."})
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
	publicCache.forget(page.Slug)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": page})
}

// DeleteMyBookingPage deletes one of the person's booking pages; bookings
// already made stay on the calendar. POST /event/bookingPages/delete {id}
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
	ok, err := bookingModel.DeletePage(userInfo.UserPostgresInfo.Id, id)
	if err != nil {
		writeAvailabilityError(w, r, "DeleteMyBookingPage", err)
		return
	}
	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That isn't one of your pages."})
		return
	}
	publicCache.clear()
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted"})
}

// publicCache keeps a page's slots for a minute. Every public look reads the
// owner's calendars (Google's too), and a visitor flicking between weeks, or
// someone hammering the page, shouldn't cost a Google call each time. A
// booking or an edit to the page drops its entries.
var publicCache = &slotCache{entries: map[string]slotEntry{}}

type slotEntry struct {
	slug    string
	page    *business.PublicPage
	expires time.Time
}

type slotCache struct {
	mu      sync.Mutex
	entries map[string]slotEntry
}

func (c *slotCache) get(key string, now time.Time) *business.PublicPage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && now.Before(e.expires) {
		return e.page
	}
	return nil
}

func (c *slotCache) put(key, slug string, p *business.PublicPage, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) > 2000 {
		c.entries = map[string]slotEntry{}
	}
	c.entries[key] = slotEntry{slug: slug, page: p, expires: now.Add(time.Minute)}
}

func (c *slotCache) forget(slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if e.slug == slug {
			delete(c.entries, k)
		}
	}
}

func (c *slotCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]slotEntry{}
}

// GetPublicBookingPage is a booking page and its free slots, for anyone.
// GET /public/book/{slug}?from=&to=
func GetPublicBookingPage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	slug := business.NormaliseSlug(chi.URLParam(r, "slug"))
	from := timeParam(r, "from", now).Truncate(time.Hour)
	to := timeParam(r, "to", from.Add(14*24*time.Hour)).Truncate(time.Hour)
	key := slug + "|" + from.Format(time.RFC3339) + "|" + to.Format(time.RFC3339)
	if p := publicCache.get(key, now); p != nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": p})
		return
	}
	page, err := business.GetPublicPage(r.Context(), slug, from, to, now)
	if err != nil {
		writeAvailabilityError(w, r, "GetPublicBookingPage", err)
		return
	}
	publicCache.put(key, slug, page, now)
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
	slug := business.NormaliseSlug(chi.URLParam(r, "slug"))
	booked, err := business.Book(r.Context(), slug, in.BookInput, time.Now())
	if err != nil {
		writeAvailabilityError(w, r, "BookPublicSlot", err)
		return
	}
	publicCache.forget(slug)
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
	publicCache.forget(view.Slug)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": view})
}
