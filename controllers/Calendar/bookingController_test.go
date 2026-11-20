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

// The largest booking validation admits (a 1,000-character note and ten
// 1,000-character answers, in 4-byte characters) is read whole: a guest is
// refused by a sentence, never by the body's size.
func TestABookingsLargestBodyIsRead(t *testing.T) {
	long := strings.Repeat("\U00020000", 1000)
	answers := make([]string, 10)
	for i := range answers {
		answers[i] = fmt.Sprintf(`{"question_id":"q%d","answer":%q}`, i, long)
	}
	body := fmt.Sprintf(`{"start":"2026-12-01T10:00:00Z","name":"Alex Kim","email":"alex@example.org","tz":"UTC","note":%q,"answers":[%s]}`,
		long, strings.Join(answers, ","))
	if len(body) < 40<<10 || len(body) > BookBodyLimit {
		t.Fatalf("a body of %d bytes, want 40 KiB to the limit", len(body))
	}
	in, err := decodeBook(strings.NewReader(body))
	if err != nil || len(in.Answers) != 10 || in.Note != long {
		t.Fatalf("a %d-byte booking wasn't read whole: %v", len(body), err)
	}
	if _, err := business.CheckGuest(in.BookInput); err != nil {
		t.Errorf("the largest booking isn't valid: %v", err)
	}
	if _, err := decodeBook(strings.NewReader(`{"note":"` + strings.Repeat("x", BookBodyLimit) + `"}`)); err == nil {
		t.Error("a body over the limit was read")
	}
}
