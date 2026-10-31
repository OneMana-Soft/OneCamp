package linear

import (
	"fmt"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// L-2: every label comes over, with no cap of 20. A label in a group (Linear
// gives an issue one label of each group) becomes that group's single-select
// field, the same field on every issue; the rest become tags. An id the
// workspace no longer lists (a label since deleted) is left out.
func TestEveryLabelComesAndAGroupBecomesASelectField(t *testing.T) {
	labels := map[string]linearLabel{
		"g-size": {ID: "g-size", Name: "Size", IsGroup: true},
		"s":      {ID: "s", Name: "S", ParentID: "g-size", Color: "#4ea7fc"},
		"l":      {ID: "l", Name: "L", ParentID: "g-size", Color: "#eb5757"},
		"g-plat": {ID: "g-plat", Name: "Platform", IsGroup: true},
		"ios":    {ID: "ios", Name: "iOS", ParentID: "g-plat"},
		"loose":  {ID: "loose", Name: "Loose", ParentID: "not-a-group"},
	}
	ids := []string{"l", "ios", "gone", "loose"}
	for i := 0; i < 22; i++ {
		id := fmt.Sprintf("t%02d", i)
		labels[id] = linearLabel{ID: id, Name: "tag " + id}
		ids = append(ids, id)
	}
	iss := linearIssue{ID: "iss-1", Identifier: "ENG-1", LabelIDs: ids}
	placeLabels(&iss, labels)
	out := New().issueToSourceTask(iss, "proj-1")

	if len(out.Labels) != 23 {
		t.Fatalf("%d tags, want 23 (22 plain and one whose parent isn't a group): %v", len(out.Labels), out.Labels)
	}
	byName := map[string]importProvider.SourceFieldValue{}
	for _, v := range out.Fields {
		byName[v.Field.Name] = v
	}
	size, ok := byName["Size"]
	if !ok || size.Field.Type != importProvider.FieldSelect || size.Field.SourceID != "label-group:g-size" {
		t.Fatalf("the Size group as a field: %+v (%v)", size.Field, ok)
	}
	if size.Value != "l" || size.Text != "L" {
		t.Errorf("Size's value %v (%q), want l (L)", size.Value, size.Text)
	}
	if len(size.Field.Options) != 2 || size.Field.Options[0].Label != "L" || size.Field.Options[1].Label != "S" {
		t.Errorf("Size's options %+v, want L and S", size.Field.Options)
	}
	if size.Field.Options[0].Color == "" {
		t.Errorf("an option keeps its label's colour")
	}
	if plat := byName["Platform"]; plat.Value != "ios" || plat.Text != "iOS" {
		t.Errorf("Platform's value %v (%q), want ios (iOS)", plat.Value, plat.Text)
	}
}
