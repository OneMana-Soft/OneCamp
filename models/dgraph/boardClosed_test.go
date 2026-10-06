package models

import (
	"net/url"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// helpers.ClosedLimit holds its own copy of these limits (helpers cannot
// import this package); this keeps the two from drifting apart.
func TestClosedLimitAgreesWithTheBoardLimits(t *testing.T) {
	if got := helpers.ClosedLimit(url.Values{}); got != BoardClosedLimit {
		t.Errorf("default %d, want BoardClosedLimit %d", got, BoardClosedLimit)
	}
	if got := helpers.ClosedLimit(url.Values{"closedLimit": {"1000000"}}); got != BoardClosedMax {
		t.Errorf("ceiling %d, want BoardClosedMax %d", got, BoardClosedMax)
	}
}

// The time report asks for every task (0); a board for its newest few.
func TestClosedFirst(t *testing.T) {
	cases := map[int]string{0: "", -1: "", 200: ", first: 200", 999999: ", first: 2000"}
	for in, want := range cases {
		if got := ClosedFirst(in); got != want {
			t.Errorf("ClosedFirst(%d) = %q, want %q", in, got, want)
		}
	}
}
