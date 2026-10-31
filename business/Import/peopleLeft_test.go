package business

import (
	"encoding/json"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// Someone who left the source (Linear's deactivated users, Jira's
// inactive accounts) came in as a placeholder like anyone else and was
// offered an invitation. The import now records that they left, which the
// invitations read (people model: metadata deleted), so they are never
// offered one; their history keeps their name.
func TestSomeoneWhoLeftTheSourceIsNeverInvited(t *testing.T) {
	raw, _ := json.Marshal(placeholderMetadata(importProvider.SourceUser{SourceID: "u1", Left: true}, "ada@acme.test", "Ada", "ada"))
	var md struct {
		Deleted bool `json:"deleted"`
	}
	if err := json.Unmarshal(raw, &md); err != nil || !md.Deleted {
		t.Fatalf("metadata %s: want deleted true for someone who left", raw)
	}
	if got := classifyPerson(importModels.ImportedPerson{IsExternal: true, Email: "ada@acme.test", SourceLeft: md.Deleted}); got != personLeft {
		t.Fatalf("someone who left is offered as %v, want personLeft", got)
	}
	raw, _ = json.Marshal(placeholderMetadata(importProvider.SourceUser{SourceID: "u2"}, "bo@acme.test", "Bo", "bo"))
	if json.Unmarshal(raw, &md) != nil || md.Deleted {
		t.Fatalf("metadata %s: someone still there isn't marked as left", raw)
	}
}
