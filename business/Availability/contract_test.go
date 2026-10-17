package business

import (
	"context"
	"errors"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
)

// Suggest is FindTime: its rules answer, here before anything is read.
func TestSuggestIsFindTime(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	in := calendarBusiness.FindTimeInput{Participants: []string{"not a person"}, DurationMinutes: 30, From: now, To: now.Add(24 * time.Hour)}
	_, err := Times.Suggest(context.Background(), "11111111-1111-4111-8111-111111111111", in, now)
	var refused *calendarBusiness.AvailabilityError
	if !errors.As(err, &refused) || refused.Error() != "One of those people isn't in this workspace." {
		t.Errorf("FindTime's rule didn't answer: %v", err)
	}
}

// Nobody is busy when nobody is asked about, and nothing explains a time yet.
func TestWhatTheEngineDoesntReadYet(t *testing.T) {
	got, err := Times.Busy(context.Background(), nil, time.Now(), time.Now().Add(time.Hour))
	if err != nil || len(got) != 0 {
		t.Errorf("nobody: %v %v", got, err)
	}
	if _, err := Times.Explain(context.Background(), "p", calendarBusiness.Interval{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("explain: %v", err)
	}
}
