//go:build integration
// +build integration

package integration_test

// The audit log's order is the order things were recorded in.
//
// Record wrote each row from a goroutine of its own, and a row took its time
// and its place in the chain when that goroutine got the chain's lock, so two
// saves a moment apart could be recorded the other way round: an allow-list
// changed from A to B read as B then A (TestAnAllowListedDomainJoins flaked on
// it). Rows now join the chain in the order they were handed over
// (models/AdminAudit chainWriter.go).
//
// Run: go test -tags=integration ./tests/integration/ -run TestAuditRows -v

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

// probeRow is one row the order tests wrote, in chain order.
type probeRow struct {
	writer, i int
	action    string
	at        time.Time
}

// probeRows reads back the rows the order tests wrote, in chain order.
func probeRows(t *testing.T, ctx context.Context) []probeRow {
	t.Helper()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT action, COALESCE(metadata->>'writer', '0'), metadata->>'i', created_at
		  FROM admin_audit_log WHERE action LIKE '%.order_probe' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []probeRow
	for rows.Next() {
		var r probeRow
		var writer, i string
		if err := rows.Scan(&r.action, &writer, &i, &r.at); err != nil {
			t.Fatal(err)
		}
		r.writer, _ = strconv.Atoi(writer)
		r.i, _ = strconv.Atoi(i)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// verifyChain checks the whole chain links up and covers want rows.
func verifyChain(t *testing.T, ctx context.Context, want int) {
	t.Helper()
	res, err := auditBusiness.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Checked != want {
		t.Errorf("the chain: %s, %d rows checked, want %d", res.Message, res.Checked, want)
	}
}

// Many quick records from one request path, then an agent's: the chain holds
// them in the order they were made, with times that never go backwards, and
// the agent's row, recorded last and waited for, is last.
func TestAuditRowsKeepTheOrderTheyWereRecordedIn(t *testing.T) {
	ctx := auditEnv(t)
	const n = 200
	for i := 0; i < n; i++ {
		r := httptest.NewRequest(http.MethodPost, "/admin/settings", nil)
		auditBusiness.Record(r, "settings.order_probe", auditBusiness.CategorySettings,
			fmt.Sprintf("save %d", i), map[string]interface{}{"i": i})
	}
	if err := auditBusiness.RecordForPrincipal(ctx, nil, "", auditBusiness.ActorAgent, "agent.order_probe",
		auditBusiness.CategoryAgent, "after the saves", map[string]interface{}{"i": n}); err != nil {
		t.Fatalf("the agent's row: %v", err)
	}
	integration.Settle(t)

	got := probeRows(t, ctx)
	if len(got) != n+1 {
		t.Fatalf("%d rows written, want %d", len(got), n+1)
	}
	for k, r := range got {
		if r.i != k {
			t.Fatalf("row %d of the chain is the record made %d-th: the log's order isn't the order they were made in", k, r.i)
		}
		if k > 0 && r.at.Before(got[k-1].at) {
			t.Fatalf("row %d is timed %s, before the row ahead of it (%s)", k, r.at, got[k-1].at)
		}
	}
	if got[n].action != "agent.order_probe" {
		t.Errorf("the last row is %s, want the agent's", got[n].action)
	}
	verifyChain(t, ctx, n+1)
}

// Several callers at once, Record and RecordForPrincipal mixed: one linear
// chain that verifies, each caller's rows in the order it made them, and times
// that follow the chain.
func TestAuditRowsFromManyCallersAtOnceMakeOneChain(t *testing.T) {
	ctx := auditEnv(t)
	const writers, each = 8, 40
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 1; w <= writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				meta := map[string]interface{}{"writer": w, "i": i}
				if i%5 == 4 {
					if err := auditBusiness.RecordForPrincipal(ctx, nil, "", auditBusiness.ActorAgent, "agent.order_probe",
						auditBusiness.CategoryAgent, "an agent's", meta); err != nil {
						errs <- err
					}
					continue
				}
				r := httptest.NewRequest(http.MethodPost, "/admin/settings", nil)
				auditBusiness.Record(r, "settings.order_probe", auditBusiness.CategorySettings, "a save", meta)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("an agent's row: %v", err)
	}
	integration.Settle(t)

	got := probeRows(t, ctx)
	if len(got) != writers*each {
		t.Fatalf("%d rows written, want %d", len(got), writers*each)
	}
	next := map[int]int{}
	for k, r := range got {
		if r.i != next[r.writer] {
			t.Fatalf("caller %d's record %d is in the chain where its record %d belongs", r.writer, r.i, next[r.writer])
		}
		next[r.writer]++
		if k > 0 && r.at.Before(got[k-1].at) {
			t.Fatalf("row %d is timed %s, before the row ahead of it (%s)", k, r.at, got[k-1].at)
		}
	}
	verifyChain(t, ctx, writers*each)
}
