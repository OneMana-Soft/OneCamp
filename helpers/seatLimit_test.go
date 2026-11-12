package helpers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSeatLimit(t *testing.T) {
	for in, want := range map[string]int{"": 0, "25": 25, " 10 ": 10, "0": 0, "-3": 0, "x": 0} {
		if got := parseSeatLimit(in); got != want {
			t.Errorf("parseSeatLimit(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestSeatLimitError(t *testing.T) {
	err := fmt.Errorf("creating user: %w", &SeatLimitError{Limit: 25})
	if !IsSeatLimit(err) {
		t.Fatal("a wrapped seat limit must still be recognised")
	}
	if IsSeatLimit(fmt.Errorf("other")) {
		t.Fatal("other errors are not seat limits")
	}
	msg := (&SeatLimitError{Limit: 25}).Error()
	for _, want := range []string{"25 people", "deactivate", "onemana.dev/pricing"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

// The refusal carries the limit as a number, so a member who meets it is
// told the plan's own figure ("Ask an admin to remove the 25-person limit")
// without reading the seats, which are an admin's.
func TestSeatLimitAnswerCarriesTheLimit(t *testing.T) {
	w := httptest.NewRecorder()
	if !WriteSeatLimit(w, fmt.Errorf("joining: %w", &SeatLimitError{Limit: 25})) {
		t.Fatal("a seat limit must be answered")
	}
	var body struct {
		Code  string `json:"code"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusForbidden || body.Code != "seat_limit" || body.Limit != 25 {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
}

// Someone whose own way in met the limit, once the admins are told, hears
// that, and nothing of licences or prices (ADMIN-02).
func TestSeatLimitErrorOnceTheAdminsAreTold(t *testing.T) {
	msg := (&SeatLimitError{Limit: 25, Told: true}).Error()
	if msg != "This workspace is on OneCamp's free plan, which has room for 25 people, and it's full. "+
		"Your admin has been told; try again once they've freed a place or removed the limit." {
		t.Fatalf("told: %q", msg)
	}
	for _, never := range []string{"onemana.dev", "licence", "pricing"} {
		if strings.Contains(msg, never) {
			t.Errorf("the joiner is offered %q: %q", never, msg)
		}
	}
}
