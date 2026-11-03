//go:build integration
// +build integration

package integration_test

// People joining at once at a free plan's last seat: exactly one gets it.
// Each way in (the member insert under sign-up, invitation, SSO and SCIM;
// adopting an external row; reactivation) counts the members and takes the
// seat under one transaction lock (domain/User WithSeat), so none of them,
// alone or mixed, leaves the workspace past its limit (V-237). And an admin
// reactivating someone into a full plan is told why.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

func TestTwoJoinsAtOnceNeverPassTheSeatLimit(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	// Each case below sets the plan's limit; the tests after this one get it
	// back as it was (its subtests run one at a time, inside this function).
	oldLimit := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = oldLimit })
	lr := setupLandReady(t)
	ctx := lr.ctx
	now := time.Now()
	people := 0
	next := func() (email, name string) {
		people++
		return fmt.Sprintf("p%d@example.test", people), fmt.Sprintf("p%d", people)
	}
	// Each maker sets its person up with the limit lifted, and hands back the
	// join to race.
	signUp := func(t *testing.T) func() error {
		email, name := next()
		id := uuid.New()
		return func() error { return userDomain.CreateUserWithMethod(ctx, email, name, nil, id, "", false) }
	}
	gone := func(t *testing.T) uuid.UUID {
		t.Helper()
		helpers.SeatLimit = ""
		email, name := next()
		id := uuid.New()
		if err := userDomain.CreateUserWithMethod(ctx, email, name, nil, id, "", false); err != nil {
			t.Fatal(err)
		}
		if err := userDomain.UpdateDeletedTimeByUUID(ctx, &now, &now, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	comeBack := func(t *testing.T) func() error {
		id := gone(t)
		return func() error { return userDomain.UpdateDeletedTimeToNullByUUID(ctx, &now, id) }
	}
	adopt := func(t *testing.T) func() error {
		t.Helper()
		email, _ := next()
		if _, err := lr.env.PG.Exec(`INSERT INTO users (id, email_id, is_external) VALUES ($1, $2, true)`, uuid.New(), email); err != nil {
			t.Fatal(err)
		}
		return func() error {
			_, adopted, err := userDomain.AdoptExternalUser(ctx, email, nil, "", false)
			if err == nil && !adopted {
				return fmt.Errorf("nothing left to adopt for %s", email)
			}
			return err
		}
	}
	// atTheLastSeat sets the limit one above the members there are, runs every
	// join at once, and wants one in, the rest refused with the limit's error,
	// and the workspace exactly full.
	atTheLastSeat := func(t *testing.T, joins []func() error) {
		t.Helper()
		used, _, err := userDomain.SeatUsage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		limit := used + 1
		helpers.SeatLimit = strconv.Itoa(limit)
		errs := make([]error, len(joins))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, join := range joins {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = join()
			}()
		}
		close(start)
		wg.Wait()
		in, refused := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				in++
			case helpers.IsSeatLimit(err):
				refused++
			default:
				t.Errorf("a join failed for another reason: %v", err)
			}
		}
		if in != 1 || refused != len(joins)-1 {
			t.Errorf("%d joins at the last seat: %d in, %d refused; want 1 in, %d refused", len(joins), in, refused, len(joins)-1)
		}
		if used, _, err := userDomain.SeatUsage(ctx); err != nil || used != limit {
			t.Errorf("members = %d (%v), want exactly the limit %d", used, err, limit)
		}
	}
	race := func(t *testing.T, n int, makers ...func(*testing.T) func() error) {
		t.Helper()
		var joins []func() error
		for range n {
			for _, maker := range makers {
				joins = append(joins, maker(t))
			}
		}
		atTheLastSeat(t, joins)
	}

	t.Run("ten sign-ups", func(t *testing.T) { race(t, 10, signUp) })
	t.Run("ten members coming back", func(t *testing.T) { race(t, 10, comeBack) })
	t.Run("ten external rows adopted", func(t *testing.T) { race(t, 10, adopt) })
	t.Run("all three ways at once take one lock", func(t *testing.T) { race(t, 3, signUp, comeBack, adopt) })

	t.Run("an admin's Activate on a full plan says why", func(t *testing.T) {
		id := gone(t)
		used, _, err := userDomain.SeatUsage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		helpers.SeatLimit = strconv.Itoa(used)
		raw, _ := json.Marshal(map[string]string{"user_uuid": id.String()})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).
			WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, lr.owner))
		userController.ActivateUser(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"seat_limit"`) {
			t.Fatalf("Activate on a full plan = %d %s, want 403 with the seat_limit code", rec.Code, rec.Body.String())
		}
		var stillGone bool
		if err := lr.env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM users WHERE id = $1`, id).Scan(&stillGone); err != nil || !stillGone {
			t.Fatalf("a refused Activate must leave the member deactivated (%v)", err)
		}
	})
}
