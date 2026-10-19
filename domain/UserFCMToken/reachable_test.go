package domain

import (
	"slices"
	"strings"
	"testing"
)

// One malformed id must never fail the read for everyone else, and someone
// named twice is asked about once.
func TestWellFormedUserIDs(t *testing.T) {
	a := "6f0e2a4c-1b7d-4c55-9a43-2f6a8b1c9d01"
	b := "6F0E2A4C-1B7D-4C55-9A43-2F6A8B1C9D02"
	got := WellFormedUserIDs([]string{a, "", "not-a-uuid", b, a, "1; DROP TABLE users"})
	want := []string{a, strings.ToLower(b)}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := WellFormedUserIDs(nil); len(got) != 0 {
		t.Fatalf("got %v for none", got)
	}
}

// The gate is the push's own (notPaused), by person: nothing about devices
// decides who is told, and the version that goes through a pause has no gate.
func TestReachableQueriesShareThePushGate(t *testing.T) {
	if !strings.Contains(reachableQuery(), notPaused("u.id")) {
		t.Fatal("ReachableNow doesn't apply the push gate")
	}
	if !strings.Contains(reachableQuery(), "LEFT JOIN users_fcm_token") {
		t.Fatal("ReachableNow must keep people with no device (LEFT JOIN)")
	}
	if strings.Contains(reachableThroughPauseQuery, "users_notification_preferences") {
		t.Fatal("ReachableThroughPause must not apply the gate")
	}
	// A deactivated person is never told, through a pause or not (BE-11-G-3).
	for name, q := range map[string]string{"ReachableNow": reachableQuery(), "ReachableThroughPause": reachableThroughPauseQuery} {
		if !strings.Contains(q, stillActive("u.id")) {
			t.Errorf("%s doesn't leave out deactivated people", name)
		}
	}
}
