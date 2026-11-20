package business

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// CR-459 D2: what a table's input refuses is the person's to change, not ours
// to retry, so each refusal is a 400 helpers.Refusal in its words, and a
// template install answers it 400 (CR-444 D1), not 500 "Please try again".
// Every one is refused before any store is read.
func TestWhatATablesInputRefusesIsA400InItsWords(t *testing.T) {
	for _, c := range []struct {
		in    TableInput
		words string
	}{
		{TableInput{Name: "  "}, "name is required"},
		{TableInput{Name: strings.Repeat("n", maxNameLen+1)}, "name is too long"},
		{TableInput{Name: "CRM", Description: strings.Repeat("d", maxDescriptionLen+1)}, "description is too long"},
		{TableInput{Name: "CRM", Icon: strings.Repeat("i", maxIconLen+1)}, "icon is too long"},
		{TableInput{Name: "CRM", Visibility: "nobody"}, "invalid visibility"},
	} {
		_, err := createTableRow(context.Background(), c.in, Actor{UserID: uuid.New()})
		var r *helpers.Refusal
		if !errors.As(err, &r) || r.Status != http.StatusBadRequest || r.Msg != c.words {
			t.Errorf("%q: got %#v, want a 400 helpers.Refusal in its words", c.words, err)
		}
	}
}
