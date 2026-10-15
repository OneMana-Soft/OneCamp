package Calendar

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	business "github.com/akashc777/OneCamp/business/Calendar"
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
