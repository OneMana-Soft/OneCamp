//go:build integration
// +build integration

package integration_test

// "AI activity on this doc" (docs programme B9): doc AI's rows in the record are
// read by their doc, newest first, through the partial index the doc AI index
// migration adds, and that migration goes up, down and up again on Postgres 12.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestDocAI' -v

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
)

// docAIIndexMigration is the doc AI index migration's number, found by name so
// a renumbering at merge needs no edit here, and the number below it.
func docAIIndexMigration(t *testing.T, dir string) (mine, before uint) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var all []uint
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		n, err := strconv.ParseUint(strings.SplitN(name, "_", 2)[0], 10, 32)
		if err != nil {
			continue
		}
		all = append(all, uint(n))
		if strings.HasSuffix(name, "_audit_doc_ai_index.up.sql") {
			mine = uint(n)
		}
	}
	if mine == 0 {
		t.Fatal("no *_audit_doc_ai_index.up.sql among the migrations")
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	for _, n := range all {
		if n < mine {
			before = n
		}
	}
	return mine, before
}

func TestDocAIIndexMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(here), "..", "..", "migrations")
	mine, before := docAIIndexMigration(t, dir)
	m, err := migrate.New("file://"+dir, env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	present := func() bool {
		var n int
		if err := env.PG.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_admin_audit_log_doc_ai'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if v, _, _ := m.Version(); v < mine || !present() {
		t.Fatalf("at %d the index is missing (want it from %d)", v, mine)
	}
	if err := m.Migrate(before); err != nil {
		t.Fatalf("down past %d: %v", mine, err)
	}
	if present() {
		t.Fatal("the index is still there after down")
	}
	if err := m.Migrate(mine); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if !present() {
		t.Fatal("the index is missing after up again")
	}
}

func TestDocAIActivityIsReadByItsDoc(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatalf("wire the pool: %v", err)
	}
	ctx := context.Background()
	docA, docB := uuid.NewString(), uuid.NewString()
	record := func(action, doc string) {
		t.Helper()
		if err := auditBusiness.RecordForPrincipal(ctx, nil, "sam@example.test", auditBusiness.ActorHuman, action,
			auditBusiness.CategoryAgent, "row "+action, map[string]interface{}{"doc_uuid": doc}); err != nil {
			t.Fatalf("record %s: %v", action, err)
		}
	}
	record("ai.doc.edit.accepted", docA)
	record("ai.doc.edit.accepted", docB)
	record("ai.answer.given", docA) // AI, but not doc AI
	record("ai.doc.agent_edit.allowed", docA)

	rows, err := auditModel.ListDocAIEntries(ctx, docA, "ai.doc.", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Action != "ai.doc.agent_edit.allowed" || rows[1].Action != "ai.doc.edit.accepted" || rows[0].Seq <= rows[1].Seq {
		var got []string
		for _, r := range rows {
			got = append(got, r.Action)
		}
		t.Fatalf("doc A's rows, newest first: %v", got)
	}
	if rows, _ := auditModel.ListDocAIEntries(ctx, docA, "ai.doc.agent_edit.", 50); len(rows) != 1 {
		t.Errorf("by a narrower prefix: %d rows", len(rows))
	}

	// The partial index answers the read, so a long record is not walked.
	tx, err := env.PG.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	plan, err := tx.QueryContext(ctx, `EXPLAIN SELECT seq FROM admin_audit_log
		WHERE action LIKE 'ai.doc.%' AND action LIKE $1 AND (metadata ->> 'doc_uuid') = $2
		ORDER BY seq DESC LIMIT 50`, "ai.doc.%", docA)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	var lines []string
	for plan.Next() {
		var l string
		_ = plan.Scan(&l)
		lines = append(lines, l)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "idx_admin_audit_log_doc_ai") {
		t.Errorf("the read does not use the doc AI index:\n%s", strings.Join(lines, "\n"))
	}
}
