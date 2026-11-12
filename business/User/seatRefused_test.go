package business

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// A join the seat limit refused is passed on, and once the admins are told
// the joiner's error says so; any other failure passes untouched (ADMIN-02).
func TestTellIfSeatRefused(t *testing.T) {
	old := seatRefused
	t.Cleanup(func() { seatRefused = old })
	var heard []string
	told := true
	seatRefused = func(_ context.Context, email, method string) bool {
		heard = append(heard, email+" "+method)
		return told
	}
	ctx := context.Background()

	err := tellIfSeatRefused(ctx, fmt.Errorf("insert: %w", &helpers.SeatLimitError{Limit: 25}), "ana@example.com", "email")
	var seat *helpers.SeatLimitError
	if !errors.As(err, &seat) || !seat.Told || seat.Limit != 25 {
		t.Fatalf("told: %v", err)
	}

	told = false
	err = tellIfSeatRefused(ctx, &helpers.SeatLimitError{Limit: 25}, "bo@example.com", "sso")
	if !errors.As(err, &seat) || seat.Told {
		t.Fatalf("nobody told, so the joiner isn't told anybody was: %v", err)
	}

	other := errors.New("db down")
	if got := tellIfSeatRefused(ctx, other, "cy@example.com", "email"); got != other {
		t.Fatalf("another failure: %v", got)
	}
	if len(heard) != 2 || heard[0] != "ana@example.com email" || heard[1] != "bo@example.com sso" {
		t.Fatalf("heard %v", heard)
	}
}
