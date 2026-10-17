package business

import (
	"errors"
	"testing"
	"time"

	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
)

// Next is the Scheduler's NextRun, rule for rule.
func TestNextIsTheSchedulersNextRun(t *testing.T) {
	prev := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC) // a Tuesday
	after := prev.Add(time.Minute)
	for _, rule := range []string{"FREQ=DAILY", "FREQ=WEEKLY;BYDAY=MO,TH", "FREQ=MONTHLY;INTERVAL=2", "FREQ=YEARLY", "", "FREQ=SOMETIMES"} {
		want, wantErr := schedulerBusiness.NextRun(rule, after, prev)
		got, err := Rules.Next(rule, after, prev)
		if !got.Equal(want) || (err == nil) != (wantErr == nil) {
			t.Errorf("%q: %v %v, NextRun says %v %v", rule, got, err, want, wantErr)
		}
	}
}

// Nothing describes a rule on the server yet.
func TestDescribeIsntBuiltYet(t *testing.T) {
	if _, err := Rules.Describe("FREQ=DAILY"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("%v", err)
	}
}
