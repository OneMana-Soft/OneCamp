//go:build integration
// +build integration

package integration_test

// Refresh tokens against a real Redis: traded once, the previous one honoured
// for another tab, a copy used later signs the device out, and two tabs
// refreshing at once end up holding the same token.
//
// Run: go test -tags=integration ./tests/integration/ -run TestRefresh -v

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestRefreshTokensAreTradedOnceAndCopiesEndTheSession(t *testing.T) {
	t.Setenv("JWT_SECRET", "session-rotation-integration-secret")
	integration.SetupRedis(t)
	ctx := context.Background()
	user := uuid.NewString()
	authExp, refreshExp := time.Now().Add(5*time.Minute).Unix(), time.Now().Add(30*24*time.Hour).Unix()
	signIn := func(device string) string {
		t.Helper()
		_, refresh, err := userBusiness.StartSession(ctx, user, device, authExp, refreshExp)
		if err != nil {
			t.Fatalf("sign in: %v", err)
		}
		return refresh
	}
	rotate := func(device, presented string) (string, error) {
		_, refresh, err := userBusiness.RotateRefreshToken(ctx, user, device, presented, authExp, refreshExp)
		return refresh, err
	}

	r0 := signIn("laptop")
	r1, err := rotate("laptop", r0)
	if err != nil || r1 == "" || r1 == r0 {
		t.Fatalf("first trade: %q %v", r1, err)
	}
	// Another tab sent the old one as this one refreshed: it gets the new one.
	if got, err := rotate("laptop", r0); err != nil || got != r1 {
		t.Fatalf("the previous token, from another tab: %q %v, want %q", got, err, r1)
	}
	r2, err := rotate("laptop", r1)
	if err != nil || r2 == r1 {
		t.Fatalf("second trade: %q %v", r2, err)
	}
	// r0 is now two trades old: someone else has a copy. Signed out.
	if _, err := rotate("laptop", r0); !errors.Is(err, userBusiness.ErrRefreshReused) {
		t.Fatalf("a token two trades old: %v, want ErrRefreshReused", err)
	}
	if _, err := rotate("laptop", r2); !errors.Is(err, userBusiness.ErrRefreshUnknown) {
		t.Fatalf("after the device was signed out: %v, want ErrRefreshUnknown", err)
	}

	// Twenty tabs refresh with the same token at once: one trades it, the
	// rest get its successor, so every tab ends up holding the same token.
	start := signIn("phone")
	var wg sync.WaitGroup
	got := make([]string, 20)
	errs := make([]error, 20)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = rotate("phone", start)
		}(i)
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i] != got[0] || got[i] == start {
			t.Fatalf("tab %d: %q %v; tab 0 got %q", i, got[i], errs[i], got[0])
		}
	}
	if _, err := rotate("phone", got[0]); err != nil {
		t.Fatalf("the token every tab holds still trades: %v", err)
	}

	// Changing the password here ends the other devices and keeps this one.
	keep, other := signIn("desk"), signIn("tablet")
	if err := userBusiness.EndOtherSessions(ctx, user, "desk"); err != nil {
		t.Fatal(err)
	}
	if _, err := rotate("desk", keep); err != nil {
		t.Errorf("the device the password was changed on: %v", err)
	}
	if _, err := rotate("tablet", other); !errors.Is(err, userBusiness.ErrRefreshUnknown) {
		t.Errorf("another device after the change: %v, want ErrRefreshUnknown", err)
	}
}
