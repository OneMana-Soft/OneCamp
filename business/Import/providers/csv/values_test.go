package csv

import (
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// A column's values suggest its field: our export's kinds read back as they
// were, and anything else is text.
func TestAColumnsValuesSuggestItsKind(t *testing.T) {
	for _, c := range []struct {
		values   []string
		currency string
		want     string
	}{
		{[]string{"12.5", "1250.50"}, "USD", importProvider.FieldMoney},
		{[]string{"3", "-1.5", "1,250"}, "", importProvider.FieldNumber},
		{[]string{"2026-10-24", "2026-11-01"}, "", importProvider.FieldDate},
		{[]string{"Yes", "Yes"}, "", importProvider.FieldCheckbox},
		{[]string{"No", "true"}, "", importProvider.FieldCheckbox},
		{[]string{"https://a.test/x", "http://b.test"}, "", importProvider.FieldURL},
		{[]string{"Design, Research", "Research", "Design"}, "", importProvider.FieldMultiSelect},
		{[]string{"[Design, Research]", "[Design]"}, "", importProvider.FieldMultiSelect},
		{[]string{"High", "Low", "high"}, "", importProvider.FieldSelect},
		{[]string{"Ships Monday, if QA passes", "Waits on legal"}, "", importProvider.FieldText},
		{[]string{"One", "Two", "Three"}, "", importProvider.FieldText},
		{[]string{"No", "No"}, "", importProvider.FieldSelect},
		{nil, "", importProvider.FieldText},
	} {
		if got := guessKind(c.values, c.currency); got != c.want {
			t.Errorf("%q: got %s, want %s", c.values, got, c.want)
		}
	}
}

// Our export names a money field with its currency, and a field named like
// one of the list's own columns as a field; any other bracket is the name's.
func TestAFieldsHeaderSaysItsNameAndCurrency(t *testing.T) {
	for header, want := range map[string][2]string{
		"Budget (USD)":   {"Budget", "USD"},
		"Cost (eur)":     {"Cost", "EUR"},
		"Status (field)": {"Status", ""},
		"Effort (hrs)":   {"Effort (hrs)", ""},
		"Notes (old)":    {"Notes (old)", ""},
		"Plain":          {"Plain", ""},
	} {
		if name, cur := fieldName(header); name != want[0] || cur != want[1] {
			t.Errorf("%q: got %q %q", header, name, cur)
		}
	}
}

// A date is a day or a time as the files write them, never a guess between
// one country's order and another's.
func TestWhenReadsDaysAndTimesWithoutGuessing(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"2026-10-24", "Oct 24, 2026", "October 24, 2026", "24 Oct 2026", "2026/10/24"} {
		if d, at, ok := when(s, kolkata); !ok || at != nil || d.String() != "2026-10-24" {
			t.Errorf("%q: got %v %v %v", s, d, at, ok)
		}
	}
	for s, want := range map[string]time.Time{
		"2026-10-24 15:04":          time.Date(2026, 10, 24, 15, 4, 0, 0, kolkata),
		"2026-10-24T15:04:05Z":      time.Date(2026, 10, 24, 15, 4, 5, 0, time.UTC),
		"2026-10-24T15:04:05+02:00": time.Date(2026, 10, 24, 13, 4, 5, 0, time.UTC),
		"24/Oct/26 3:04 PM":         time.Date(2026, 10, 24, 15, 4, 0, 0, kolkata),
	} {
		if _, at, ok := when(s, kolkata); !ok || at == nil || !at.Equal(want) {
			t.Errorf("%q: got %v %v, want %v", s, at, ok, want)
		}
	}
	for _, s := range []string{"10/24/2026", "03/04/2026", "soon", ""} {
		if _, _, ok := when(s, kolkata); ok {
			t.Errorf("%q is never guessed", s)
		}
	}
}

// Money reads with its sign or code; a number with its thousands.
func TestMoneyAndNumbersRead(t *testing.T) {
	for s, want := range map[string]float64{"1250.50": 1250.5, "$1,250.50": 1250.5, "1,250.50 EUR": 1250.5, "€-3": -3, "₹ 99": 99} {
		if n, ok := money(s); !ok || n != want {
			t.Errorf("money %q: got %v %v", s, n, ok)
		}
	}
	for _, s := range []string{"n/a", "1.2.3", "NaN", "Inf"} {
		if _, ok := number(s); ok {
			t.Errorf("%q is no number", s)
		}
	}
}

// A cell the field's kind can't read stays its text, which the import keeps
// in the task's description; one it can is the kind's value.
func TestAValueTheKindCantReadStaysText(t *testing.T) {
	person := func(name, email string) string { return "p:" + name + email }
	for _, c := range []struct {
		kind, cell string
		want       any
	}{
		{importProvider.FieldNumber, "3.5", 3.5},
		{importProvider.FieldNumber, "n/a", "n/a"},
		{importProvider.FieldMoney, "$12.50", 12.5},
		{importProvider.FieldDate, "Oct 24, 2026", "2026-10-24"},
		{importProvider.FieldDate, "2026-10-24 15:04", "2026-10-24 15:04"},
		{importProvider.FieldCheckbox, "Yes", true},
		{importProvider.FieldCheckbox, "maybe", "maybe"},
		{importProvider.FieldPerson, "Ana Lima", "p:Ana Lima"},
		{importProvider.FieldPerson, "ana@acme.test", "p:ana@acme.test"},
		{importProvider.FieldText, "anything", "anything"},
	} {
		v := value(importProvider.SourceField{Type: c.kind}, c.cell, person)
		if v.Value != c.want || v.Text != c.cell {
			t.Errorf("%s %q: got %v (%q)", c.kind, c.cell, v.Value, v.Text)
		}
	}
}
