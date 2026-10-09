//go:build integration
// +build integration

package integration_test

// A guest's open link is audited once per half hour, not on every refresh.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAGuestOpeningALinkIsAuditedOncePerHalfHour -v

import (
	"context"
	"testing"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAGuestOpeningALinkIsAuditedOncePerHalfHour(t *testing.T) {
	integration.SetupRedis(t)
	ctx := context.Background()
	link, other := uuid.New(), uuid.New()

	if !guestBusiness.AuditAccessDue(ctx, link, "") {
		t.Fatal("the first open of a link is recorded")
	}
	// The page refreshes every five seconds.
	for i := 0; i < 12; i++ {
		if guestBusiness.AuditAccessDue(ctx, link, "") {
			t.Fatalf("refresh %d within the half hour was recorded again", i+1)
		}
	}
	if !guestBusiness.AuditAccessDue(ctx, link, "Priya") {
		t.Fatal("another guest on the same link is recorded")
	}
	if guestBusiness.AuditAccessDue(ctx, link, "Priya") {
		t.Fatal("that guest's refresh is not")
	}
	if !guestBusiness.AuditAccessDue(ctx, other, "") {
		t.Fatal("another link is recorded")
	}
}
