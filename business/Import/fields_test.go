package business

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	taskField "github.com/akashc777/OneCamp/business/TaskField"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
)

func TestImportedFieldMatching(t *testing.T) {
	have := []*taskFieldModel.Field{
		{Name: "Budget", Type: taskField.TypeMoney, Currency: "USD"},
		{Name: "Notes", Type: taskField.TypeNumber},
		{Name: strings.Repeat("A", 40), Type: taskField.TypeText},
	}
	money := func(cur string) importProvider.SourceField {
		return importProvider.SourceField{Type: importProvider.FieldMoney, Currency: cur}
	}
	if !sameField(have[0], "budget", money("usd")) || !sameField(have[0], "Budget", money("")) {
		t.Error("a field of the same name and kind is the same field, whatever the case")
	}
	if sameField(have[0], "Budget", money("EUR")) {
		t.Error("a budget in euros isn't the one in dollars")
	}
	if sameField(have[1], "Notes", importProvider.SourceField{Type: importProvider.FieldText}) {
		t.Error("a text field isn't the number field of its name")
	}
	if got := freeName(have, "Notes"); got != "Notes (2)" {
		t.Errorf("a name in use by another kind of field: %q", got)
	}
	if got := freeName(have, "Owner"); got != "Owner" {
		t.Errorf("a free name stays: %q", got)
	}
	if got := freeName(have, strings.Repeat("a", 40)); utf8.RuneCountInString(got) > taskField.MaxNameLength || !strings.HasSuffix(got, " (2)") {
		t.Errorf("a long name makes room for its number: %q", got)
	}
}

func TestImportedOptions(t *testing.T) {
	in := []importProvider.SourceOption{{Label: " Blog "}, {Label: "blog"}, {Label: " "}, {Label: strings.Repeat("x", 60), Color: "teal"}}
	out, dropped := optionInputs(in)
	if len(out) != 2 || dropped != 0 || out[0].Label != "Blog" || utf8.RuneCountInString(out[1].Label) != taskField.MaxOptionLabel || out[1].Color != "teal" {
		t.Fatalf("names fitted, blanks and namesakes left out: %+v", out)
	}
	var many []importProvider.SourceOption
	for i := 0; i < 70; i++ {
		many = append(many, importProvider.SourceOption{Label: fmt.Sprintf("Option %d", i)})
	}
	// Namesakes and blanks aren't counted as dropped; only options past the limit are.
	many = append(many, importProvider.SourceOption{Label: "option 0"}, importProvider.SourceOption{Label: ""})
	if got, dropped := optionInputs(many); len(got) != taskField.MaxOptions || dropped != 70-taskField.MaxOptions {
		t.Errorf("as many options as a field can have: %d, dropped %d", len(got), dropped)
	}
}

func TestLinkOf(t *testing.T) {
	cases := map[string]string{
		"https://example.com/a":    "https://example.com/a",
		" example.com/brief ":      "https://example.com/brief",
		"x.com/r?to=https://y.com": "https://x.com/r?to=https://y.com",
		"TBD":                      "TBD",
		"N/A":                      "N/A",
		"-":                        "-",
		"mailto:ana@acme.com":      "mailto:ana@acme.com",
		"ana@acme.com":             "ana@acme.com",
		"see the doc":              "see the doc",
		"localhost:3000":           "localhost:3000",
	}
	for in, want := range cases {
		if got := linkOf(in); got != want {
			t.Errorf("linkOf(%q) = %q, want %q", in, got, want)
		}
	}
	// What linkOf leaves alone, Normalize turns away, so the value goes to the
	// task's description rather than becoming a link to nowhere.
	f := &taskFieldModel.Field{Name: "Brief", Type: taskField.TypeURL}
	for _, s := range []string{"TBD", "mailto:ana@acme.com", "ana@acme.com", "localhost:3000"} {
		if _, err := taskField.Normalize(f, mustMarshal(linkOf(s))); err == nil {
			t.Errorf("%q was kept as a link", s)
		}
	}
}

func TestValueText(t *testing.T) {
	channel := importProvider.SourceField{Name: "Channel", Type: importProvider.FieldSelect, Options: []importProvider.SourceOption{{SourceID: "o1", Label: "Blog"}, {SourceID: "o2", Label: "Email"}}}
	tags := channel
	tags.Type = importProvider.FieldMultiSelect
	cases := []struct {
		v    importProvider.SourceFieldValue
		want string
	}{
		{importProvider.SourceFieldValue{Field: channel, Value: "o2"}, "Email"},
		{importProvider.SourceFieldValue{Field: tags, Value: []string{"o1", "o2"}}, "Blog, Email"},
		{importProvider.SourceFieldValue{Field: importProvider.SourceField{Type: importProvider.FieldMoney, Currency: "EUR"}, Value: 1250.5}, "1250.5 EUR"},
		{importProvider.SourceFieldValue{Field: importProvider.SourceField{Type: importProvider.FieldCheckbox}, Value: true}, "Yes"},
		{importProvider.SourceFieldValue{Field: importProvider.SourceField{Type: importProvider.FieldPerson}, Value: "u-9"}, ""},
		{importProvider.SourceFieldValue{Field: importProvider.SourceField{Type: importProvider.FieldPerson}, Value: "u-9", Text: "Ada, Grace"}, "Ada, Grace"},
	}
	for _, c := range cases {
		if got := valueText(c.v); got != c.want {
			t.Errorf("%+v: %q, want %q", c.v, got, c.want)
		}
	}
	if got := renderLostValues([]lostValue{{"Brief", "TBD <soon>"}}); got != "<p><strong>Imported fields</strong></p><ul><li><strong>Brief:</strong> TBD &lt;soon&gt;</li></ul>" {
		t.Errorf("rendered: %s", got)
	}
	formula := importProvider.SourceFieldValue{Field: importProvider.SourceField{SourceID: "9", Name: "Margin"}, Text: "40%"}
	if lost := appendLost(nil, formula); len(lost) != 1 || lost[0] != (lostValue{"Margin", "40%"}) {
		t.Errorf("a value no field holds is listed with its text: %v", lost)
	}
	kept := importProvider.SourceFieldValue{Field: importProvider.SourceField{Name: "Reviewer", Type: importProvider.FieldPerson}, Value: "u-1", Text: "Ada, Grace", InDescription: true}
	if lost := appendLost(nil, kept); len(lost) != 0 {
		t.Errorf("a value the source lists in the description already isn't listed twice: %v", lost)
	}
}
