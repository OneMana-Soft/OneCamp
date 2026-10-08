// Package models (Invoice) stores the invoices a project's admins save
// (migration 194): numbered, with their lines as billed and their state.
// Queries are scoped to a project.
package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// The states an invoice is in.
const (
	StatusDraft = "draft"
	StatusSent  = "sent"
	StatusPaid  = "paid"
	StatusVoid  = "void"
)

// Party is the seller or the client, as the invoice names them.
type Party struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	TaxID   string `json:"tax_id,omitempty"`
	Payment string `json:"payment,omitempty"` // how to pay: a bank account, a UPI id
	Email   string `json:"email,omitempty"`
}

// Line is one line as billed: hours at a rate, in hundredths of the currency.
type Line struct {
	Description string  `json:"description"`
	Hours       float64 `json:"hours"`
	RateCents   int64   `json:"rate_cents"`
	AmountCents int64   `json:"amount_cents"`
}

// Invoice is one saved invoice. Dates a person picks (issued, due) are days,
// "2006-01-02"; the billed period is the moments its time report covered.
type Invoice struct {
	ID            uuid.UUID  `json:"id"`
	ProjectID     uuid.UUID  `json:"project_id"`
	Number        string     `json:"number"`
	Status        string     `json:"status"`
	IssuedOn      string     `json:"issued_on"`
	DueOn         string     `json:"due_on"`
	PeriodFrom    *time.Time `json:"period_from,omitempty"`
	PeriodTo      *time.Time `json:"period_to,omitempty"`
	Currency      string     `json:"currency"`
	Seller        Party      `json:"seller"`
	Client        Party      `json:"client"`
	Lines         []Line     `json:"lines"`
	SubtotalCents int64      `json:"subtotal_cents"`
	TaxPercent    float64    `json:"tax_percent"`
	TaxCents      int64      `json:"tax_cents"`
	TotalCents    int64      `json:"total_cents"`
	Notes         string     `json:"notes"`
	SentAt        *time.Time `json:"sent_at,omitempty"`
	PaidAt        *time.Time `json:"paid_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

var (
	ErrNotFound    = errors.New("invoice not found")
	ErrNumberTaken = errors.New("an invoice with that number already exists")
)

const columns = `id, project_id, number, status, to_char(issued_on, 'YYYY-MM-DD'), to_char(due_on, 'YYYY-MM-DD'),
	period_from, period_to, currency, seller, client, lines, subtotal_cents, tax_percent::float8, tax_cents,
	total_cents, notes, sent_at, paid_at, created_at, updated_at`

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

func scan(row interface{ Scan(...any) error }) (*Invoice, error) {
	var inv Invoice
	var seller, client, lines []byte
	var from, to, sent, paid sql.NullTime
	err := row.Scan(&inv.ID, &inv.ProjectID, &inv.Number, &inv.Status, &inv.IssuedOn, &inv.DueOn,
		&from, &to, &inv.Currency, &seller, &client, &lines, &inv.SubtotalCents, &inv.TaxPercent, &inv.TaxCents,
		&inv.TotalCents, &inv.Notes, &sent, &paid, &inv.CreatedAt, &inv.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	for _, p := range []struct {
		raw []byte
		to  any
	}{{seller, &inv.Seller}, {client, &inv.Client}, {lines, &inv.Lines}} {
		if err := json.Unmarshal(p.raw, p.to); err != nil {
			return nil, err
		}
	}
	at := func(t sql.NullTime) *time.Time {
		if !t.Valid {
			return nil
		}
		v := t.Time
		return &v
	}
	inv.PeriodFrom, inv.PeriodTo, inv.SentAt, inv.PaidAt = at(from), at(to), at(sent), at(paid)
	if inv.Lines == nil {
		inv.Lines = []Line{}
	}
	return &inv, nil
}

// List is a project's invoices, the latest first.
func List(ctx context.Context, projectID uuid.UUID) ([]*Invoice, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `SELECT `+columns+` FROM invoices WHERE project_id = $1 ORDER BY issued_on DESC, created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Invoice{}
	for rows.Next() {
		inv, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// Get is one of a project's invoices.
func Get(ctx context.Context, projectID, id uuid.UUID) (*Invoice, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(c, `SELECT `+columns+` FROM invoices WHERE id = $1 AND project_id = $2`, id, projectID))
}

// Numbers is every invoice number that starts with prefix, whatever its case.
func Numbers(ctx context.Context, prefix string) ([]string, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `SELECT number FROM invoices WHERE lower(number) LIKE lower($1) || '%'`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func encode(inv *Invoice) (seller, client, lines []byte, err error) {
	if seller, err = json.Marshal(inv.Seller); err != nil {
		return
	}
	if client, err = json.Marshal(inv.Client); err != nil {
		return
	}
	if inv.Lines == nil {
		inv.Lines = []Line{}
	}
	lines, err = json.Marshal(inv.Lines)
	return
}

// Create saves a new invoice, a draft or already sent.
func Create(ctx context.Context, inv *Invoice, by uuid.UUID) (*Invoice, error) {
	seller, client, lines, err := encode(inv)
	if err != nil {
		return nil, err
	}
	c, cancel := withTimeout(ctx)
	defer cancel()
	out, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		INSERT INTO invoices (id, project_id, number, status, issued_on, due_on, period_from, period_to, currency,
			seller, client, lines, subtotal_cents, tax_percent, tax_cents, total_cents, notes, sent_at, paid_at, created_by)
		VALUES ($1, $2, $3, $4::text, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
			CASE WHEN $4::text IN ('sent', 'paid') THEN NOW() END, CASE WHEN $4::text = 'paid' THEN NOW() END, $18)
		RETURNING `+columns,
		uuid.New(), inv.ProjectID, inv.Number, inv.Status, inv.IssuedOn, inv.DueOn, inv.PeriodFrom, inv.PeriodTo, inv.Currency,
		seller, client, lines, inv.SubtotalCents, inv.TaxPercent, inv.TaxCents, inv.TotalCents, inv.Notes, by))
	if helpers.IsUniqueViolation(err) {
		return nil, ErrNumberTaken
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Invoice Create err: %+v", err)
	}
	return out, err
}

// Update changes what a draft says. A draft only: an invoice once sent is as
// it was sent. ErrNotFound when there's no draft of that id in the project.
func Update(ctx context.Context, inv *Invoice) (*Invoice, error) {
	seller, client, lines, err := encode(inv)
	if err != nil {
		return nil, err
	}
	c, cancel := withTimeout(ctx)
	defer cancel()
	out, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		UPDATE invoices SET number = $3, issued_on = $4, due_on = $5, period_from = $6, period_to = $7, currency = $8,
			seller = $9, client = $10, lines = $11, subtotal_cents = $12, tax_percent = $13, tax_cents = $14,
			total_cents = $15, notes = $16, updated_at = NOW()
		WHERE id = $1 AND project_id = $2 AND status = 'draft'
		RETURNING `+columns,
		inv.ID, inv.ProjectID, inv.Number, inv.IssuedOn, inv.DueOn, inv.PeriodFrom, inv.PeriodTo, inv.Currency,
		seller, client, lines, inv.SubtotalCents, inv.TaxPercent, inv.TaxCents, inv.TotalCents, inv.Notes))
	if helpers.IsUniqueViolation(err) {
		return nil, ErrNumberTaken
	}
	return out, err
}

// SetStatus moves an invoice to a state, keeping when it was first sent and
// when it was paid: a paid invoice taken back to sent is unpaid again; one
// taken back to draft was never sent. A void invoice stays void.
func SetStatus(ctx context.Context, projectID, id uuid.UUID, status string) (*Invoice, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		UPDATE invoices SET status = $3::text,
			sent_at = CASE WHEN $3::text = 'draft' THEN NULL WHEN $3::text IN ('sent', 'paid') THEN COALESCE(sent_at, NOW()) ELSE sent_at END,
			paid_at = CASE WHEN $3::text = 'paid' THEN COALESCE(paid_at, NOW()) WHEN $3::text = 'void' THEN paid_at ELSE NULL END,
			updated_at = NOW()
		WHERE id = $1 AND project_id = $2 AND status <> 'void'
		RETURNING `+columns, id, projectID, status))
}

// Delete removes a draft. A sent invoice is voided, not deleted, so its
// number is never given to another.
func Delete(ctx context.Context, projectID, id uuid.UUID) error {
	c, cancel := withTimeout(ctx)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(c, `DELETE FROM invoices WHERE id = $1 AND project_id = $2 AND status = 'draft'`, id, projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
