package domain

import (
	"context"
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// withSeats stubs the licence, the count and the reactivation check, and runs
// a seat-taking write as softDelete.Bound.Add does (count, then add while
// there is room) without a database; tests/integration/seat_race_test.go
// proves the lock itself.
func withSeats(t *testing.T, limit, used int, countErr error, deactivatedMember bool) {
	t.Helper()
	oldL, oldC, oldD, oldA := seatLimit, countActiveMembers, isDeactivatedMember, addCapped
	t.Cleanup(func() { seatLimit, countActiveMembers, isDeactivatedMember, addCapped = oldL, oldC, oldD, oldA })
	seatLimit = func() int { return limit }
	countActiveMembers = func(context.Context) (int, error) { return used, countErr }
	isDeactivatedMember = func(context.Context, uuid.UUID) (bool, error) { return deactivatedMember, nil }
	addCapped = func(b softDelete.Bound, ctx context.Context, add func(context.Context) error) error {
		if b.Lock != seatsLock {
			t.Errorf("seats are taken under lock %q, want %q", b.Lock, seatsLock)
		}
		n, err := b.Count(ctx)
		if err != nil {
			return err
		}
		if n >= b.Most {
			return b.Full
		}
		return add(ctx)
	}
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

func TestWithSeat(t *testing.T) {
	ctx := context.Background()
	failed := errors.New("insert failed")
	cases := []struct {
		name        string
		limit, used int
		countErr    error
		addErr      error
		wantAdds    int
		want        func(error) bool
	}{
		{"unlimited", 0, 500, nil, nil, 1, func(err error) bool { return err == nil }},
		{"room left", 25, 24, nil, nil, 1, func(err error) bool { return err == nil }},
		{"full", 25, 25, nil, nil, 0, helpers.IsSeatLimit},
		{"over (limit lowered)", 25, 30, nil, nil, 0, helpers.IsSeatLimit},
		{"count failed: never lock people out", 25, 0, errors.New("timeout"), nil, 1, func(err error) bool { return err == nil }},
		{"the write's own failure, once", 25, 3, nil, failed, 1, func(err error) bool { return errors.Is(err, failed) }},
	}
	for _, c := range cases {
		withSeats(t, c.limit, c.used, c.countErr, false)
		adds := 0
		err := WithSeat(ctx, func(context.Context) error { adds++; return c.addErr })
		if adds != c.wantAdds || !c.want(err) {
			t.Errorf("%s: wrote %d times, err = %v; want %d writes", c.name, adds, err, c.wantAdds)
		}
	}
}

func TestReactivationCountsOnlyAReturningMember(t *testing.T) {
	ctx := context.Background()
	reactivate := func() (int, error) {
		updates := 0
		err := withSeatForReactivation(ctx, uuid.New(), func(context.Context) error { updates++; return nil })
		return updates, err
	}
	withSeats(t, 25, 25, nil, true)
	if n, err := reactivate(); !helpers.IsSeatLimit(err) || n != 0 {
		t.Fatalf("reactivating a member into a full workspace must be refused unwritten, got %d writes, %v", n, err)
	}
	withSeats(t, 25, 24, nil, true)
	if n, err := reactivate(); err != nil || n != 1 {
		t.Fatalf("the last seat takes a returning member, got %d writes, %v", n, err)
	}
	withSeats(t, 25, 25, nil, false)
	if n, err := reactivate(); err != nil || n != 1 {
		t.Fatalf("an active user, a bot or an external identity adds no seat, got %d writes, %v", n, err)
	}
	withSeats(t, 0, 99, nil, true)
	if n, err := reactivate(); err != nil || n != 1 {
		t.Fatalf("unlimited licences never refuse, got %d writes, %v", n, err)
	}
}
