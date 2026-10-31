package business

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// The acceptance case through the engine: each person's kept hours and
// zone, their names in the reasons, and the asker's zone for the days.
func TestSuggestionsReadEachPersonsOwnHours(t *testing.T) {
	sam, jonas, maya := uuid.New(), uuid.New(), uuid.New()
	oldB, oldH, oldP, oldS := busyFor, hoursOf, peopleOf, peopleSeenBy
	t.Cleanup(func() { busyFor, hoursOf, peopleOf, peopleSeenBy = oldB, oldH, oldP, oldS })
	busyFor = func(context.Context, string, time.Time, time.Time) (calendarBusiness.Busy, error) {
		return calendarBusiness.Busy{}, nil
	}
	peopleSeenBy = func(_ context.Context, _ string, people []string) ([]string, []string, error) {
		return people, nil, nil
	}
	hoursOf = func(context.Context, []uuid.UUID) (map[uuid.UUID]calendarBusiness.WorkingHours, error) {
		return map[uuid.UUID]calendarBusiness.WorkingHours{
			jonas: *week("Europe/London", calendarBusiness.Span{Start: "09:00", End: "17:00"}),
			maya:  *week("Asia/Kolkata", calendarBusiness.Span{Start: "10:00", End: "18:30"}),
		}, nil
	}
	peopleOf = func(context.Context, []string) ([]*dgraphStruct.DgraphUser, error) {
		return []*dgraphStruct.DgraphUser{{Uuid: jonas.String(), UserName: "Jonas"}, {Uuid: maya.String(), UserName: "Maya"}}, nil
	}
	london, _ := time.LoadLocation("Europe/London")
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, london)
	from := time.Date(2026, 10, 19, 0, 0, 0, 0, london)
	in := calendarBusiness.FindTimeInput{Participants: []string{jonas.String(), maya.String()}, DurationMinutes: 30, From: from, To: from.Add(24 * time.Hour),
		Hours: calendarBusiness.WorkingHours{Days: []int{1, 2, 3, 4, 5}, Start: "08:00", End: "18:00", TZ: "Europe/London"}}
	got, err := Suggestions(context.Background(), sam.String(), in, now)
	if err != nil || len(got.Suggestions) == 0 {
		t.Fatalf("%+v %v", got, err)
	}
	first := got.Suggestions[0]
	if first.Reason != "Everyone's free" || !first.Start.Equal(time.Date(2026, 10, 19, 9, 0, 0, 0, london)) {
		t.Errorf("first: %v %q", first.Start.In(london), first.Reason)
	}
	for _, s := range got.Suggestions {
		if s.Reason != "Everyone's free" && s.Reason != "2 of 3 free · outside Maya's hours" && s.Reason != "2 of 3 free · outside Jonas's hours" && s.Reason != "1 of 3 free · outside Jonas and Maya's hours" {
			t.Errorf("%v: %q", s.Start.In(london), s.Reason)
		}
	}
}

// What a request can't ask, in words.
func TestSuggestionsRefuseWhatCantBeAnswered(t *testing.T) {
	now := time.Now()
	me := uuid.NewString()
	var refusal *Refusal
	for _, c := range []struct {
		in   calendarBusiness.FindTimeInput
		says string
	}{
		{calendarBusiness.FindTimeInput{Participants: []string{"someone"}, DurationMinutes: 30, From: now, To: now.Add(time.Hour)}, "One of those people isn't in this workspace."},
		{calendarBusiness.FindTimeInput{DurationMinutes: 5, From: now, To: now.Add(time.Hour)}, "Meetings run from 15 minutes to 8 hours."},
		{calendarBusiness.FindTimeInput{DurationMinutes: 30, From: now, To: now.Add(-time.Hour)}, "Pick a range that ends after it starts."},
	} {
		if _, err := Suggestions(context.Background(), me, c.in, now); !errors.As(err, &refusal) || refusal.Msg != c.says {
			t.Errorf("%q: %v", c.says, err)
		}
	}
}

// In a workspace that keeps when members are busy to themselves, the ranked
// times are the asker's alone: nobody else's calendar or hours are read, no
// reason names them, and they come back as private, as Find a time's do.
func TestSuggestionsLeaveOutWhoseBusyTimesAreKeptPrivate(t *testing.T) {
	prevPolicies := policyBusiness.Policies
	policyBusiness.Policies = busyKeptPrivate{prevPolicies}
	oldB, oldH, oldP := busyFor, hoursOf, peopleOf
	t.Cleanup(func() { policyBusiness.Policies, busyFor, hoursOf, peopleOf = prevPolicies, oldB, oldH, oldP })
	me, maya := uuid.New(), uuid.New()
	var read []string
	var mu sync.Mutex
	busyFor = func(_ context.Context, person string, _, _ time.Time) (calendarBusiness.Busy, error) {
		mu.Lock()
		read = append(read, person)
		mu.Unlock()
		return calendarBusiness.Busy{}, nil
	}
	var hoursAsked []uuid.UUID
	hoursOf = func(_ context.Context, people []uuid.UUID) (map[uuid.UUID]calendarBusiness.WorkingHours, error) {
		hoursAsked = append(hoursAsked, people...)
		return map[uuid.UUID]calendarBusiness.WorkingHours{maya: *week("Asia/Kolkata", calendarBusiness.Span{Start: "10:00", End: "18:30"})}, nil
	}
	peopleOf = func(context.Context, []string) ([]*dgraphStruct.DgraphUser, error) {
		return []*dgraphStruct.DgraphUser{{Uuid: maya.String(), UserName: "Maya"}}, nil
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	from := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	in := calendarBusiness.FindTimeInput{Participants: []string{maya.String()}, DurationMinutes: 30, From: from, To: from.Add(24 * time.Hour)}
	got, err := Suggestions(context.Background(), me.String(), in, now)
	if err != nil || len(got.Suggestions) == 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if !reflect.DeepEqual(read, []string{me.String()}) || !reflect.DeepEqual(hoursAsked, []uuid.UUID{me}) {
		t.Errorf("read the calendars of %v and the hours of %v: only the asker's may be", read, hoursAsked)
	}
	for _, s := range got.Suggestions {
		if strings.Contains(s.Reason, "Maya") || strings.Contains(s.Reason, " of ") {
			t.Errorf("%v: %q, a reason from someone whose times are private", s.Start, s.Reason)
		}
	}
	if !reflect.DeepEqual(got.Private, []string{maya.String()}) {
		t.Errorf("private %v, want Maya", got.Private)
	}
}
