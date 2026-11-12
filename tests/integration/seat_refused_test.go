//go:build integration
// +build integration

package integration_test

// Someone whose own way in (sign-up, an invitation, SSO's first sign-in,
// SCIM) meets the free plan's limit is refused, and every admin is told in
// their Inbox, once that day: before, only the joiner heard, and the admin
// found out when someone asked why they couldn't get in (ADMIN-02).

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	_ "github.com/akashc777/OneCamp/business/Seats"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func TestJoinAsMemberSeatLimitTellsAdmins(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	lr := setupLandReady(t)
	ctx := lr.ctx
	used, _, err := userDomain.SeatUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = old })
	helpers.SeatLimit = strconv.Itoa(used)

	notices := func() int {
		t.Helper()
		var n int
		if err := lr.env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE kind = 'member.seat_refused' AND user_uuid = $1`,
			lr.owner.UserPostgresInfo.Id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Two tries at once, in either case: one claim (the notice's unique
	// key), so the admin is told once, and each joiner hears that they were.
	tries := []string{"Late@Example.test", "late@example.test"}
	errs := make([]error, len(tries))
	var wg sync.WaitGroup
	for i, try := range tries {
		wg.Add(1)
		go func(i int, try string) {
			defer wg.Done()
			_, errs[i] = userBusiness.JoinAsMember(ctx, try, "Late", nil, userModels.AuthMethodEmail, false)
		}(i, try)
	}
	wg.Wait()
	for _, err := range errs {
		var seat *helpers.SeatLimitError
		if !errors.As(err, &seat) || !seat.Told || !strings.Contains(err.Error(), "Your admin has been told") {
			t.Fatalf("joining a full plan: %v, want the seat limit, saying the admin was told", err)
		}
	}
	if n := notices(); n != 1 {
		t.Fatalf("the admin was told %d times of one person's refusals that day, want once", n)
	}
	var words, limit string
	if err := lr.env.PG.QueryRow(`SELECT data->>'text', data->>'limit' FROM resource_notices WHERE kind = 'member.seat_refused' AND user_uuid = $1`,
		lr.owner.UserPostgresInfo.Id).Scan(&words, &limit); err != nil {
		t.Fatal(err)
	}
	if words != "late@example.test" || limit != strconv.Itoa(used) {
		t.Fatalf("the notice names %q against a limit of %q", words, limit)
	}
}

// Someone an import or GitHub sync left as an external row joins with that
// address: adopting the row takes a seat (domain.AdoptExternalUser, in
// WithSeat), so at the limit it is refused there, before any account is
// made, and the admins are told of it as on every other way in.
func TestAdoptingSomeoneAtTheLimitTellsAdmins(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	lr := setupLandReady(t)
	ctx := lr.ctx
	const email = "jira-ana@example.test"
	id := uuid.New()
	if _, err := lr.env.PG.Exec(`INSERT INTO users (id, email_id, is_external, created_at, updated_at) VALUES ($1, $2, true, NOW(), NOW())`, id, email); err != nil {
		t.Fatal(err)
	}
	lr.dg.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": id.String(),
		"user_email_id": email, "user_name": "jira-ana", "user_full_name": "Ana", "is_external": true})
	used, _, err := userDomain.SeatUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = old })
	helpers.SeatLimit = strconv.Itoa(used)

	_, err = userBusiness.JoinAsMember(ctx, email, "Ana", nil, userModels.AuthMethodEmail, false)
	var seat *helpers.SeatLimitError
	if !errors.As(err, &seat) || !seat.Told {
		t.Fatalf("adopting at the limit: %v, want the seat limit, saying the admin was told", err)
	}
	var external bool
	if err := lr.env.PG.QueryRow(`SELECT is_external FROM users WHERE id = $1`, id).Scan(&external); err != nil || !external {
		t.Fatalf("the refused adoption changed the row: external %v, %v", external, err)
	}
	var words string
	if err := lr.env.PG.QueryRow(`SELECT data->>'text' FROM resource_notices WHERE kind = 'member.seat_refused' AND user_uuid = $1`,
		lr.owner.UserPostgresInfo.Id).Scan(&words); err != nil || words != email {
		t.Fatalf("the admin's notice names %q (%v), want %q", words, err, email)
	}
}
