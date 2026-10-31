package basecamp

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	"github.com/google/uuid"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// An old import's people re-checked against Basecamp: of the projects asked
// about, Launch answers, one deleted since doesn't, and a source id that isn't
// Basecamp's number isn't asked at all; everyone the account lists comes back
// across its pages, Cleo, its client, kept out of projects, and the deleted
// person not at all. A sign-in that doesn't see the import's account answers
// for no project.
func TestARecheckReadsWhomBasecampKeepsOutOfProjects(t *testing.T) {
	fakeBasecamp(t)
	p, j := testProvider(t)
	got, err := p.recheckPeople(context.Background(), "tok", j, []string{"1", "77", "../people"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"1": true}; !maps.Equal(got.Answered, want) {
		t.Errorf("projects answering: %v, want %v", got.Answered, want)
	}
	if want := map[string]bool{"10": false, "11": false, "12": true, "14": false}; !maps.Equal(got.KeptOut, want) {
		t.Errorf("people kept out of projects: %v, want %v", got.KeptOut, want)
	}
	opts, _ := json.Marshal(map[string]any{importProvider.OptBasecampAccountID: "1234"})
	got, err = p.recheckPeople(context.Background(), "tok", &importModels.Job{Id: uuid.New(), Options: opts}, []string{"1"})
	if err != nil || len(got.Answered) != 0 {
		t.Errorf("another account's import: %v answering, %v; want none and no error", got.Answered, err)
	}
}
