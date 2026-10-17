package domain

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// GetDocReferences is the docs a doc's body links to now (doc_mentions), by
// uuid: the edges SetDocReferences keeps; and when the doc was made, which the
// mentions hook asks of a doc's first keeping (business/Doc docMentions.go).
func GetDocReferences(ctx context.Context, docUUID string) (*dgraphStruct.DgraphDoc, error) {
	return dgraphModels.GetDgraphDocInfoByUUID(ctx, `query refs($id: string) {
			docInfo(func: eq(doc_uuid, $id)) {
				uid
				doc_uuid
				doc_created_at
				doc_mentions { uid doc_uuid }
			}
		}`, map[string]string{"$id": docUUID})
}

// SetDocReferences makes a doc's doc_mentions the docs in refs that exist
// (uuids; itself left out), adding and removing only the edges that changed, and
// says whether anything did. doc is the doc as GetDocReferences read it.
func SetDocReferences(ctx context.Context, doc *dgraphStruct.DgraphDoc, refs []string) (bool, error) {
	if doc == nil || doc.Uid == "" || !helpers.IsDgraphUID(doc.Uid) {
		return false, ErrNotFound
	}
	want := map[string]bool{}
	for _, q := range quoteUUIDs(refs) {
		id := strings.Trim(q, `"`)
		if id != doc.Uuid {
			want[id] = true
		}
	}
	have := map[string]string{} // uuid -> uid
	for _, m := range doc.Mentions {
		if m != nil && m.Uuid != "" && helpers.IsDgraphUID(m.Uid) {
			have[m.Uuid] = m.Uid
		}
	}
	var del strings.Builder
	for id, uid := range have {
		if !want[id] {
			del.WriteString("<" + doc.Uid + "> <doc_mentions> <" + uid + "> .\n")
		}
	}
	var adds []string
	for id := range want {
		if _, ok := have[id]; !ok {
			adds = append(adds, id)
		}
	}
	if del.Len() == 0 && len(adds) == 0 {
		return false, nil
	}
	sort.Strings(adds)
	req := &api.Request{}
	if del.Len() > 0 {
		req.Mutations = append(req.Mutations, &api.Mutation{DelNquads: []byte(del.String())})
	}
	if len(adds) > 0 {
		// Each linked doc by uuid, so a link to a doc that doesn't exist adds
		// nothing (an empty variable sets no edge).
		var q, set strings.Builder
		q.WriteString("query {\n")
		for i, id := range adds {
			q.WriteString("\t\tr" + strconv.Itoa(i) + ` as var(func: eq(doc_uuid, "` + id + `"))` + "\n")
			set.WriteString("<" + doc.Uid + "> <doc_mentions> uid(r" + strconv.Itoa(i) + ") .\n")
		}
		q.WriteString("\t}")
		req.Query = q.String()
		req.Mutations = append(req.Mutations, &api.Mutation{SetNquads: []byte(set.String())})
	}
	if _, err := dgraphInit.DoCommitNow(ctx, req); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SetDocReferences err: %+v", err)
		return false, err
	}
	return true, nil
}

// GetDocHead is the little of a doc that a rename or a privacy change is told
// against: its title, privacy, comment setting and deletion.
func GetDocHead(ctx context.Context, docUUID string) (*dgraphStruct.DgraphDoc, error) {
	return dgraphModels.GetDgraphDocInfoByUUID(ctx, `query head($id: string) {
			docInfo(func: eq(doc_uuid, $id)) {
				uid
				doc_uuid
				doc_title
				doc_private
				doc_public_comment
				doc_deleted_at
			}
		}`, map[string]string{"$id": docUUID})
}
