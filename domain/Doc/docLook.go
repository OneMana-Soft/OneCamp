package domain

// A doc's page chrome (docs programme B6): its look (D-2: the icon and the
// cover, and where the cover sits; D-7: full width; the font and small text),
// the docs that link to it, and a doc in the trash as its page shows it. The
// predicates are A3's (connectDgraphDB.go). Who may read or change any of it
// is the Access Resolver's to say, before these are called.

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// LookWrite is one change to a doc's look. A nil field is left as it is; an
// empty Icon, Cover or Font takes it away (a cover's offset goes with it).
type LookWrite struct {
	Icon        *string
	Cover       *string
	CoverOffset *int
	FullWidth   *bool
	Font        *string
	SmallText   *bool
}

// lookFields is a doc's look, as its page reads it. (Its lock level is one of
// the access facts, so a block reading those doesn't name it twice.)
const lookFields = `
			doc_icon
			doc_cover_key
			doc_cover_offset
			doc_full_width
			doc_font
			doc_small_text`

// lookRequest is WriteDocLook's upsert: the change, on the doc with the uuid
// only while it is live, and the doc as found. It writes doc_uuid too, as
// every doc update does, so a delete committed while it is in flight aborts
// it, and its retry finds the doc deleted. Pure.
func lookRequest(docUUID string, w LookWrite) (*api.Request, error) {
	id, err := uuid.Parse(docUUID)
	if err != nil {
		return nil, ErrNoDocToUpdate
	}
	set := map[string]any{"uid": "uid(doc)", "doc_uuid": id.String()}
	var del []string
	text := func(pred string, v *string) {
		switch {
		case v == nil:
		case *v == "":
			del = append(del, pred)
		default:
			set[pred] = *v
		}
	}
	text("doc_icon", w.Icon)
	text("doc_cover_key", w.Cover)
	text("doc_font", w.Font)
	if w.Cover != nil && *w.Cover == "" {
		del = append(del, "doc_cover_offset")
	} else if w.CoverOffset != nil {
		set["doc_cover_offset"] = *w.CoverOffset
	}
	if w.FullWidth != nil {
		set["doc_full_width"] = *w.FullWidth
	}
	if w.SmallText != nil {
		set["doc_small_text"] = *w.SmallText
	}
	raw, err := json.Marshal(set)
	if err != nil {
		return nil, err
	}
	muts := []*api.Mutation{{SetJson: raw, Cond: "@if(eq(len(doc), 1))"}}
	if len(del) > 0 {
		var nq []byte
		for _, p := range del {
			nq = append(nq, "uid(doc) <"+p+"> * .\n"...)
		}
		muts = append(muts, &api.Mutation{DelNquads: nq, Cond: "@if(eq(len(doc), 1))"})
	}
	return &api.Request{
		Query: `query q($id: string) {
			doc as var(func: eq(doc_uuid, $id)) @filter(` + liveDocFilter + `)
			found(func: eq(doc_uuid, $id)) { uid doc_deleted_at }
		}`,
		Vars:      map[string]string{"$id": id.String()},
		Mutations: muts,
	}, nil
}

// WriteDocLook writes a change to a live doc's look in one conditional
// transaction: ErrNotFound when no doc has the uuid, ErrDocDeleted when it is
// in the trash.
func WriteDocLook(ctx context.Context, docUUID string, w LookWrite) error {
	req, err := lookRequest(docUUID, w)
	if err != nil {
		return err
	}
	resp, err := dgraphInit.DoCommitNow(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WriteDocLook err: %+v", err)
		return err
	}
	var found struct {
		Found []*dgraphStruct.DgraphDoc `json:"found"`
	}
	if err := json.Unmarshal(resp.GetJson(), &found); err != nil {
		return err
	}
	switch {
	case len(found.Found) == 0:
		return ErrNotFound
	case helpers.IsSoftDeleted(found.Found[0].DeletedAt):
		return ErrDocDeleted
	}
	return nil
}

// GetDocMentioners reads the live pages whose bodies link to a doc (A6's
// doc_mentions, by its reverse), newest edit first, at most limit, with what
// a backlink shows of each: only a page has a body to link from (pageOnly).
// Nobody's access is decided here.
func GetDocMentioners(ctx context.Context, docUUID string, limit int) ([]*dgraphStruct.DgraphDoc, error) {
	q := `query mentioners($id: string) {
		var(func: eq(doc_uuid, $id)) { m as ~doc_mentions }
		docInfo(func: uid(m), orderdesc: doc_updated_at, first: ` + strconv.Itoa(limit) + `) @filter(` + liveDocFilter + ` AND has(doc_uuid) AND ` + pageOnly + `) {
			uid
			doc_uuid
			doc_title
			doc_icon
			doc_updated_at
		}
	}`
	docs, err := dgraphModels.GetDgraphDocs(ctx, q, map[string]string{"$id": docUUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocMentioners err: %+v", err)
	}
	return docs, err
}

// GetDocForTrashPage reads a doc, deleted or not, with what its page shows in
// the trash (its title, look and body, and who deleted it and when) and the
// access facts that say whether the person could open it before. ErrNotFound
// when no doc has the uuid.
func GetDocForTrashPage(ctx context.Context, docUUID, viewerUID string) (*dgraphStruct.DgraphDoc, error) {
	q := `query trashed($id: string, $viewer: string) {` + AccessPrelude("$viewer") + `
		docInfo(func: eq(doc_uuid, $id)) {
			uid
			doc_uuid
			doc_title
			doc_body
			doc_deleted_batch
			doc_deleted_by { uid user_uuid user_name user_full_name user_profile_object_key }` + lookFields + accessFacts + `
		}
	}`
	doc, err := dgraphModels.GetDgraphDocInfoByUUID(ctx, q, map[string]string{"$id": docUUID, "$viewer": ViewerUID(viewerUID)})
	helpers.LogReadFailure(ctx, err, "domain/GetDocForTrashPage doc "+docUUID)
	return doc, err
}
