package business

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// V-967: the AI scheduler sees only the busy times its asker may see, by the
// calendar's one rule (PeopleSeenBy), as Find a time does: a member outside
// the policy's view never moves the proposed slots, and an admin-off policy
// hides everyone's busy times from the AI as it does from people.

// fakeSeen stands in for the calendar's one rule (PeopleSeenBy): the asker
// sees their own busy times, and someone else's only while the workspace lets
// members see them and that person isn't kept private. It answers who asked.
func fakeSeen(t *testing.T, policyOn bool, private ...string) *[]string {
	t.Helper()
	prev := peopleSeenBy
	t.Cleanup(func() { peopleSeenBy = prev })
	var askers []string
	peopleSeenBy = func(_ context.Context, asker string, people []string) (seen, hidden []string, err error) {
		askers = append(askers, asker)
		for _, p := range people {
			if p == asker || (policyOn && !slices.Contains(private, p)) {
				seen = append(seen, p)
			} else {
				hidden = append(hidden, p)
			}
		}
		return seen, hidden, nil
	}
	return &askers
}

// busyAt is a busy block on mondayUTC from one hour to another.
func busyAt(from, to int) calendarBusiness.Busy {
	day := mondayUTC()
	return calendarBusiness.Busy{Intervals: []calendarBusiness.Interval{{Start: day.Add(time.Duration(from) * time.Hour), End: day.Add(time.Duration(to) * time.Hour)}}}
}

// proposed is proposeSlots' answer for asker and people on mondayUTC's
// business hours, under whatever fakes are in.
func proposed(asker string, people ...string) *adapter.ScheduleProposeResponse {
	resp := &adapter.ScheduleProposeResponse{}
	for _, p := range people {
		name := strings.ToUpper(p[:1]) + p[1:]
		if p == asker {
			name = "You"
		}
		resp.Participants = append(resp.Participants, adapter.ScheduleParticipant{UUID: p, Name: name})
	}
	proposeSlots(context.Background(), resp, asker, people, mondayUTC(), 1, 60, time.UTC, 9, 18)
	return resp
}

// The workspace lets members see when others are busy, but Maya is kept
// private from the asker: her busy day is never read, the slots are what they
// are without her, and no note names her.
func TestTheAISchedulerNeverCountsSomeoneKeptPrivate(t *testing.T) {
	read := fakeBusy(t, map[string]calendarBusiness.Busy{"me": busyAt(9, 10), "maya": busyAt(9, 18), "jonas": busyAt(14, 15)}, nil, nil)
	fakeSeen(t, true, "maya")
	got := proposed("me", "me", "maya", "jonas")
	if slices.Contains(*read, "maya") {
		t.Errorf("read %v: Maya's calendar was read", *read)
	}
	want := proposed("me", "me", "jonas")
	if len(got.Candidates) == 0 || !reflect.DeepEqual(got.Candidates, want.Candidates) {
		t.Errorf("slots with Maya kept private %+v, want those without her %+v", got.Candidates, want.Candidates)
	}
	if strings.Contains(got.Note, "Maya") || !strings.Contains(got.Note, "count only the calendars you may see") {
		t.Errorf("the note names Maya or doesn't say who counts: %q", got.Note)
	}
}

// With the workspace's policy off, nobody's busy times reach the AI: the
// slots are the asker's own calendar alone, and only theirs is read. The asker
// is who asks, wherever they are in the list.
func TestTheAISchedulerWithThePolicyOffSeesOnlyTheAsker(t *testing.T) {
	busy := map[string]calendarBusiness.Busy{"me": busyAt(9, 10), "maya": busyAt(10, 18), "jonas": busyAt(9, 18)}
	for name, people := range map[string][]string{"asker first": {"me", "maya", "jonas"}, "asker last": {"maya", "jonas", "me"}} {
		read := fakeBusy(t, busy, nil, nil)
		askers := fakeSeen(t, false)
		got := proposed("me", people...)
		if !reflect.DeepEqual(*read, []string{"me"}) || !reflect.DeepEqual(*askers, []string{"me"}) {
			t.Errorf("%s: read %v as %v; want only the asker's, as the asker", name, *read, *askers)
		}
		want := proposed("me", "me")
		if len(got.Candidates) == 0 || !reflect.DeepEqual(got.Candidates, want.Candidates) {
			t.Errorf("%s: slots %+v, want the asker's own %+v", name, got.Candidates, want.Candidates)
		}
		if strings.Contains(got.Note, "Maya") || strings.Contains(got.Note, "Jonas") {
			t.Errorf("%s: the note names someone kept private: %q", name, got.Note)
		}
	}
}

