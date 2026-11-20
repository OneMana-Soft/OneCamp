package business

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// CR-459 D2: what a move filter refuses is the person's to change, not ours to
// retry, so it is a 400 helpers.Refusal in its words, and an agent or workflow
// install answers it 400 (CR-444 D1), not 500 "Please try again". Both are
// refused before any store is read; a project's own statuses need the
// integration test.
func TestWhatAMoveFilterRefusesIsA400InItsWords(t *testing.T) {
	for _, c := range []struct {
		project, status, words string
	}{
		{"not-a-uuid", "", "that is not a project"},
		{"", "QA", `"QA" is not a status every project has`},
	} {
		_, err := NormalizeMoveFilter(context.Background(), c.project, c.status)
		var r *helpers.Refusal
		if !errors.As(err, &r) || r.Status != http.StatusBadRequest || !strings.HasPrefix(r.Msg, c.words) {
			t.Errorf("(%q, %q): got %#v, want a 400 helpers.Refusal %q", c.project, c.status, err, c.words)
		}
	}
}
