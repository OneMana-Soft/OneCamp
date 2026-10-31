package business

import (
	"errors"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/CheckIn"
	"github.com/google/uuid"
)

// A check-in on a calendar (calendar head 9): each time it asks in a
// stretch, as the sweeper asks it.
func TestAsksIn(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	weekdays := Schedule{Days: 31, AtMinute: 17 * 60, Loc: ny}
	from := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) // a Monday
	got := asksIn(weekdays, from, from.AddDate(0, 0, 14), 100)
	// Monday to Friday at 17:00 in New York, the second week after the clocks went back (1 Nov).
	want := []string{"26 21:00", "27 21:00", "28 21:00", "29 21:00", "30 21:00", "2 22:00", "3 22:00", "4 22:00", "5 22:00", "6 22:00"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i, at := range got {
		if s := at.UTC().Format("2 15:04"); s != want[i] {
			t.Fatalf("time %d is %s, want %s (%v)", i, s, want[i], got)
		}
	}
	// One at the start counts; the end is not in the stretch.
	first := time.Date(2026, 10, 26, 21, 0, 0, 0, time.UTC)
	if got := asksIn(weekdays, first, first.Add(24*time.Hour), 100); len(got) != 1 || !got[0].Equal(first) {
		t.Fatalf("from its own time: %v", got)
	}
	if got := asksIn(weekdays, from, from.AddDate(0, 0, 14), 3); len(got) != 3 {
		t.Fatalf("at most 3: %v", got)
	}
}

func TestAsksBetween(t *testing.T) {
	standup, retro, lost := uuid.New(), uuid.New(), uuid.New()
	ch1, ch2 := uuid.New(), uuid.New()
	var asked [][]uuid.UUID
	stub := func(rows []model.CheckIn, err error) {
		running = func(channels []uuid.UUID) ([]model.CheckIn, error) {
			asked = append(asked, channels)
			return rows, err
		}
	}
	defer func() { running = model.Running }()
	from := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

	stub([]model.CheckIn{
		{Id: standup, ChannelUUID: ch1, Question: "What did you work on today?", Days: 31, AtMinute: 17 * 60, TZ: "America/New_York"},
		// Sundays only: none this week.
		{Id: retro, ChannelUUID: ch2, Question: "Retro?", Days: 64, AtMinute: 9 * 60, TZ: "UTC"},
		{Id: lost, ChannelUUID: ch2, Question: "Gone zone", Days: 127, AtMinute: 60, TZ: "Mars/Olympus"},
	}, nil)
	got, err := AsksBetween([]uuid.UUID{ch1, ch2}, from, from.AddDate(0, 0, 5), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != standup || got[0].Channel != ch1 || got[0].Question != "What did you work on today?" || len(got[0].At) != 5 {
		t.Fatalf("got %+v", got)
	}
	if len(asked) != 1 || len(asked[0]) != 2 {
		t.Fatalf("read the channels %v", asked)
	}

	// No channels, or no stretch: nothing read.
	asked = nil
	for _, c := range [][]uuid.UUID{nil, {}} {
		if got, err := AsksBetween(c, from, from.AddDate(0, 0, 5), 100); err != nil || len(got) != 0 {
			t.Fatalf("no channels: %v %v", got, err)
		}
	}
	if got, err := AsksBetween([]uuid.UUID{ch1}, from, from, 100); err != nil || len(got) != 0 {
		t.Fatalf("no stretch: %v %v", got, err)
	}
	if len(asked) != 0 {
		t.Fatalf("read with nothing to ask: %v", asked)
	}

	stub(nil, errors.New("down"))
	if _, err := AsksBetween([]uuid.UUID{ch1}, from, from.AddDate(0, 0, 5), 100); err == nil {
		t.Fatal("a failed read was no check-ins")
	}
}
