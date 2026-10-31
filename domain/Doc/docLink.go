package domain

// A link's title (docs F1): its address when it is made, and its preview's
// title once that is fetched (controllers/Doc), unless someone named it
// meanwhile.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
	"github.com/google/uuid"
)

// retitleRequest is RetitleLink's upsert: the title to, set on the live doc
// with docUUID only while its title is still from, and its uuid written
// again, as every doc update writes it; and the doc it renames. Pure.
//
// The title is compared as a value variable: doc_title's only index is a
// trigram one, which cannot answer eq for anything longer than three letters
// ("does not have a valid tokenizer").
func retitleRequest(docUUID, from, to string) (string, map[string]string, []dgexec.Mutation, error) {
	id, err := uuid.Parse(docUUID)
	if err != nil {
		return "", nil, nil, ErrNoDocToUpdate
	}
	title, _ := json.Marshal(to)
	q := `query q($id: string, $from: string) {
		live as var(func: eq(doc_uuid, $id)) @filter(` + liveDocFilter + `) { title as doc_title }
		doc as var(func: uid(live)) @filter(eq(val(title), $from))
		renamed(func: uid(doc)) { doc_uuid }
	}`
	set := fmt.Sprintf("uid(doc) <doc_title> %s .\nuid(doc) <doc_uuid> %q .\n", title, id.String())
	return q, map[string]string{"$id": id.String(), "$from": from}, []dgexec.Mutation{{SetNQuads: set, Cond: "@if(eq(len(doc), 1))"}}, nil
}

// RetitleLink names a live doc to while it is still called from (a link
// named by its address), in one conditional write, and says whether it did:
// a rename someone made meanwhile is left as it is.
func RetitleLink(ctx context.Context, docUUID, from, to string) (bool, error) {
	q, vars, muts, err := retitleRequest(docUUID, from, to)
	if err != nil {
		return false, err
	}
	resp, err := dgexec.Mutate(ctx, q, vars, muts...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/RetitleLink %s err: %+v", docUUID, err)
		return false, err
	}
	var got struct {
		Renamed []struct {
			UUID string `json:"doc_uuid"`
		} `json:"renamed"`
	}
	if err := json.Unmarshal(resp.GetJson(), &got); err != nil {
		return false, err
	}
	return len(got.Renamed) == 1, nil
}
