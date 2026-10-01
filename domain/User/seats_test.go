package domain

import (
	"context"
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

func withSeats(t *testing.T, limit, used int, countErr error, deactivatedMember bool) {
	t.Helper()
	oldL, oldC, oldD := seatLimit, countActiveMembers, isDeactivatedMember
	t.Cleanup(func() { seatLimit, countActiveMembers, isDeactivatedMember = oldL, oldC, oldD })
	seatLimit = func() int { return limit }
	countActiveMembers = func(context.Context) (int, error) { return used, countErr }
	isDeactivatedMember = func(context.Context, uuid.UUID) (bool, error) { return deactivatedMember, nil }
}

func TestEnsureSeatAvailable(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		limit, used int
		countErr    error
		refuse      bool
	}{
		{"unlimited", 0, 500, nil, false},
		{"room left", 25, 24, nil, false},
		{"full", 25, 25, nil, true},
		{"over (limit lowered)", 25, 30, nil, true},
		{"count failed: never lock people out", 25, 0, errors.New("timeout"), false},
	}
	for _, c := range cases {
		withSeats(t, c.limit, c.used, c.countErr, false)
		err := EnsureSeatAvailable(ctx)
		if (err != nil) != c.refuse {
			t.Errorf("%s: err = %v, want refuse=%v", c.name, err, c.refuse)
		}
		if err != nil && !helpers.IsSeatLimit(err) {
			t.Errorf("%s: refusal must be a SeatLimitError, got %T", c.name, err)
		}
	}
}

func TestReactivationCountsOnlyAReturningMember(t *testing.T) {
	ctx := context.Background()
	withSeats(t, 25, 25, nil, true)
	if err := ensureSeatForReactivation(ctx, uuid.New()); !helpers.IsSeatLimit(err) {
		t.Fatalf("reactivating a member into a full workspace must be refused, got %v", err)
	}
	withSeats(t, 25, 25, nil, false)
	if err := ensureSeatForReactivation(ctx, uuid.New()); err != nil {
		t.Fatalf("an active user, a bot or an external identity adds no seat, got %v", err)
	}
	withSeats(t, 0, 99, nil, true)
	if err := ensureSeatForReactivation(ctx, uuid.New()); err != nil {
		t.Fatalf("unlimited licences never refuse, got %v", err)
	}
}
