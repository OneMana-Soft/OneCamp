package business

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Names and descriptions are counted in characters, as people and the web's
// fields count them. Counted in bytes, a column name in Hindi (three bytes a
// letter) was refused at 41 letters of 120, and a description at 667 of 2,000.
func TestNamesAndDescriptionsAreCountedInCharacters(t *testing.T) {
	hindi := func(n int) string { return strings.Repeat("क", n) }
	table := uuid.New()

	if _, _, err := buildField(table, FieldInput{Name: hindi(maxNameLen), Type: "text", Description: hindi(maxDescriptionLen)}, nil, false); err != nil {
		t.Errorf("a field named in %d Hindi letters, described in %d, was refused: %v", maxNameLen, maxDescriptionLen, err)
	}
	if _, _, err := buildField(table, FieldInput{Name: hindi(maxNameLen + 1), Type: "text"}, nil, false); err == nil || err.Error() != "field name is too long" {
		t.Errorf("a field name of %d letters gave %v", maxNameLen+1, err)
	}
	if _, _, err := buildField(table, FieldInput{Name: "Notes", Type: "text", Description: hindi(maxDescriptionLen + 1)}, nil, false); err == nil || err.Error() != "field description is too long" {
		t.Errorf("a description of %d letters gave %v", maxDescriptionLen+1, err)
	}

	if _, err := buildView(table, ViewInput{Name: hindi(maxNameLen), Type: "grid"}); err != nil {
		t.Errorf("a view named in %d Hindi letters was refused: %v", maxNameLen, err)
	}
	if _, err := buildView(table, ViewInput{Name: hindi(maxNameLen + 1), Type: "grid"}); err == nil {
		t.Errorf("a view name of %d letters was taken", maxNameLen+1)
	}
}
