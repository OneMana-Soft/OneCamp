package controller

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// linear-20 head 2: what one forecasts read asks for.
func TestForecastIDs(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	ids, ok := forecastIDs(" " + a.String() + ",," + strings.ToUpper(a.String()) + ",not-an-id," + b.String() + "," + a.String())
	if !ok || len(ids) != 2 || ids[0] != a || ids[1] != b {
		t.Fatalf("got %v %v, want [%s %s] once each, in the order asked, the non-id left out", ids, ok, a, b)
	}
	if ids, ok := forecastIDs(""); !ok || len(ids) != 0 {
		t.Fatalf("nothing asked: %v %v, want none and ok", ids, ok)
	}

	many := make([]string, 0, forecastsCap+1)
	for len(many) < forecastsCap {
		many = append(many, uuid.NewString())
	}
	if ids, ok := forecastIDs(strings.Join(append(many, many...), ",")); !ok || len(ids) != forecastsCap {
		t.Fatalf("%d distinct ids, each twice: %d %v, want all %d and ok (a repeat doesn't count)", forecastsCap, len(ids), ok, forecastsCap)
	}
	if _, ok := forecastIDs(strings.Join(append(many, "not-an-id"), ",")); ok {
		t.Fatalf("%d distinct asks: ok, want refused (a non-id still counts as asked)", forecastsCap+1)
	}
	if _, ok := forecastIDs(strings.Join(append(many, uuid.NewString()), ",")); ok {
		t.Fatalf("%d distinct ids: ok, want refused", forecastsCap+1)
	}
}
