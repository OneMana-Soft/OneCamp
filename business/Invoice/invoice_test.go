package business

import (
	"errors"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/Invoice"
)

func input() Input {
	return Input{
		Number: " QL-0001 ", IssuedOn: "2026-10-01", DueOn: "2026-10-16", Currency: "usd",
		Seller: model.Party{Name: "  Acme   Studio ", Payment: "IBAN DE00 1234"},
		Client: model.Party{Name: "Northwind", Address: "1 Main St"},
		Lines: []LineInput{
			{Description: "Write the launch announcement", Hours: 3.333, RateCents: 9500},
			{Description: " Review the pricing page ", Hours: 1.25, RateCents: 8500},
		},
		TaxPercent: 18,
	}
}

func TestCheckWorksOutTotals(t *testing.T) {
	inv, err := Check(input())
	if err != nil {
		t.Fatal(err)
	}
	// 3.33 h × 95.00 = 316.35; 1.25 h × 85.00 = 106.25; 422.60 + 18% (76.068 → 76.07).
	if inv.Lines[0].Hours != 3.33 || inv.Lines[0].AmountCents != 31635 || inv.Lines[1].AmountCents != 10625 {
		t.Errorf("lines: %+v", inv.Lines)
	}
	if inv.SubtotalCents != 42260 || inv.TaxCents != 7607 || inv.TotalCents != 49867 {
		t.Errorf("totals: %d %d %d", inv.SubtotalCents, inv.TaxCents, inv.TotalCents)
	}
	if inv.Number != "QL-0001" || inv.Currency != "USD" || inv.Status != model.StatusDraft || inv.Seller.Name != "Acme Studio" || inv.Lines[1].Description != "Review the pricing page" {
		t.Errorf("tidied: %+v", inv)
	}
}

func TestCheckRefuses(t *testing.T) {
	cases := []struct {
		change func(*Input)
		say    string
	}{
		{func(in *Input) { in.Number = " " }, "number"},
		{func(in *Input) { in.Number = strings.Repeat("9", MaxNumberLength+1) }, "number"},
		{func(in *Input) { in.Status = "paid" }, "draft or sent"},
		{func(in *Input) { in.IssuedOn = "1/10/2026" }, "issued"},
		{func(in *Input) { in.DueOn = "2026-09-30" }, "due before"},
		{func(in *Input) { in.Currency = "dollars" }, "three-letter"},
		{func(in *Input) { in.Lines = nil }, "at least one line"},
		{func(in *Input) { in.Lines[0].Hours = 0 }, "hours"},
		{func(in *Input) { in.Lines[0].Hours = MaxHours + 1 }, "hours"},
		{func(in *Input) { in.Lines[0].RateCents = -1 }, "less than nothing"},
		{func(in *Input) { in.Lines[0].Description = " " }, "Describe"},
		{func(in *Input) { in.TaxPercent = 101 }, "percentage"},
		{func(in *Input) { in.Client.Name = strings.Repeat("n", 201) }, "client"},
		{func(in *Input) { in.Notes = strings.Repeat("n", MaxNotes+1) }, "notes"},
	}
	for _, c := range cases {
		in := input()
		c.change(&in)
		var ie *InputError
		if _, err := Check(in); !errors.As(err, &ie) || !strings.Contains(err.Error(), c.say) {
			t.Errorf("want a message about %q, got %v", c.say, err)
		}
	}
	in := input()
	in.Status = model.StatusSent
	if inv, err := Check(in); err != nil || inv.Status != model.StatusSent {
		t.Errorf("a new invoice can be sent straight away: %v", err)
	}
}

func TestPrefixAndNextNumber(t *testing.T) {
	cases := map[string]string{"Q4 launch": "QL", "Customer onboarding": "CO", "a b c d e": "ABCD", "   ": "INV", "Проект": "INV", "2026 rebrand": "2R"}
	for name, want := range cases {
		if got := Prefix(name); got != want {
			t.Errorf("Prefix(%q) = %q, want %q", name, got, want)
		}
	}
	if got := NextNumber("QL", nil); got != "QL-0001" {
		t.Errorf("the first: %s", got)
	}
	if got := NextNumber("QL", []string{"QL-0002", "ql-0009", "QL-draft", "QLX-0050", "QL-12"}); got != "QL-0013" {
		t.Errorf("after the highest of its own, whatever the case: %s", got)
	}
}
