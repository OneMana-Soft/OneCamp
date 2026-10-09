// Package business (Invoice) is the invoices a project's admins save from its
// billable time: numbered, with each line's hours and rate as billed, totals
// worked out here so they always multiply out, and a state (draft, sent,
// paid, void) so what's owed, and what's late, can be seen. A draft can be
// changed or deleted; once sent, an invoice stays as it was sent and is
// voided rather than deleted, so its number is never given to another.
// Money is in hundredths of the currency's unit (migration 194).
package business

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	model "github.com/akashc777/OneCamp/models/postgres/Invoice"
	"github.com/google/uuid"
)

// Limits on what an invoice holds.
const (
	MaxNumberLength = 40
	MaxLines        = 500
	MaxDescription  = 300
	MaxNotes        = 4000
	MaxHours        = 100000
	// MaxCents bounds a rate, an amount and a total alike.
	MaxCents = 1e15
)

// InputError is something about an invoice its maker can fix; its message
// says what.
type InputError struct{ msg string }

func (e *InputError) Error() string { return e.msg }

func invalid(format string, a ...any) error { return &InputError{fmt.Sprintf(format, a...)} }

var (
	ErrNotFound    = model.ErrNotFound
	ErrNumberTaken = model.ErrNumberTaken
	// ErrNotDraft is changing or deleting an invoice that has been sent.
	ErrNotDraft = errors.New("only a draft can be changed")
	// ErrWasSent is renumbering or deleting a draft that was sent before:
	// its number is spoken for.
	ErrWasSent = errors.New("an invoice once sent keeps its number")
)

// LineInput is one line as the app sends it; its amount is worked out here.
type LineInput struct {
	Description string  `json:"description"`
	Hours       float64 `json:"hours"`
	RateCents   int64   `json:"rate_cents"`
}

// Input is an invoice as the app sends it.
type Input struct {
	Number     string      `json:"number"`
	Status     string      `json:"status"` // on creating: draft (the default) or sent
	IssuedOn   string      `json:"issued_on"`
	DueOn      string      `json:"due_on"`
	PeriodFrom *time.Time  `json:"period_from"`
	PeriodTo   *time.Time  `json:"period_to"`
	Currency   string      `json:"currency"`
	Seller     model.Party `json:"seller"`
	Client     model.Party `json:"client"`
	Lines      []LineInput `json:"lines"`
	TaxPercent float64     `json:"tax_percent"`
	Notes      string      `json:"notes"`
}

func tidy(s string) string { return strings.Join(strings.Fields(s), " ") }

func day(s, what string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return t, invalid("Choose the day it's %s.", what)
	}
	return t, nil
}

// cents is a line's amount: its hours, as shown to two places, times its
// rate, rounded half away from zero, as the invoice page works it out.
func cents(hours float64, rateCents int64) int64 {
	return int64(math.Round(hours * float64(rateCents)))
}

func party(p model.Party, who string) (model.Party, error) {
	p = model.Party{Name: tidy(p.Name), Address: strings.TrimSpace(p.Address), TaxID: tidy(p.TaxID), Payment: strings.TrimSpace(p.Payment), Email: strings.TrimSpace(p.Email)}
	if utf8.RuneCountInString(p.Name) > 200 || utf8.RuneCountInString(p.Address) > 1000 || utf8.RuneCountInString(p.TaxID) > 100 ||
		utf8.RuneCountInString(p.Payment) > 1000 || utf8.RuneCountInString(p.Email) > 254 {
		return p, invalid("The %s's details are longer than an invoice holds.", who)
	}
	return p, nil
}

// Check is an invoice as stored, totals worked out, or what to fix. Pure.
func Check(in Input) (*model.Invoice, error) {
	inv := &model.Invoice{Number: tidy(in.Number), Status: in.Status, Notes: strings.TrimSpace(in.Notes), PeriodFrom: in.PeriodFrom, PeriodTo: in.PeriodTo}
	if inv.Number == "" || utf8.RuneCountInString(inv.Number) > MaxNumberLength {
		return nil, invalid("Give the invoice a number of up to %d characters.", MaxNumberLength)
	}
	if inv.Status == "" {
		inv.Status = model.StatusDraft
	}
	if inv.Status != model.StatusDraft && inv.Status != model.StatusSent {
		return nil, invalid("A new invoice is a draft or sent.")
	}
	issued, err := day(in.IssuedOn, "issued")
	if err != nil {
		return nil, err
	}
	due, err := day(in.DueOn, "due")
	if err != nil {
		return nil, err
	}
	if due.Before(issued) {
		return nil, invalid("An invoice can't be due before it's issued.")
	}
	inv.IssuedOn, inv.DueOn = issued.Format("2006-01-02"), due.Format("2006-01-02")
	inv.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if len(inv.Currency) != 3 || strings.Trim(inv.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return nil, invalid("Choose the currency by its three-letter code, like INR, USD or EUR.")
	}
	if inv.Seller, err = party(in.Seller, "seller"); err != nil {
		return nil, err
	}
	if inv.Client, err = party(in.Client, "client"); err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(inv.Notes) > MaxNotes {
		return nil, invalid("Keep the notes under %d characters.", MaxNotes)
	}
	if len(in.Lines) == 0 {
		return nil, invalid("An invoice needs at least one line.")
	}
	if len(in.Lines) > MaxLines {
		return nil, invalid("An invoice can have up to %d lines.", MaxLines)
	}
	if math.IsNaN(in.TaxPercent) || in.TaxPercent < 0 || in.TaxPercent > 100 {
		return nil, invalid("Tax is a percentage from 0 to 100.")
	}
	inv.TaxPercent = math.Round(in.TaxPercent*100) / 100
	for _, l := range in.Lines {
		line := model.Line{Description: tidy(l.Description), Hours: math.Round(l.Hours*100) / 100, RateCents: l.RateCents}
		if line.Description == "" || utf8.RuneCountInString(line.Description) > MaxDescription {
			return nil, invalid("Describe each line in up to %d characters.", MaxDescription)
		}
		if math.IsNaN(l.Hours) || line.Hours <= 0 || line.Hours > MaxHours {
			return nil, invalid("Each line needs some hours, up to %d.", MaxHours)
		}
		if line.RateCents < 0 || line.RateCents > MaxCents {
			return nil, invalid("A rate can't be less than nothing.")
		}
		line.AmountCents = cents(line.Hours, line.RateCents)
		inv.Lines = append(inv.Lines, line)
		inv.SubtotalCents += line.AmountCents
		if inv.SubtotalCents > MaxCents {
			return nil, invalid("That's more than an invoice can hold.")
		}
	}
	inv.TaxCents = int64(math.Round(float64(inv.SubtotalCents) * inv.TaxPercent / 100))
	inv.TotalCents = inv.SubtotalCents + inv.TaxCents
	return inv, nil
}

