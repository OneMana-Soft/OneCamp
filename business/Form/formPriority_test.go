package business

import (
	"errors"
	"testing"
)

// UX-PROJECTS-H1-BE (PROJECTS-14): a new form makes tasks with no priority
// until it picks one, and keeps the one it picks; a form's old rows keep the
// medium they were saved with. A form that adds table rows has none to pick.
func TestANewFormHasNoPriorityUntilItPicksOne(t *testing.T) {
	in := Input{Title: "Requests", Fields: []Field{{Label: "What", Type: ShortText}}}
	if f, err := CheckForm(in); err != nil || f.Priority != "" {
		t.Fatalf("a new form: %q %v, want no priority", f.Priority, err)
	}
	in.Priority = "urgent"
	if f, err := CheckForm(in); err != nil || f.Priority != "urgent" {
		t.Fatalf("a form that picks urgent: %q %v", f.Priority, err)
	}
	in.Priority = "someday"
	var fe *FormError
	if _, err := CheckForm(in); !errors.As(err, &fe) || err.Error() != "Pick urgent, high, medium or low priority, or none." {
		t.Fatalf("a priority that isn't one: %v", err)
	}
}
