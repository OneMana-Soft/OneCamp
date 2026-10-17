package business

import (
	"context"
	"testing"
	"time"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

type embedCall struct {
	title, body, doc, createdBy string
	private                     bool
	reading, editing, comment   []string
}

func withEmbedSeams(t *testing.T, doc *dgraphStruct.DgraphDoc) *[]embedCall {
	t.Helper()
	prevRead, prevEmbed := readDocForEmbed, embedDoc
	t.Cleanup(func() { readDocForEmbed, embedDoc = prevRead, prevEmbed })
	calls := &[]embedCall{}
	readDocForEmbed = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) { return doc, nil }
	embedDoc = func(title, body, docUUID, _, _ string, private bool, createdBy string, reading, editing, commenting []string) {
		*calls = append(*calls, embedCall{title, body, docUUID, createdBy, private, reading, editing, commenting})
	}
	return calls
}

// A changed save re-embeds the doc with its stored title and the saved body as
// text, and with who may see it as the doc says (the save carries the body
// alone: it used to drop the title, and to index a private doc as public).
func TestASavedBodyIsEmbeddedWithItsTitleAndSharing(t *testing.T) {
	yes := true
	zero := time.Time{}
	calls := withEmbedSeams(t, &dgraphStruct.DgraphDoc{
		Uuid: "d1", Title: "Q3 reorg", IsPrivate: &yes, DeletedAt: &zero,
		CreatedBy:   &dgraphStruct.DgraphUser{Uuid: "ana"},
		EditingUser: []*dgraphStruct.DgraphUser{{Uuid: "ana"}, nil, {Uuid: "ben"}},
	})
	embedSavedDoc(context.Background(), docBusiness.Change{DocUUID: "d1", Body: "<h1>Plan</h1><p>zebra-quartz</p>"})
	if len(*calls) != 1 {
		t.Fatalf("embedded %d times", len(*calls))
	}
	c := (*calls)[0]
	if c.title != "Q3 reorg" || c.doc != "d1" || !c.private || c.createdBy != "ana" {
		t.Errorf("%+v", c)
	}
	if c.body == "" || c.body[0] == '<' {
		t.Errorf("the body is embedded as markup: %q", c.body)
	}
	if len(c.editing) != 2 || c.reading == nil || len(c.reading) != 0 || c.comment == nil {
		t.Errorf("sharing: editing %v reading %v commenting %v (empty lists, never nil)", c.editing, c.reading, c.comment)
	}
}

// A doc deleted since its save is not put back in the index.
func TestADocDeletedSinceItsSaveIsNotEmbedded(t *testing.T) {
	gone := time.Now()
	calls := withEmbedSeams(t, &dgraphStruct.DgraphDoc{Uuid: "d1", Title: "Old", DeletedAt: &gone})
	embedSavedDoc(context.Background(), docBusiness.Change{DocUUID: "d1", Body: "<p>x</p>"})
	if len(*calls) != 0 {
		t.Errorf("a deleted doc was embedded: %+v", *calls)
	}
}

// The hook is on the save path: registering its name again panics.
func TestTheEmbedHookIsRegistered(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("ai.embed is not registered as a save hook")
		}
	}()
	docBusiness.SaveHooks.Register("ai.embed", func(context.Context, docBusiness.Change) {})
}
