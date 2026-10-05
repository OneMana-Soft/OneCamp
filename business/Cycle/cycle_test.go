package business

import (
	"testing"
	"time"

	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
)

func TestState(t *testing.T) {
	at := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	c := cycleModel.Cycle{StartsAt: at(5), EndsAt: at(19)}
	for now, want := range map[time.Time]string{at(4): "upcoming", at(5): "current", at(18): "current", at(19): "ended"} {
		if got := State(c, now); got != want {
			t.Errorf("%v: got %s, want %s", now, got, want)
		}
	}
	done := at(20)
	c.CompletedAt = &done
	if State(c, at(10)) != "completed" {
		t.Error("completed cycle not completed")
	}
}

func TestCount(t *testing.T) {
	p := Count([]string{"todo", "inProgress", "inReview", "done", "canceled", "backlog"})
	if p != (Progress{Total: 6, Started: 2, Done: 2}) {
		t.Fatalf("got %+v", p)
	}
}

func TestWindow(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	// Midnight in India is 18:30 the day before in UTC; the cycle keeps it.
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, ist).UTC()
	s, e, err := Window(start, 2)
	if err != nil || !s.Equal(start) || !e.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, ist)) {
		t.Fatalf("got %v %v %v", s, e, err)
	}
	for _, w := range []int{0, 9} {
		if _, _, err := Window(time.Now(), w); err == nil {
			t.Errorf("%d weeks accepted", w)
		}
	}
	if _, _, err := Window(time.Time{}, 2); err == nil {
		t.Error("no start accepted")
	}
}
