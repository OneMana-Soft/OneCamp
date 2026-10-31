package business

import (
	"fmt"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// What an import makes options of: the labels a select's or a multi-select's
// value names that its field doesn't offer, exactly when they are all that
// keep the value out; made options, they let it through.
func TestNotOptionsAreAllThatKeepAValueOut(t *testing.T) {
	state := field(model.FieldSelect, "State")
	state.Config = `{"options": [{"id": "o_read", "label": "Read"}, {"label": "Reading"}]}`
	tags := field(model.FieldMultiSelect, "Tags")
	tags.Config = `{"options": [{"label": "Classic"}]}`
	for _, c := range []struct {
		f    *model.Field
		v    any
		want string
	}{
		{state, "Abandoned", "Abandoned"},
		{state, "Read", ""},
		{state, "o_read", ""},        // an option named by its id
		{state, "label:Reading", ""}, // one stored without an id, by the id it goes by
		{state, "read", "read"},      // labels are matched as written
		{state, []string{"Abandoned"}, ""},
		{state, 3.0, ""},
		{state, "  ", ""},
		{tags, []string{"Classic", "Sci-fi", "Romance"}, "Sci-fi,Romance"},
		{tags, "Sci-fi", "Sci-fi"},
		{tags, []any{"Sci-fi", 2.0}, ""}, // an item no option takes keeps the cell out whatever is added
		{tags, []string{}, ""},
		{field(model.FieldText, "Notes"), "Abandoned", ""},
	} {
		if got := strings.Join(NotOptions(c.f, c.v), ","); got != c.want {
			t.Errorf("%s %#v: %q, want %q", c.f.Name, c.v, got, c.want)
		}
	}
	value := []string{"Classic", "Sci-fi", "Romance"}
	if _, err := Kinds.Check([]*model.Field{tags}, map[string]any{tags.Id.String(): value}, Input{}); err == nil {
		t.Fatal("a value outside the options was taken")
	}
	var opts []string
	for _, l := range append([]string{"Classic"}, NotOptions(tags, value)...) {
		opts = append(opts, fmt.Sprintf(`{"label": %q}`, l))
	}
	tags.Config = `{"options": [` + strings.Join(opts, ", ") + `]}`
	if _, err := Kinds.Check([]*model.Field{tags}, map[string]any{tags.Id.String(): value}, Input{}); err != nil {
		t.Errorf("made options, the labels didn't let the value through: %v", err)
	}
}
