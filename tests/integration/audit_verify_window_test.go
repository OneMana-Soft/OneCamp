//go:build integration
// +build integration

package integration_test

// Verifying a WINDOW of the chain instead of all of it.
//
// WHY THE WINDOW EXISTS. Verify walks every hashed row. That is the right answer
// for an auditor and the wrong one for anything on a request path: the log only
// grows, so a call that is instant on a fresh install becomes a slow query and
// then a gateway timeout on a workspace that has been running a year. The
// governance drill verifies the chain as its final step and is clicked from a
// browser, so it needs an answer bounded by something other than the customer's
// age. The roadmap named this risk before the drill was built.
//
// WHY IT HAS TO SAY SO. A window seeds from the stored prev_hash of its earliest
// row rather than from genesis, so it proves the links inside the window and takes
// that one value on trust. It cannot see tampering before the window. "The last
// 500 entries verify" and "the log has not been altered" are different claims, and
// a compliance feature that quietly makes the larger one is the exact failure it
// exists to prevent. So these tests check the disclosure as hard as the result.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAuditVerifyWindow -v

import (
	"context"
	"fmt"
	"testing"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// seedAuditRows writes n chained entries as a real principal and returns its id.
func seedAuditRows(t *testing.T, ctx context.Context, n int) uuid.UUID {
	t.Helper()
	actor := uuid.New()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1,$2,$3,$4,NOW(),NOW())`,
		actor, "audit-"+actor.String()[:8]+"@example.test", "auditor", "Auditor"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	for i := 0; i < n; i++ {
		if err := auditBusiness.RecordForPrincipal(ctx, &actor, "audit@example.test",
			auditBusiness.ActorAgent, "agent.drill.attempt", auditBusiness.CategoryAgent,
			fmt.Sprintf("entry %d", i),
			map[string]interface{}{"i": i, "channel": "drill-finance", "phase": "before"}); err != nil {
			t.Fatalf("seed audit row %d: %v", i, err)
		}
	}
	return actor
}

func auditEnv(t *testing.T) context.Context {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatalf("wire the pool: %v", err)
	}
	return context.Background()
}

func TestAuditVerifyWindowChecksOnlyTheWindowAndSaysSo(t *testing.T) {
	ctx := auditEnv(t)
	seedAuditRows(t, ctx, 40)

	res, err := auditBusiness.VerifyRecent(ctx, 10)
	if err != nil {
		t.Fatalf("verify recent: %v", err)
	}
	if !res.OK {
		t.Fatalf("a window of an untouched chain did not verify: %s", res.Message)
	}
	if res.Checked != 10 {
		t.Errorf("checked %d rows, want exactly the 10 asked for; a window that quietly reads "+
			"more is the cost this exists to avoid", res.Checked)
	}
	if !res.Partial {
		t.Error("the result does not declare itself partial, so a caller will render " +
			"'the last 10 entries verify' as 'the log has not been altered'")
	}
	if res.FromSeq == 0 {
		t.Error("the result does not say where the window began, so nobody can tell what was " +
			"NOT checked")
	}
	if res.Message == "audit chain intact" {
		t.Errorf("a windowed result reports the same message as a full verification (%q); "+
			"the two claims must not be indistinguishable", res.Message)
	}
}

// A window wide enough to reach the first entry IS a full verification, and must
// stop warning about a limit it never hit.
func TestAuditVerifyWindowReachingTheStartIsNotPartial(t *testing.T) {
	ctx := auditEnv(t)
	seedAuditRows(t, ctx, 5)

	res, err := auditBusiness.VerifyRecent(ctx, 500)
	if err != nil {
		t.Fatalf("verify recent: %v", err)
	}
	if !res.OK || res.Checked != 5 {
		t.Fatalf("expected all 5 rows to verify, got ok=%v checked=%d (%s)", res.OK, res.Checked, res.Message)
	}
	if res.Partial {
		t.Error("the window covered the entire log and still reported itself partial, which " +
			"understates a result that is in fact complete")
	}
}

// The window must still catch tampering inside it, or it is decoration.
func TestAuditVerifyWindowCatchesTamperingInsideIt(t *testing.T) {
	ctx := auditEnv(t)
	seedAuditRows(t, ctx, 30)

	// Edit a row that sits inside the last 10 by sequence.
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE admin_audit_log SET summary = 'edited after the fact'
		WHERE seq = (SELECT max(seq) - 3 FROM admin_audit_log)`); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	res, err := auditBusiness.VerifyRecent(ctx, 10)
	if err != nil {
		t.Fatalf("verify recent: %v", err)
	}
	if res.OK {
		t.Fatal("a row inside the window was edited and the window still reported the chain intact")
	}
	if res.FirstBadSeq == nil {
		t.Error("the window did not name the first bad entry, so nobody can find it")
	}
}

// And tampering OUTSIDE the window must not be reported as verified. The window
// cannot detect it -- that is the honest limit -- so the result must not claim
// completeness.
func TestAuditVerifyWindowDoesNotClaimToCoverWhatItSkipped(t *testing.T) {
	ctx := auditEnv(t)
	seedAuditRows(t, ctx, 30)

	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE admin_audit_log SET summary = 'edited long ago' WHERE seq = 2`); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	windowed, err := auditBusiness.VerifyRecent(ctx, 5)
	if err != nil {
		t.Fatalf("verify recent: %v", err)
	}
	if !windowed.Partial {
		t.Fatal("the window did not declare itself partial while an edit sat outside it; a " +
			"reader would take this as proof the log is clean")
	}

	// The full walk is what finds it, which is why the UI must offer one.
	full, err := auditBusiness.Verify(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if full.OK {
		t.Fatal("the full verification missed an edited historical row")
	}
	if full.Partial {
		t.Error("a full verification declared itself partial")
	}
}
