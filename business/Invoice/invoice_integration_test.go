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
	if err := Delete(ctx, project, draft.ID, by); !errors.Is(err, ErrNotDraft) {
		t.Errorf("deleting a sent invoice: %v", err)
	}

	// Taken back to draft to fix a line: its lines change, its number doesn't,
	// and it can't be deleted, so the number is never given to another.
	redraft, err := SetStatus(ctx, project, draft.ID, model.StatusDraft)
	if err != nil || redraft.SentAt != nil || redraft.FirstSentAt == nil {
		t.Fatalf("back to draft: %+v %v", redraft, err)
	}
	renumbered := input()
	renumbered.Number = "QL-0099"
	if _, err := Update(ctx, project, draft.ID, renumbered); !errors.Is(err, ErrWasSent) {
		t.Errorf("renumbering a draft that was sent: %v, want ErrWasSent", err)
	}
	if fixed, err := Update(ctx, project, draft.ID, input()); err != nil || fixed.Number != "QL-0001" || len(fixed.Lines) != 2 {
		t.Errorf("fixing a draft that was sent, number kept: %+v %v", fixed, err)
	}
	if err := Delete(ctx, project, draft.ID, by); !errors.Is(err, ErrWasSent) {
		t.Errorf("deleting a draft that was sent: %v, want ErrWasSent", err)
	}
	if _, next, _ := List(ctx, project, "Q4 launch"); next != "QL-0002" {
		t.Errorf("the next number after a sent one: %s", next)
	}
	if again, err := SetStatus(ctx, project, draft.ID, model.StatusSent); err != nil || !again.FirstSentAt.Equal(*redraft.FirstSentAt) {
		t.Fatalf("sent again, first sent when it was: %+v %v", again, err)
	}
	sent, _ = Get(ctx, project, draft.ID)

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
	if err := Delete(ctx, project, d2.ID, by); err != nil {
		t.Fatal(err)
	}
	if _, err := Get(ctx, project, d2.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted: %v", err)
	}
	if _, next, _ := List(ctx, project, "Q4 launch"); next != "QL-0002" {
		t.Errorf("a deleted draft's number is the next again: %s", next)
	}

	// V-78: the delete was soft. Another draft takes QL-0002, so the deleted
	// one's restore brings it back as the next number, with its lines.
	d3, err := Create(ctx, project, second, by)
	if err != nil || d3.Number != "QL-0002" {
		t.Fatalf("the next draft: %+v %v", d3, err)
	}
	if err := Restore(ctx, d2.ID); err != nil {
		t.Fatal(err)
	}
	back, err := Get(ctx, project, d2.ID)
	if err != nil || back.Number != "QL-0003" || back.TotalCents != d2.TotalCents {
		t.Fatalf("the restored draft: %+v %v", back, err)
	}
}

// ADMIN-17, Stripe's rule: void only from sent. A paid invoice is marked
// unpaid first, so money that arrived is never voided by one click; the
// refusal says what to do, and the invoice stays paid.
func TestPaidInvoiceCannotBeVoided(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	project, by := uuid.New(), uuid.New()
	inv, err := Create(ctx, project, input(), by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetStatus(ctx, project, inv.ID, model.StatusSent); err != nil {
		t.Fatal(err)
	}
	if _, err := SetStatus(ctx, project, inv.ID, model.StatusPaid); err != nil {
		t.Fatal(err)
	}
	var ie *InputError
	if _, err := SetStatus(ctx, project, inv.ID, model.StatusVoid); !errors.As(err, &ie) || ie.Error() != "A paid invoice is marked unpaid before it is voided." {
		t.Fatalf("voiding a paid invoice: %v", err)
	}
	if still, err := Get(ctx, project, inv.ID); err != nil || still.Status != model.StatusPaid || still.PaidAt == nil {
		t.Fatalf("after the refusal it is still paid: %+v %v", still, err)
	}
	// Marked unpaid, it voids.
	if _, err := SetStatus(ctx, project, inv.ID, model.StatusSent); err != nil {
		t.Fatal(err)
	}
	if v, err := SetStatus(ctx, project, inv.ID, model.StatusVoid); err != nil || v.Status != model.StatusVoid {
		t.Fatalf("voiding it once unpaid: %+v %v", v, err)
	}
}

// ADMIN-17: void only from sent. A draft is deleted, not voided; a draft that
// was sent before keeps its number, so it is marked sent and voided from there.
func TestDraftIsNotVoided(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	project, by := uuid.New(), uuid.New()
	draft, err := Create(ctx, project, input(), by)
	if err != nil {
		t.Fatal(err)
	}
	var ie *InputError
	if _, err := SetStatus(ctx, project, draft.ID, model.StatusVoid); !errors.As(err, &ie) || ie.Error() != "A draft is deleted rather than voided." {
		t.Fatalf("voiding a draft: %v", err)
	}
	if still, err := Get(ctx, project, draft.ID); err != nil || still.Status != model.StatusDraft {
		t.Fatalf("after the refusal it is still a draft: %+v %v", still, err)
	}
	// Sent, then taken back to a draft: it can't be deleted, nor voided as a
	// draft; the refusal says how to void it.
	if _, err := SetStatus(ctx, project, draft.ID, model.StatusSent); err != nil {
		t.Fatal(err)
	}
	if _, err := SetStatus(ctx, project, draft.ID, model.StatusDraft); err != nil {
		t.Fatal(err)
	}
	if _, err := SetStatus(ctx, project, draft.ID, model.StatusVoid); !errors.As(err, &ie) || ie.Error() != "This draft was sent before, so it keeps its number. Mark it sent, then void it." {
		t.Fatalf("voiding a draft that was sent: %v", err)
	}
	if err := Delete(ctx, project, draft.ID, by); !errors.Is(err, ErrWasSent) {
		t.Fatalf("deleting a draft that was sent: %v, want ErrWasSent", err)
	}
	if _, err := SetStatus(ctx, project, draft.ID, model.StatusSent); err != nil {
		t.Fatal(err)
	}
	if v, err := SetStatus(ctx, project, draft.ID, model.StatusVoid); err != nil || v.Status != model.StatusVoid {
		t.Fatalf("voiding it once sent again: %+v %v", v, err)
	}
}

// Only a move to void needs the invoice sent; every other move keeps its old
// path. A draft paid straight away is paid and counts as sent (it has both
// sent times), and a paid invoice taken back to a draft moves, losing its
// sent and paid times but keeping when it was first sent.
func TestNonVoidMoveKeepsItsPath(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	project, by := uuid.New(), uuid.New()
	draft, err := Create(ctx, project, input(), by)
	if err != nil {
		t.Fatal(err)
	}
	paid, err := SetStatus(ctx, project, draft.ID, model.StatusPaid)
	if err != nil || paid.Status != model.StatusPaid || paid.PaidAt == nil || paid.SentAt == nil || paid.FirstSentAt == nil {
		t.Fatalf("a draft paid straight away: %+v %v", paid, err)
	}
	back, err := SetStatus(ctx, project, draft.ID, model.StatusDraft)
	if err != nil || back.Status != model.StatusDraft || back.PaidAt != nil || back.SentAt != nil || back.FirstSentAt == nil || !back.FirstSentAt.Equal(*paid.FirstSentAt) {
		t.Fatalf("paid, taken back to a draft: %+v %v", back, err)
	}
}