// When whose busy times show can't be read, nobody's time counts as free and
// no calendar is read.
func TestTheAISchedulerCountsNobodyFreeWhenTheRuleCantBeRead(t *testing.T) {
	read := fakeBusy(t, map[string]calendarBusiness.Busy{}, nil, nil)
	peopleSeenBy = func(context.Context, string, []string) ([]string, []string, error) {
		return nil, nil, errors.New("postgres down")
	}
	got := proposed("me", "me", "maya")
	if len(*read) != 0 {
		t.Errorf("read %v without the rule", *read)
	}
	for _, c := range got.Candidates {
		if c.AllFree || c.FreeCount != 0 {
			t.Fatalf("candidate %+v counts someone free", c)
		}
	}
	if !strings.Contains(got.Note, "Couldn't read your calendar") {
		t.Errorf("note %q", got.Note)
	}
}

// Reschedule options are the same rule's: an event Maya made, with the asker
// and Jonas on it, all three busy at its time but the asker. With the policy
// off nobody conflicts and the slots are the asker's own; with it on and Maya
// kept private, only Jonas does.
func TestRescheduleOptionsSeeOnlyWhatTheAskerMay(t *testing.T) {
	start, end := mondayUTC().Add(10*time.Hour), mondayUTC().Add(11*time.Hour)
	event := &dgraphStruct.DgraphEvent{Uuid: "review", StartTime: &start, EndTime: &end,
		CreatedBy:    &dgraphStruct.DgraphUser{Uuid: "maya", UserName: "Maya"},
		Participants: []*dgraphStruct.DgraphUser{{Uuid: "me", UserName: "Me"}, {Uuid: "jonas", UserName: "Jonas"}}}
	alone := &dgraphStruct.DgraphEvent{Uuid: "review", StartTime: &start, EndTime: &end, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "me"}}
	busy := map[string]calendarBusiness.Busy{"me": busyAt(14, 15), "maya": busyAt(9, 18), "jonas": busyAt(10, 11)}
	options := func(e *dgraphStruct.DgraphEvent) *adapter.ScheduleRescheduleResponse {
		resp := &adapter.ScheduleRescheduleResponse{Conflicts: []string{}}
		rescheduleSlots(context.Background(), resp, "me", e, mondayUTC(), 1, 60, time.UTC, 9, 18)
		return resp
	}

	read := fakeBusy(t, busy, nil, nil)
	askers := fakeSeen(t, false)
	got := options(event)
	if !reflect.DeepEqual(*read, []string{"me"}) || !reflect.DeepEqual(*askers, []string{"me"}) {
		t.Errorf("policy off: read %v as %v; want only the asker's, as the asker", *read, *askers)
	}
	if got.ConflictCount != 0 || len(got.Conflicts) != 0 {
		t.Errorf("policy off: conflicts %v name someone the asker may not see", got.Conflicts)
	}
	if want := options(alone); len(got.Candidates) == 0 || !reflect.DeepEqual(got.Candidates, want.Candidates) {
		t.Errorf("policy off: slots %+v, want the asker's own %+v", got.Candidates, want.Candidates)
	}

	read = fakeBusy(t, busy, nil, nil)
	fakeSeen(t, true, "maya")
	got = options(event)
	if slices.Contains(*read, "maya") || !reflect.DeepEqual(got.Conflicts, []string{"Jonas"}) || strings.Contains(got.Note, "Maya") {
		t.Errorf("Maya kept private: read %v, conflicts %v, note %q; want Jonas's conflict alone", *read, got.Conflicts, got.Note)
	}
}
