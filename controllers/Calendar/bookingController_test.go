package Calendar

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
)

// S1: a booking refused because the calendar couldn't be checked says so, as
// a retry, not as the visitor's mistake or a vague failure.
func TestABookingTheCalendarCouldntCheckAsksForARetry(t *testing.T) {
	w := httptest.NewRecorder()
	writeAvailabilityError(w, httptest.NewRequest(http.MethodPost, "/booking/intro-call/book", nil), "BookPublicSlot", fmt.Errorf("book: %w", business.ErrCalendarUnread))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Couldn't check the calendar. Try again in a moment.") {
		t.Fatalf("body %s", w.Body.String())
	}
}

// An address another page already uses is a clash the person sorts out by
// picking another: a 409 that names the address. On 4 Oct a duplicate slug
// answered 503, and since then a bare 400 that didn't say which address.
func TestATakenBookingAddressIsAConflictThatNamesIt(t *testing.T) {
	w := httptest.NewRecorder()
	taken := helpers.Conflict(`Another booking page already uses the address "office-hours". Pick another.`, bookingModel.ErrSlugTaken)
	writeAvailabilityError(w, httptest.NewRequest(http.MethodPost, "/event/bookingPages", nil), "SaveMyBookingPage", fmt.Errorf("save: %w", taken))
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), `already uses the address \"office-hours\"`) {
		t.Fatalf("body %s doesn't name the address", w.Body.String())
	}
}
