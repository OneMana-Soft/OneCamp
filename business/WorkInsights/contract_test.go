package business

import (
	"context"
	"errors"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
)

// There is no change log to measure yet.
func TestInsightsArentBuiltYet(t *testing.T) {
	_, err := Insights.Series(context.Background(), accessBusiness.Subject{}, CycleTime, querySpec.Spec{Scope: querySpec.Scope{Kind: querySpec.ScopeMine}}, time.Now().AddDate(0, 0, -7), time.Now())
	if !errors.Is(err, ErrNotBuilt) {
		t.Errorf("%v", err)
	}
}
