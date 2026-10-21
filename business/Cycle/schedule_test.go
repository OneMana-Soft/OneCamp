package business

import (
	"testing"
	"time"

	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

// B3b: the Cycle Scheduler's plan, pure: which cycles to make so the current
// one and the next few are ready, and which ended cycles to complete.

func day(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

func slots(p SchedulePlan) [][2]time.Time {
	out := make([][2]time.Time, 0, len(p.Create))
	for _, s := range p.Create {
		out = append(out, [2]time.Time{s.StartsAt, s.EndsAt})
	}
	return out
}

func sameSlots(t *testing.T, got SchedulePlan, want ...[2]time.Time) {
	t.Helper()
	g := slots(got)
	if len(g) != len(want) {
		t.Fatalf("made %v, want %v", g, want)
	}
	for i := range want {
		if !g[i][0].Equal(want[i][0]) || !g[i][1].Equal(want[i][1]) {
			t.Fatalf("made %v, want %v", g, want)
		}
	}
}

func span(a, b time.Time) [2]time.Time { return [2]time.Time{a, b} }

func TestScheduleMakesTheCurrentCycleAndTheNextTwo(t *testing.T) {
	s := Schedule{Weeks: 2, Upcoming: 2, RollOver: true, Anchor: day(10, 5)}
	got := PlanSchedule(s, nil, day(10, 7))
	sameSlots(t, got, span(day(10, 5), day(10, 19)), span(day(10, 19), day(11, 2)), span(day(11, 2), day(11, 16)))
	if len(got.Complete) != 0 {
		t.Fatalf("completes %v", got.Complete)
	}
}

func TestACooldownWeekSitsBetweenCycles(t *testing.T) {
	s := Schedule{Weeks: 2, CooldownWeeks: 1, Upcoming: 2, Anchor: day(10, 5)}
	sameSlots(t, PlanSchedule(s, nil, day(10, 7)), span(day(10, 5), day(10, 19)), span(day(10, 26), day(11, 9)), span(day(11, 16), day(11, 30)))
	// In the cooldown there is no current cycle, only the next ones.
	sameSlots(t, PlanSchedule(s, nil, day(10, 21)), span(day(10, 26), day(11, 9)), span(day(11, 16), day(11, 30)))
}

func TestAScheduleThatStartsLaterMakesOnlyUpcomingCycles(t *testing.T) {
	s := Schedule{Weeks: 2, Upcoming: 2, Anchor: day(10, 16)}
	sameSlots(t, PlanSchedule(s, nil, day(10, 7)), span(day(10, 16), day(10, 30)), span(day(10, 30), day(11, 13)))
}

func TestTheScheduleFollowsCyclesMadeByHand(t *testing.T) {
	// A cycle made by hand off the cadence: the next scheduled one starts on
	// the first slot after it ends.
	hand := cycleModel.Cycle{Id: uuid.New(), Number: 1, StartsAt: day(10, 1), EndsAt: day(10, 15)}
	s := Schedule{Weeks: 2, Upcoming: 2, Anchor: day(10, 5)}
	sameSlots(t, PlanSchedule(s, []cycleModel.Cycle{hand}, day(10, 7)), span(day(10, 19), day(11, 2)), span(day(11, 2), day(11, 16)))
}

func TestAnEndedCycleIsCompletedAndTheNextOnesMade(t *testing.T) {
	ended := cycleModel.Cycle{Id: uuid.New(), Number: 1, StartsAt: day(10, 5), EndsAt: day(10, 19)}
	s := Schedule{Weeks: 2, Upcoming: 2, RollOver: true, Anchor: day(10, 5)}
	got := PlanSchedule(s, []cycleModel.Cycle{ended}, day(10, 20))
	sameSlots(t, got, span(day(10, 19), day(11, 2)), span(day(11, 2), day(11, 16)), span(day(11, 16), day(11, 30)))
	if len(got.Complete) != 1 || got.Complete[0] != ended.Id {
		t.Fatalf("completes %v, want the ended cycle", got.Complete)
	}
	// One already completed by hand is left alone.
	at := day(10, 19)
	ended.CompletedAt = &at
	if got := PlanSchedule(s, []cycleModel.Cycle{ended}, day(10, 20)); len(got.Complete) != 0 {
		t.Fatalf("completes a completed cycle: %v", got.Complete)
	}
}

func TestAfterALongPauseTheCycleRunningNowIsMade(t *testing.T) {
	old := cycleModel.Cycle{Id: uuid.New(), Number: 3, StartsAt: day(7, 20), EndsAt: day(8, 3)}
	done := day(8, 3)
	old.CompletedAt = &done
	s := Schedule{Weeks: 2, Upcoming: 1, Anchor: day(8, 3)}
	// Slots run 3 Aug, 17 Aug, ... 28 Sep, 12 Oct: 7 Oct is in 28 Sep's.
	sameSlots(t, PlanSchedule(s, []cycleModel.Cycle{old}, day(10, 7)), span(day(9, 28), day(10, 12)), span(day(10, 12), day(10, 26)))
}

func TestNothingIsMadeWhenTheNextCyclesAreReady(t *testing.T) {
	s := Schedule{Weeks: 1, Upcoming: 2, Anchor: day(10, 5)}
	cycles := []cycleModel.Cycle{
		{Id: uuid.New(), Number: 1, StartsAt: day(10, 5), EndsAt: day(10, 12)},
		{Id: uuid.New(), Number: 2, StartsAt: day(10, 12), EndsAt: day(10, 19)},
		{Id: uuid.New(), Number: 3, StartsAt: day(10, 19), EndsAt: day(10, 26)},
	}
	if got := PlanSchedule(s, cycles, day(10, 7)); len(got.Create) != 0 || len(got.Complete) != 0 {
		t.Fatalf("plans %+v", got)
	}
}

func TestAScheduleKeepsAtMostFifteenReady(t *testing.T) {
	s := Schedule{Weeks: 1, Upcoming: 40, Anchor: day(10, 5)}
	if got := PlanSchedule(s, nil, day(10, 7)); len(got.Create) != 1+MaxUpcoming {
		t.Fatalf("made %d, want the current one and %d", len(got.Create), MaxUpcoming)
	}
}

func TestScheduleInputIsChecked(t *testing.T) {
	for _, in := range []ScheduleInput{
		{Weeks: 0, Upcoming: 2, StartsAt: day(10, 5)},
		{Weeks: 9, Upcoming: 2, StartsAt: day(10, 5)},
		{Weeks: 2, CooldownWeeks: 2, Upcoming: 2, StartsAt: day(10, 5)},
		{Weeks: 2, Upcoming: 0, StartsAt: day(10, 5)},
		{Weeks: 2, Upcoming: 16, StartsAt: day(10, 5)},
		{Weeks: 2, Upcoming: 2},
	} {
		if _, err := in.Schedule(); err == nil {
			t.Errorf("%+v accepted", in)
		}
	}
	got, err := ScheduleInput{Weeks: 2, CooldownWeeks: 1, Upcoming: 3, RollOver: true, StartsAt: day(10, 5)}.Schedule()
	if err != nil || got.Weeks != 2 || got.CooldownWeeks != 1 || got.Upcoming != 3 || !got.RollOver || !got.Anchor.Equal(day(10, 5)) {
		t.Fatalf("got %+v, %v", got, err)
	}
}
