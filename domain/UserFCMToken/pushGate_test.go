package domain

import (
	"strings"
	"testing"
)

// Every lookup of devices to push to applies the whole push gate, whichever
// place pushes: a deactivated person's devices are never handed out
// (deactivation leaves their tokens in place, and BE-11-G-3 had closed only
// Deliver's lookup, so whatever looked devices up for itself still reached
// their phone), and neither are the devices of someone paused, in quiet hours
// or in focus time.
func TestEveryDeviceLookupAppliesThePushGate(t *testing.T) {
	for _, c := range []struct {
		lookup, query, userCol string
	}{
		{"GetFCMTokenByUserId", tokensOfPersonQuery, "users_fcm_token.user_id"},
		{"GetFCMTokenByListOfUserId", tokensOfPeopleQuery(3), "users_fcm_token.user_id"},
		{"ReachableNow", reachableQuery, "u.id"},
	} {
		if !strings.Contains(c.query, stillActive(c.userCol)) {
			t.Errorf("%s hands out a deactivated person's devices", c.lookup)
		}
		if !strings.Contains(c.query, notPaused(c.userCol)) {
			t.Errorf("%s hands out the devices of someone paused, in quiet hours or in focus time", c.lookup)
		}
	}
}
