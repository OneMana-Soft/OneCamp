package business

import (
	"errors"
	"fmt"
	"testing"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
)

// A spec naming a field the table hasn't got is the spec's fault, as an
// operator that isn't one is: a bad spec, said in words a person reads. The
// app's view shows it (422) and the API answers it as the caller's (400),
// never as the server failing to load the rows (the app's 503, the API's 500).
func TestAFieldTheTableHasntGotIsABadSpecInItsOwnWords(t *testing.T) {
	err := fmt.Errorf("filtering: %w", errNoField("field:5f0c"))
	if !errors.Is(err, query.ErrBadSpec) {
		t.Errorf("%v isn't a bad spec, so the app answers it 503 and the API 500", err)
	}
	if got, want := Refusal(err), `There's no field "5f0c" in this table`; got != want {
		t.Errorf("Refusal(%v) = %q, want %q", err, got, want)
	}
}

// A refusal's words leave out its kind and whatever wrapped it on the way out:
// those are for the logs.
func TestARefusalSaysWhyAndNotWhatKindItIs(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: %s", ErrCantQuery, "a number can't be compared with contains"), "A number can't be compared with contains"},
		{fmt.Errorf("sorting: %w", fmt.Errorf("%w: %s", query.ErrBadSpec, "at most 10 sorts")), "At most 10 sorts"},
		{ErrBadCursor, "That page marker isn't this query's"},
		{errors.New(""), ""},
	} {
		if got := Refusal(c.err); got != c.want {
			t.Errorf("Refusal(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