// numbered matches a number made by NextNumber: a prefix, a dash and a count.
var numbered = regexp.MustCompile(`^(.*)-(\d+)$`)

// Prefix is the start of a project's invoice numbers: the first letter or
// digit of each of its words, up to four ("Q4 launch" is QL), or INV. Pure.
func Prefix(project string) string {
	var b strings.Builder
	for _, w := range strings.Fields(project) {
		for _, r := range w {
			if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
			break
		}
	}
	p := strings.ToUpper(b.String())
	if len(p) > 4 {
		p = p[:4]
	}
	if p == "" {
		return "INV"
	}
	return p
}

// NextNumber is the number after the highest of taken with prefix:
// "QL-0001", then "QL-0002". Pure.
func NextNumber(prefix string, taken []string) string {
	highest := 0
	for _, n := range taken {
		m := numbered.FindStringSubmatch(n)
		if m == nil || !strings.EqualFold(m[1], prefix) {
			continue
		}
		if v, err := strconv.Atoi(m[2]); err == nil && v > highest {
			highest = v
		}
	}
	return fmt.Sprintf("%s-%04d", prefix, highest+1)
}

// List is a project's invoices, and the number the next one would take.
func List(ctx context.Context, project uuid.UUID, projectName string) ([]*model.Invoice, string, error) {
	invoices, err := model.List(ctx, project)
	if err != nil {
		return nil, "", err
	}
	prefix := Prefix(projectName)
	taken, err := model.Numbers(ctx, prefix)
	if err != nil {
		return nil, "", err
	}
	return invoices, NextNumber(prefix, taken), nil
}

// Get is one of a project's invoices.
func Get(ctx context.Context, project, id uuid.UUID) (*model.Invoice, error) {
	return model.Get(ctx, project, id)
}

// Create saves an invoice, a draft or already sent.
func Create(ctx context.Context, project uuid.UUID, in Input, by uuid.UUID) (*model.Invoice, error) {
	inv, err := Check(in)
	if err != nil {
		return nil, err
	}
	inv.ProjectID = project
	return model.Create(ctx, inv, by)
}

// Update changes a draft.
func Update(ctx context.Context, project, id uuid.UUID, in Input) (*model.Invoice, error) {
	in.Status = model.StatusDraft
	inv, err := Check(in)
	if err != nil {
		return nil, err
	}
	inv.ID, inv.ProjectID = id, project
	out, err := model.Update(ctx, inv)
	if errors.Is(err, model.ErrNotFound) {
		return nil, notDraft(ctx, project, id)
	}
	return out, err
}

// notDraft says why a draft-only change didn't happen: the invoice is gone,
// it has been sent, or it is a draft that was sent before (whose number
// stays).
func notDraft(ctx context.Context, project, id uuid.UUID) error {
	inv, err := model.Get(ctx, project, id)
	if err != nil {
		return err
	}
	if inv.Status == model.StatusDraft && inv.FirstSentAt != nil {
		return ErrWasSent
	}
	return ErrNotDraft
}

// SetStatus moves an invoice to a state: sent, paid, back to draft or sent,
// or void. A void invoice stays void.
func SetStatus(ctx context.Context, project, id uuid.UUID, status string) (*model.Invoice, error) {
	switch status {
	case model.StatusDraft, model.StatusSent, model.StatusPaid, model.StatusVoid:
	default:
		return nil, invalid("An invoice is a draft, sent, paid or void.")
	}
	out, err := model.SetStatus(ctx, project, id, status)
	if errors.Is(err, model.ErrNotFound) {
		if inv, getErr := model.Get(ctx, project, id); getErr == nil && inv.Status == model.StatusVoid {
			return nil, invalid("A void invoice stays void. Make a new one instead.")
		}
	}
	return out, err
}

// Delete removes a draft.
func Delete(ctx context.Context, project, id uuid.UUID) error {
	err := model.Delete(ctx, project, id)
	if errors.Is(err, model.ErrNotFound) {
		return notDraft(ctx, project, id)
	}
	return err
}
