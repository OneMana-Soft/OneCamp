package business

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	"github.com/google/uuid"
)

// busyKeptPrivate is a workspace that keeps when members are busy to
// themselves (calendar.freebusy.members off).
type busyKeptPrivate struct{ policyBusiness.Store }

func (s busyKeptPrivate) Bool(ctx context.Context, p policyBusiness.BoolPolicy) bool {
	if p.Key == policyBusiness.CalendarFreeBusy.Key {
		return false
	}
	return s.Store.Bool(ctx, p)
}

// The engine answers for an asker: in a workspace that keeps busy times
// private, asking about someone else reads nothing and names them private.
func TestTheEngineKeepsSomeoneElsesBusyTimesFromTheAsker(t *testing.T) {
	prev := policyBusiness.Policies
	t.Cleanup(func() { policyBusiness.Policies = prev })
	policyBusiness.Policies = busyKeptPrivate{prev}
	me, maya := uuid.NewString(), uuid.NewString()
	busy, private, err := Times.Busy(context.Background(), me, []string{maya}, time.Now(), time.Now().Add(time.Hour))
	if err != nil || len(busy) != 0 || !reflect.DeepEqual(private, []string{maya}) {
		t.Errorf("busy %v, private %v, err %v: want nothing read and Maya private", busy, private, err)
	}
}

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

// Nobody is busy when nobody is asked about; someone who isn't a person can't be explained.
func TestWhatTheEngineAnswersWithoutReading(t *testing.T) {
	got, private, err := Times.Busy(context.Background(), "", nil, time.Now(), time.Now().Add(time.Hour))
	if err != nil || len(got) != 0 || len(private) != 0 {
		t.Errorf("nobody: %v %v %v", got, private, err)
	}
	var refusal *Refusal
	if _, err := Times.Explain(context.Background(), "", "p", calendarBusiness.Interval{}); !errors.As(err, &refusal) {
		t.Errorf("explain: %v", err)
	}
}

// Calendar A3: why a slot isn't free, in words, never a title.
func TestExplainSaysWhyASlotIsntFree(t *testing.T) {
	at := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	slot := calendarBusiness.Interval{Start: at, End: at.Add(30 * time.Minute)}
	span := func(kind string) calendarBusiness.BusySpan {
		return calendarBusiness.BusySpan{Interval: calendarBusiness.Interval{Start: at.Add(-time.Hour), End: at.Add(time.Hour)}, Kind: kind}
	}
	nine := week("UTC", calendarBusiness.Span{Start: "09:00", End: "17:00"})
	late := week("UTC", calendarBusiness.Span{Start: "13:00", End: "17:00"})
	for _, c := range []struct {
		b      calendarBusiness.Busy
		h      *calendarBusiness.WorkingHours
		unread bool
		want   string
	}{
		{calendarBusiness.Busy{Spans: []calendarBusiness.BusySpan{span(calendarBusiness.BusyMeeting), span(calendarBusiness.BusyAway)}}, nine, false, "Away then"},
		{calendarBusiness.Busy{Spans: []calendarBusiness.BusySpan{span(calendarBusiness.BusyFocus)}}, nine, false, "In focus time then"},
		{calendarBusiness.Busy{Spans: []calendarBusiness.BusySpan{span(calendarBusiness.BusyGoogle)}}, nil, false, "In a meeting then"},
		{calendarBusiness.Busy{}, late, false, "Outside their working hours"},
		{calendarBusiness.Busy{}, nine, false, "Free then"},
		{calendarBusiness.Busy{}, nil, true, "Free in OneCamp; their Google Calendar couldn't be read"},
	} {
		if got := explain(c.b, c.h, slot, c.unread); got != c.want {
			t.Errorf("%q, want %q", got, c.want)
		}
	}
}

// Explain answers for an asker too: in a workspace that keeps busy times
// private, why someone else isn't free is theirs, and nothing is read.
func TestExplainKeepsSomeoneElsesDayFromTheAsker(t *testing.T) {
	prev, oldB, oldH := policyBusiness.Policies, busyFor, hoursOf
	t.Cleanup(func() { policyBusiness.Policies, busyFor, hoursOf = prev, oldB, oldH })
	policyBusiness.Policies = busyKeptPrivate{prev}
	busyFor = func(context.Context, string, time.Time, time.Time) (calendarBusiness.Busy, error) {
		t.Error("read a calendar the asker may not see")
		return calendarBusiness.Busy{}, nil
	}
	hoursOf = func(context.Context, []uuid.UUID) (map[uuid.UUID]calendarBusiness.WorkingHours, error) {
		t.Error("read the hours of someone the asker may not see")
		return nil, nil
	}
	me, maya := uuid.NewString(), uuid.NewString()
	at := time.Now()
	words, err := Times.Explain(context.Background(), me, maya, calendarBusiness.Interval{Start: at, End: at.Add(time.Hour)})
	if !errors.Is(err, ErrPrivate) || words != "" {
		t.Errorf("%q, %v: want ErrPrivate and no words", words, err)
	}
}
