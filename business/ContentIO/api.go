package business

// The public API's docs (/v1/docs, controllers/V1 docs.go): read one as
// Markdown, make one from Markdown, add Markdown to the end of one. Each runs
// as the token's owner through what the app runs (the Access Resolver, the
// exporter, the tree's create and the Live Doc Writer), so the API can do
// nothing its owner couldn't, and an agent acting through it is credited
// beside them (I11).

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// ErrEmpty is Markdown with nothing in it to add.
var ErrEmpty = errors.New("content io: there is nothing to add")

// ListedDoc is a doc as the API lists it.
type ListedDoc struct {
	UUID    string `json:"uuid"`
	Title   string `json:"title"`
	Icon    string `json:"icon,omitempty"`
	Private bool   `json:"private"`
	// ParentUUID is the page it sits under, named only when the person can
	// open that page too (I12).
	ParentUUID string     `json:"parent_uuid,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
}

// Seam for tests: a page of a doc list kept in the graph.
var listDocs = docBusiness.ListDocs

// ListDocs is one page of the docs who can open, newest edit first: everyone's
// (docDomain.ScopeWorkspace), their own (ScopeMine) or shared with them
// (ScopeShared), and whether more follow it. The Access Resolver has the last
// word on each doc and on the page above it.
func ListDocs(ctx context.Context, who accessBusiness.Subject, scope string, limit, offset int) ([]ListedDoc, bool, error) {
	list, err := listDocs(ctx, docDomain.DocListQuery{Scope: scope, ViewerUID: who.UserUID, Sort: docDomain.SortEdited, First: limit, Offset: offset})
	if err != nil {
		return nil, false, err
	}
	var res []accessBusiness.Resource
	for _, d := range list.Docs {
		res = append(res, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: d.Uuid})
		if d.Parent != nil && d.Parent.Uuid != "" {
			res = append(res, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: d.Parent.Uuid})
		}
	}
	open, err := openable(ctx, who, res)
	if err != nil {
		return nil, false, err
	}
	out := []ListedDoc{}
	for _, d := range list.Docs {
		if open[d.Uuid] {
			out = append(out, listed(d, open))
		}
	}
	return out, offset+limit < int(list.Count), nil
}

// openable is which of the resources who can open, by the Access Resolver.
func openable(ctx context.Context, who accessBusiness.Subject, res []accessBusiness.Resource) (map[string]bool, error) {
	out := map[string]bool{}
	if len(res) == 0 {
		return out, nil
	}
	kept, err := accessBusiness.Rules.Filter(ctx, who, res, accessBusiness.LevelView)
	if err != nil {
		return nil, err
	}
	for _, k := range kept {
		out[k.UUID] = true
	}
	return out, nil
}

// listed is a doc's row as the API lists it; open says which pages the
// person can open, for naming the page above it.
func listed(d *dgraphStruct.DgraphDoc, open map[string]bool) ListedDoc {
	out := ListedDoc{UUID: d.Uuid, Title: d.Title, Icon: d.Icon, Private: docDomain.IsPrivate(d), CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
	if d.Parent != nil && open[d.Parent.Uuid] {
		out.ParentUUID = d.Parent.Uuid
	}
	return out
}

// ReadForAPI is a doc as Markdown, the app's Markdown export of it (its title
// the first heading, read from the live doc, typing included), recorded as an
// export made through the API, with the doc as the API lists it.
func ReadForAPI(ctx context.Context, who accessBusiness.Subject, docUUID string) (ListedDoc, string, error) {
	var md markdownBuffer
	if err := thePipeline.export(ctx, who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID}, "md", "api", &md); err != nil {
		return ListedDoc{}, "", err
	}
	rows, err := docRowsOf(ctx, []string{docUUID}, who.UserUID)
	if err != nil {
		return ListedDoc{}, "", err
	}
	if len(rows) == 0 {
		return ListedDoc{}, "", ErrNotFound
	}
	var parent []accessBusiness.Resource
	if p := rows[0].Parent; p != nil && p.Uuid != "" {
		parent = append(parent, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: p.Uuid})
	}
	open, err := openable(ctx, who, parent)
	if err != nil {
		return ListedDoc{}, "", err
	}
	return listed(rows[0], open), md.String(), nil
}

// markdownBuffer keeps an export's file.
type markdownBuffer struct{ bytes.Buffer }

func (b *markdownBuffer) Start(string, string) io.Writer { return &b.Buffer }

// CreateFromMarkdown makes a doc from Markdown under parentUUID ("" is the
// top, where it is private to who), titled title or, with none, by the
// heading 1 the Markdown starts with, and says what it left out: a picture it
// couldn't upload. ErrUnavailable when the document service could not take
// the content: the doc made for it is taken away again.
func CreateFromMarkdown(ctx context.Context, who accessBusiness.Subject, title, markdown, parentUUID string) (Created, []Skipped, error) {
	body, err := contentcodec.FromMarkdown(markdown, nil)
	if err != nil {
		return Created{}, nil, err
	}
	if strings.TrimSpace(title) == "" {
		title, body = titleFromBody(body, "")
	}
	made, err := MakePages(ctx, who, parentUUID, []Page{{Key: "markdown", Title: title, Body: body}})
	if err != nil {
		return Created{}, nil, err
	}
	if made.Filled == 0 || len(made.Created) == 0 {
		return Created{}, nil, ErrUnavailable
	}
	return made.Created[0], made.Skipped, nil
}

// AppendMarkdown adds Markdown to the end of a doc as one live edit on its
// live copy (A4); who must be able to edit the doc. A picture the Markdown
// carries (a data: image) is uploaded to the doc first, as an import's is, and
// one that can't be is left out and said so. An agent acting for who has its
// addition marked as its own, with the doc's version kept first, so the
// addition can be reverted.
func AppendMarkdown(ctx context.Context, who accessBusiness.Subject, docUUID, markdown string) (collab.Result, []Skipped, error) {
	if docUUID == "" {
		return collab.Result{}, nil, ErrNotFound
	}
	if err := checkParent(ctx, who, docUUID); err != nil {
		return collab.Result{}, nil, err
	}
	if strings.TrimSpace(markdown) == "" {
		return collab.Result{}, nil, ErrEmpty
	}
	body, err := contentcodec.FromMarkdown(markdown, nil)
	if err != nil {
		return collab.Result{}, nil, err
	}
	person, user, err := subjectFor(ctx, who.UserUUID)
	if err != nil {
		return collab.Result{}, nil, ErrForbidden
	}
	userID, _ := uuid.Parse(who.UserUUID)
	maker := &docMaker{who: person, user: user, userID: userID}
	body, lost := maker.uploadImages(ctx, docUUID, Page{Key: "markdown", Body: body}, body)
	opts := collab.Options{By: collab.By{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID}}
	if who.AgentUUID != "" {
		opts.AgentEdit, opts.SnapshotFirst = true, true
	}
	res, err := collab.Live.Append(ctx, docUUID, contentcodec.ToHTML(body), opts)
	return res, lost, err
}
