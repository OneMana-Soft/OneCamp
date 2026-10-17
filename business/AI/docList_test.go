package business

import (
	"context"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// openOnly is an Access Resolver whose Filter keeps the docs one person can
// open.
type openOnly struct {
	accessBusiness.Resolver
	uuid string
	open map[string]bool
}

func (o openOnly) Filter(_ context.Context, who accessBusiness.Subject, res []accessBusiness.Resource, _ accessBusiness.Level) ([]accessBusiness.Resource, error) {
	var kept []accessBusiness.Resource
	for _, r := range res {
		if who.UserUUID == o.uuid && o.open[r.UUID] {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// list_docs lists what the person can open (the API's list); for a run
// someone else asked for, only what that person can open too.
func TestListDocsListsWhatTheAskerCanOpenToo(t *testing.T) {
	savedList, savedReader, savedRules := listDocsFor, docListReader, accessBusiness.Rules
	t.Cleanup(func() { listDocsFor, docListReader, accessBusiness.Rules = savedList, savedReader, savedRules })
	var asked accessBusiness.Subject
	listDocsFor = func(_ context.Context, who accessBusiness.Subject, scope string, limit, _ int) ([]contentIOBusiness.ListedDoc, bool, error) {
		asked = who
		if scope != "mine" || limit != listDocsMax {
			t.Errorf("listed %s, %d", scope, limit)
		}
		return []contentIOBusiness.ListedDoc{{UUID: "d1", Title: "Roadmap"}, {UUID: "d2", Title: "", Private: true}, {UUID: "d3", Title: "Salaries", Private: true}}, false, nil
	}
	readers := []docReader{{uid: "0x1", uuid: "sponsor"}}
	docListReader = func(context.Context, string) ([]docReader, bool, error) { return readers, true, nil }
	accessBusiness.Rules = openOnly{uuid: "asker", open: map[string]bool{"d1": true, "d2": true}}

	out, _, err := executeListDocs(context.Background(), ai.ProposedAction{Params: map[string]string{"scope": "mine"}}, "sponsor")
	if err != nil || asked.UserUID != "0x1" || !strings.Contains(out, "Salaries (doc_uuid: d3, private)") || !strings.Contains(out, "Untitled (doc_uuid: d2, private)") {
		t.Fatalf("the sponsor's own run: %q, %v", out, err)
	}
	readers = append(readers, docReader{uid: "0x2", uuid: "asker"})
	out, _, err = executeListDocs(context.Background(), ai.ProposedAction{Params: map[string]string{"scope": "mine"}}, "sponsor")
	if err != nil || strings.Contains(out, "Salaries") || !strings.Contains(out, "Roadmap (doc_uuid: d1)") {
		t.Fatalf("a run for someone else: %q, %v", out, err)
	}
	if _, _, err := executeListDocs(context.Background(), ai.ProposedAction{Params: map[string]string{"scope": "everyone's"}}, "sponsor"); err == nil {
		t.Fatal("an unknown scope was listed")
	}
	docListReader = func(context.Context, string) ([]docReader, bool, error) { return nil, false, nil }
	if _, _, err := executeListDocs(context.Background(), ai.ProposedAction{}, "sponsor"); err != errAskerUnknown {
		t.Fatalf("a run for someone who can't be told: %v", err)
	}
}
