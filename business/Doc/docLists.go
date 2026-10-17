package businness

// Docs as a tree and as lists, read for one person.
//
// The doc is a kind of tree (business/Tree/graph): its predicates, and how
// its levels read. The Hierarchy Service (business/Tree) orders them and asks
// the Access Resolver who may open what; nothing here decides that. The lists
// the docs home shows are read here too, each with what the access rule needs
// on every row, for controllers/Doc to ask it before naming one.
//
// This package can't ask the Access Resolver itself: business/Access is built
// on it (today's rules are in access.go).

import (
	"context"
	"strings"
	"time"

	placesBusiness "github.com/akashc777/OneCamp/business/Places"
	"github.com/akashc777/OneCamp/business/Tree/graph"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// DocTree is the doc tree's schema.
var DocTree = graph.Schema{
	Kind:      "doc",
	ID:        "doc_uuid",
	Parent:    "doc_parent",
	Position:  "doc_position",
	Created:   "doc_created_at",
	SpaceKind: "doc_space_kind",
	SpaceUUID: "doc_space_uuid",
	Live:      domain.LiveDocFilter,
}

// docKind is the doc tree's reads.
type docKind struct{}

func (docKind) Schema() graph.Schema { return DocTree }

func (docKind) Rows(ctx context.Context, viewerUID string, uuids []string) ([]graph.Row, error) {
	docs, err := domain.GetDocRowsByUUID(ctx, uuids, viewerUID)
	return docRows(docs), err
}

func (docKind) Children(ctx context.Context, viewerUID, parentUUID string, limit int) ([]graph.Row, error) {
	parent, err := domain.GetDocWithChildren(ctx, parentUUID, viewerUID, limit)
	if err == domain.ErrNotFound {
		return nil, graph.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return docRows(parent.Children), nil
}

func (docKind) Top(ctx context.Context, viewerUID, space string, limit int) ([]graph.Row, error) {
	section, kind, id := TreeSection(space)
	docs, err := domain.GetDocTreeTop(ctx, section, viewerUID, kind, id, limit)
	return docRows(docs), err
}

func (k docKind) TopFor(ctx context.Context, viewerUID, uuid string, leaving bool, limit int) ([]graph.Row, error) {
	docs, err := domain.GetDocRowsByUUID(ctx, []string{uuid}, viewerUID)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, graph.ErrNotFound
	}
	return k.Top(ctx, viewerUID, topSectionFor(docs[0], viewerUID, leaving), limit)
}

// topSectionFor is the section whose top a doc moved to the top lands in,
// for a person: its own (SpaceOf), or, leaving its space, the one it has
// without a space. Pure.
func topSectionFor(doc *dgraphStruct.DgraphDoc, viewerUID string, leaving bool) string {
	if leaving {
		out := *doc
		out.SpaceKind, out.SpaceUUID = "", ""
		doc = &out
	}
	return SpaceOf(doc, viewerUID)
}

// TreeSection reads a space as the tree's sections name it: "" or
// "workspace", "private", "shared", or "project:<uuid>" / "team:<uuid>".
func TreeSection(space string) (section, kind, uuid string) {
	switch space {
	case "", domain.SectionWorkspace:
		return domain.SectionWorkspace, "", ""
	case domain.SectionPrivate, domain.SectionShared:
		return space, "", ""
	}
	if k, id, ok := strings.Cut(space, ":"); ok && (k == "project" || k == "team") && id != "" {
		return domain.SectionSpace, k, id
	}
	return domain.SectionWorkspace, "", ""
}

// SpaceOf is the section a doc sits at the top of for a person: its space if
// it has one; else private if it's private and theirs, shared if it's private
// and someone else's, the workspace if it isn't private.
func SpaceOf(doc *dgraphStruct.DgraphDoc, viewerUID string) string {
	if doc.SpaceKind != "" && doc.SpaceUUID != "" {
		return doc.SpaceKind + ":" + doc.SpaceUUID
	}
	if doc.IsPrivate == nil || !*doc.IsPrivate {
		return domain.SectionWorkspace
	}
	if OwnedBy(doc, viewerUID) {
		return domain.SectionPrivate
	}
	return domain.SectionShared
}

// OwnedBy is whether the person (by graph uid) owns the doc: its owner, or for
// a doc made before owners were recorded, its maker.
func OwnedBy(doc *dgraphStruct.DgraphDoc, viewerUID string) bool {
	if doc == nil || viewerUID == "" {
		return false
	}
	if doc.Owner != nil && doc.Owner.Uid != "" {
		return doc.Owner.Uid == viewerUID
	}
	return doc.CreatedBy != nil && doc.CreatedBy.Uid == viewerUID
}

func docRows(docs []*dgraphStruct.DgraphDoc) []graph.Row {
	rows := make([]graph.Row, 0, len(docs))
	for _, d := range docs {
		if d == nil || d.Uuid == "" {
			continue
		}
		r := graph.Row{UID: d.Uid, UUID: d.Uuid, Title: d.Title, Icon: d.Icon, Position: d.Position, ChildCount: int(d.ChildCount), Data: d}
		if d.CreatedAt != nil {
			r.CreatedAt = *d.CreatedAt
		}
		rows = append(rows, r)
	}
	return rows
}

func init() {
	graph.RegisterKind(docKind{})
}

// ListDocs reads one page of a list kept in the graph (Mine, Shared with me,
// Workspace, Archived) and how many the list holds, under one filter.
func ListDocs(ctx context.Context, q domain.DocListQuery) (*dgraphStruct.DgraphDocList, error) {
	return domain.ListDocs(ctx, q)
}

// recentWindow and recentCap bound Recent: what the person opened in the
// last three months, the newest 500.
const (
	recentWindow = 90 * 24 * time.Hour
	recentCap    = 500
)

// RecentDocs is the docs the person (uuid, uid) opened lately, newest first,
// as list rows with when they opened each; ones gone from the graph are left
// out, the rest are the caller's to check.
func RecentDocs(ctx context.Context, viewerUUID, viewerUID string, now time.Time) ([]*dgraphStruct.DgraphDoc, error) {
	views, err := placesBusiness.Recent(ctx, viewerUUID, "doc", now.Add(-recentWindow), recentCap)
	if err != nil || len(views) == 0 {
		return nil, err
	}
	ids := make([]string, len(views))
	for i, v := range views {
		ids[i] = v.ResourceUUID
	}
	docs, err := domain.GetDocRowsByUUID(ctx, ids, viewerUID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*dgraphStruct.DgraphDoc, len(docs))
	for _, d := range docs {
		byID[strings.ToLower(d.Uuid)] = d
	}
	out := make([]*dgraphStruct.DgraphDoc, 0, len(views))
	for _, v := range views {
		if d := byID[strings.ToLower(v.ResourceUUID)]; d != nil {
			at := v.LastViewedAt
			d.ViewedAt = &at
			out = append(out, d)
		}
	}
	return out, nil
}

// StarredDocs is the docs the person starred, in their order, as list rows;
// ones gone from the graph are left out, the rest are the caller's to check.
func StarredDocs(ctx context.Context, viewerUUID, viewerUID string) ([]*dgraphStruct.DgraphDoc, error) {
	ids, err := placesBusiness.IDs(ctx, viewerUUID, placesBusiness.Star, "doc")
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	docs, err := domain.GetDocRowsByUUID(ctx, ids, viewerUID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*dgraphStruct.DgraphDoc, len(docs))
	for _, d := range docs {
		byID[strings.ToLower(d.Uuid)] = d
	}
	out := make([]*dgraphStruct.DgraphDoc, 0, len(ids))
	for _, id := range ids {
		if d := byID[strings.ToLower(id)]; d != nil {
			out = append(out, d)
		}
	}
	return out, nil
}

// DocRows reads docs by uuid as list rows, deleted ones included: a place's
// docs, a page's path.
func DocRows(ctx context.Context, uuids []string, viewerUID string) ([]*dgraphStruct.DgraphDoc, error) {
	return domain.GetDocRowsByUUID(ctx, uuids, viewerUID)
}

// Crumb is one page above a list row: its id, title and icon, or, for a page
// the person can't open, only that there is one (Redacted). The docs lists and
// the docs home's search name the pages above a row this one way (CrumbsOf).
type Crumb struct {
	UUID     string `json:"doc_uuid,omitempty"`
	Title    string `json:"doc_title,omitempty"`
	Icon     string `json:"doc_icon,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`
}

// PathAbove is the pages above a row, the top first, as the row's read holds
// them (up to three).
func PathAbove(d *dgraphStruct.DgraphDoc) []*dgraphStruct.DgraphDoc {
	var up []*dgraphStruct.DgraphDoc
	for p := d.Parent; p != nil && len(up) < 3; p = p.Parent {
		up = append([]*dgraphStruct.DgraphDoc{p}, up...)
	}
	return up
}

// CrumbsOf is the pages above a row as a list shows them: named where open
// says the person may open the page, redacted where not. Never nil, so a row
// with nothing above it says so.
func CrumbsOf(d *dgraphStruct.DgraphDoc, open func(docUUID string) bool) []Crumb {
	out := []Crumb{}
	for _, p := range PathAbove(d) {
		if open(p.Uuid) {
			out = append(out, Crumb{UUID: p.Uuid, Title: p.Title, Icon: p.Icon})
		} else {
			out = append(out, Crumb{Redacted: true})
		}
	}
	return out
}

// OwnerOf is who a list row names as a doc's owner: its owner, or, for a doc
// with none recorded, the person who made it.
func OwnerOf(d *dgraphStruct.DgraphDoc) *Person {
	if d.Owner != nil && d.Owner.Uuid != "" {
		return PersonOf(d.Owner)
	}
	return PersonOf(d.CreatedBy)
}
