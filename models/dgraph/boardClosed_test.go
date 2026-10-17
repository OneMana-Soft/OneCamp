package models

import (
	"net/url"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// helpers.ClosedLimit and helpers.OpenLimit hold their own copies of these
// limits (helpers cannot import this package); this keeps them from drifting
// apart.
func TestBoardLimitsAgreeWithTheHelpers(t *testing.T) {
	if got := helpers.ClosedLimit(url.Values{}); got != BoardClosedLimit {
		t.Errorf("closed default %d, want BoardClosedLimit %d", got, BoardClosedLimit)
	}
	if got := helpers.ClosedLimit(url.Values{"closedLimit": {"1000000"}}); got != BoardClosedMax {
		t.Errorf("closed ceiling %d, want BoardClosedMax %d", got, BoardClosedMax)
	}
	if got := helpers.OpenLimit(url.Values{}); got != BoardOpenLimit {
		t.Errorf("open default %d, want BoardOpenLimit %d", got, BoardOpenLimit)
	}
	if got := helpers.OpenLimit(url.Values{"openLimit": {"1000000"}}); got != BoardOpenMax {
		t.Errorf("open ceiling %d, want BoardOpenMax %d", got, BoardOpenMax)
	}
}
