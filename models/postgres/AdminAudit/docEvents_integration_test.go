//go:build integration

package models

// A doc's changes in the record against Postgres 12 with every migration
// (docs C6-3): one doc's "docs" rows, newest first, at most the limit, none
// of another doc's, none of another category's, none retention redacted; and
// migration 470's index is there to answer it.
// Run: go test -tags=integration ./models/postgres/AdminAudit/ -run TestADocsChanges -v

import (
	"context"
	"fmt"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestADocsChangesAreReadNewestFirst(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	doc, other := uuid.NewString(), uuid.NewString()
	row := func(category, action, resource string) {
		t.Helper()
		meta := fmt.Sprintf(`{"resource_kind":"doc","resource_uuid":%q}`, resource)
		if err := Insert(ctx, &AuditEntry{Action: action, Category: category, Summary: action + " it", Metadata: &meta, ActorKind: "human"}); err != nil {
			t.Fatal(err)
		}
	}
	row(CategoryDocs, "doc.shared", doc)
	row(CategoryDocs, "doc.locked", other)
	row("auth", "auth.login", doc)
	row(CategoryDocs, "doc.published", doc)
	row(CategoryDocs, "doc.exported", doc)

	got, err := ListDocEvents(ctx, doc, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Action != "doc.exported" || got[1].Action != "doc.published" {
		t.Fatalf("newest two: %+v", got)
	}
	if _, err := env.PG.Exec(`UPDATE admin_audit_log SET redacted_at = NOW() WHERE action = 'doc.exported'`); err != nil {
		t.Fatal(err)
	}
	got, err = ListDocEvents(ctx, doc, 30)
	if err != nil || len(got) != 2 || got[0].Action != "doc.published" || got[1].Action != "doc.shared" {
		t.Fatalf("after a redaction: %+v %v", got, err)
	}
	var index string
	if err := env.PG.QueryRow(`SELECT indexname FROM pg_indexes WHERE indexname = 'idx_admin_audit_log_doc_events'`).Scan(&index); err != nil {
		t.Fatalf("migration 470's index: %v", err)
	}
}
