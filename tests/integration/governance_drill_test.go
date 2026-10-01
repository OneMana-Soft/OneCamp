//go:build integration
// +build integration

package integration_test

// Does the governance drill actually run, and can it fail?
//
// WHAT THIS PROVES THAT NOTHING ELSE DID. The drill is the one place the product's
// central claim is demonstrated rather than asserted: an agent may only do what the
// person behind it could, the decision is written before the tool runs, and the
// refusal is in a hash chain. Its unit tests cover the fail-closed logic and the
// audit-before-execute ORDERING, both against the source. Nothing had executed it.
// A demo whose whole purpose is to be run in front of a buyer, that has never been
// run, is the same shape as every bug found this week: correct machinery, an
// unexercised seam, green tests throughout.
//
// The second test is the one that matters most. A drill that cannot fail is a
// rubber stamp, and a rubber stamp on a compliance claim is worse than no claim.
// So the fixture is deliberately broken -- the person IS in the channel -- and the
// drill must refuse to report a pass.
//
// Run: go test -tags=integration ./tests/integration/ -run TestGovernanceDrill -v

import (
	"context"
	"strings"
	"testing"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	drill "github.com/akashc777/OneCamp/business/AIDrill"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// drillFixture seeds one person and the two drill channels, and returns the
// person as the executor itself builds them -- through
// aiBusiness.BuildUserInfoByUserUUID, so the principal under test is assembled by
// production code from production rows rather than hand-filled by the test.
//
// inForbidden decides whether the person is a member of #drill-finance, which is
// the single difference between a drill that should pass and a fixture that has
// drifted.
func drillFixture(t *testing.T, inForbidden bool) (context.Context, uuid.UUID) {
	t.Helper()
	dg := wireStores(t)
	ctx := context.Background()

	userID := uuid.New()
	email := "drill-" + userID.String()[:8] + "@example.test"
	allowedID, forbiddenID := uuid.New(), uuid.New()

	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW())`,
		userID, email, "drilluser", "Drill User"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	for _, ch := range []struct {
		id   uuid.UUID
		name string
	}{{allowedID, drill.AllowedChannel}, {forbiddenID, drill.ForbiddenChannel}} {
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
			INSERT INTO channels (id, ch_name, ch_private, created_by)
			VALUES ($1, $2, true, $3)`, ch.id, ch.name, userID); err != nil {
			t.Fatalf("seed channel %s: %v", ch.name, err)
		}
	}

	forbidden := map[string]any{
		"uid": "_:forbidden", "ch_uuid": forbiddenID.String(),
		"ch_name": drill.ForbiddenChannel, "ch_private": true,
	}
	if inForbidden {
		forbidden["ch_members"] = []map[string]any{{"uid": "_:person"}}
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:person", "user_uuid": userID.String(), "user_name": "drilluser"},
		{"uid": "_:allowed", "ch_uuid": allowedID.String(), "ch_name": drill.AllowedChannel,
			"ch_private": true, "ch_members": []map[string]any{{"uid": "_:person"}}},
		forbidden,
	})
	if uids["person"] == "" || uids["forbidden"] == "" {
		t.Fatalf("seeding assigned no uids: %+v", uids)
	}
	return ctx, userID
}

func TestGovernanceDrillRefusesAndRecordsIt(t *testing.T) {
	ctx, userID := drillFixture(t, false)

	person, err := aiBusiness.BuildUserInfoByUserUUID(ctx, userID.String())
	if err != nil {
		t.Fatalf("building the principal the way the executor does: %v", err)
	}

	res, err := drill.Run(ctx, *person)
	if err != nil {
		t.Fatalf("the drill could not run at all: %v", err)
	}

	for i, s := range res.Steps {
		t.Logf("step %d  ok=%-5v  %s%s", i+1, s.OK, s.Name, detailSuffix(s.Detail))
	}
	if !res.Passed {
		t.Fatalf("the drill did not pass on a correct fixture, so either this install " +
			"is not enforcing channel membership or the drill is broken. Steps above.")
	}
	if len(res.Steps) != 5 {
		t.Fatalf("expected all five steps, got %d; an early return means something above "+
			"stopped the run", len(res.Steps))
	}

	// The refusal must come from the MEMBERSHIP check, not from some other failure
	// that happens to produce an error. A drill that passes because the channel was
	// missing, or the text empty, proves nothing about permissions.
	if !strings.Contains(res.RefusalReason, "not a member of this channel") {
		t.Fatalf("the post was refused for the wrong reason: %q.\nThe drill is meant to "+
			"demonstrate the membership check; any other refusal makes it a false positive.",
			res.RefusalReason)
	}

	// Both rows, read back from the log rather than constructed, in chain order.
	if len(res.Rows) != 2 {
		t.Fatalf("expected the attempt and the refusal to be readable from the audit log, got %d rows", len(res.Rows))
	}
	if got := res.Rows[0].Action; got != "agent.drill.attempt" {
		t.Errorf("first recorded row is %q; the attempt must be written BEFORE the outcome", got)
	}
	if got := res.Rows[1].Action; got != "agent.drill.refused" {
		t.Errorf("second recorded row is %q, want agent.drill.refused", got)
	}
	if res.Rows[1].Seq <= res.Rows[0].Seq {
		t.Errorf("the outcome row (seq %d) does not follow the attempt row (seq %d); "+
			"the ordering guarantee is about the LOG, not just the code",
			res.Rows[1].Seq, res.Rows[0].Seq)
	}
	for _, r := range res.Rows {
		if r.EntryHash == "" {
			t.Errorf("row %d (%s) carries no chain hash, so nothing about it is tamper-evident", r.Seq, r.Action)
		}
	}
	if !res.ChainOK || res.ChainChecked < 2 {
		t.Errorf("the chain did not verify over at least the two rows just written "+
			"(ok=%v checked=%d msg=%q)", res.ChainOK, res.ChainChecked, res.ChainMessage)
	}
}

// A drill that cannot fail is a rubber stamp, and a rubber stamp on a compliance
// claim is worse than making no claim. Here the fixture is wrong in the one way
// that would make every later step meaningless: the person IS in the channel.
func TestGovernanceDrillRefusesToPassOnADriftedFixture(t *testing.T) {
	ctx, userID := drillFixture(t, true)

	person, err := aiBusiness.BuildUserInfoByUserUUID(ctx, userID.String())
	if err != nil {
		t.Fatalf("building the principal: %v", err)
	}

	res, err := drill.Run(ctx, *person)
	if err != nil {
		t.Fatalf("the drill errored instead of reporting a finding: %v", err)
	}

	if res.Passed {
		t.Fatal("the drill reported PASSED while the person was a member of the channel " +
			"it claims to be refused from. It is a rubber stamp.")
	}
	if len(res.Steps) == 0 {
		t.Fatal("no steps reported, so the result says nothing about why it failed")
	}
	first := res.Steps[0]
	if first.OK {
		t.Errorf("the membership precondition reported ok=true for somebody who IS a member")
	}
	if !strings.Contains(first.Detail, "drifted") {
		t.Errorf("the failing step does not say the fixture drifted, so a reader cannot tell "+
			"a broken install from a broken fixture. Detail: %q", first.Detail)
	}
	// And it must stop there: attempting the post would write an audit row saying a
	// refusal was expected, on a fixture where nothing was.
	if len(res.Steps) > 1 {
		t.Errorf("the drill carried on past a drifted fixture (%d steps); every step after "+
			"the first would be testing nothing", len(res.Steps))
	}
}

func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return "  -- " + detail
}
