//go:build integration

package business

// Invoices against Postgres 12 with every migration: a draft is saved,
// changed, sent, paid, taken back, voided; numbers stay unique.
// Run: go test -tags=integration ./business/Invoice/ -v

import (
	"context"
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/Invoice"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestInvoiceLife(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	project, other, by := uuid.New(), uuid.New(), uuid.New()

	draft, err := Create(ctx, project, input(), by)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Status != model.StatusDraft || draft.SentAt != nil || draft.TotalCents != 49867 || len(draft.Lines) != 2 || draft.IssuedOn != "2026-10-01" {
		t.Fatalf("saved: %+v", draft)
	}

	// Numbers are unique across projects, whatever their case.
	again := input()
	again.Number = "ql-0001"
	if _, err := Create(ctx, other, again, by); !errors.Is(err, ErrNumberTaken) {
		t.Errorf("a second QL-0001: %v", err)
	}
	invoices, next, err := List(ctx, project, "Q4 launch")
	if err != nil || len(invoices) != 1 || next != "QL-0002" {
		t.Fatalf("list: %d %s %v", len(invoices), next, err)
	}

	// A draft changes.
	change := input()
	change.Lines = change.Lines[:1]
	change.TaxPercent = 0
	if changed, err := Update(ctx, project, draft.ID, change); err != nil || changed.TotalCents != 31635 || len(changed.Lines) != 1 {
		t.Fatalf("changed: %+v %v", changed, err)
	}

	// Sent, it stays as sent: it can't be changed or deleted.
	sent, err := SetStatus(ctx, project, draft.ID, model.StatusSent)
	if err != nil || sent.SentAt == nil || sent.PaidAt != nil {
		t.Fatalf("sent: %+v %v", sent, err)
	}
	if _, err := Update(ctx, project, draft.ID, change); !errors.Is(err, ErrNotDraft) {
		t.Errorf("changing a sent invoice: %v", err)
	}
	if err := Delete(ctx, project, draft.ID); !errors.Is(err, ErrNotDraft) {
		t.Errorf("deleting a sent invoice: %v", err)
	}

	// Paid, then taken back to sent: unpaid again, still sent when it was.
	paid, err := SetStatus(ctx, project, draft.ID, model.StatusPaid)
	if err != nil || paid.PaidAt == nil || !paid.SentAt.Equal(*sent.SentAt) {
		t.Fatalf("paid: %+v %v", paid, err)
	}
	if back, err := SetStatus(ctx, project, draft.ID, model.StatusSent); err != nil || back.PaidAt != nil || back.SentAt == nil {
		t.Fatalf("back to sent: %+v %v", back, err)
	}

	// Void is final.
	if _, err := SetStatus(ctx, project, draft.ID, model.StatusVoid); err != nil {
		t.Fatal(err)
	}
	var ie *InputError
	if _, err := SetStatus(ctx, project, draft.ID, model.StatusPaid); !errors.As(err, &ie) {
		t.Errorf("a void invoice paid: %v", err)
	}

	// Another project's invoice isn't this project's.
	if _, err := Get(ctx, other, draft.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("seen from another project: %v", err)
	}

	// A draft deletes; its number is free again.
	second := input()
	second.Number = "QL-0002"
	d2, err := Create(ctx, project, second, by)
	if err != nil {
		t.Fatal(err)
	}
	if err := Delete(ctx, project, d2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Get(ctx, project, d2.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted: %v", err)
	}
	if _, next, _ := List(ctx, project, "Q4 launch"); next != "QL-0002" {
		t.Errorf("a deleted draft's number is the next again: %s", next)
	}
}
