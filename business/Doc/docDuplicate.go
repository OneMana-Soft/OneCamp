package businness

// Duplicate: a copy of a page, and with it the pages under it, for the person
// asking (structure.md P0.5). The copy is theirs. It goes right after the
// page when they may edit it and may add to its parent, else at the top of
// their private pages; its subpages keep their shape and order. Each copy gets
// the page's title (the top one as "Copy of …"), body, look and style; not its
// comments, history, sharing, lock or verification. Embeds stay references: a
// table, a task list or an image still shows what it pointed at.
//
// A copy takes the sharing and the space of the page it goes under, as any
// new subpage does, only when the person may edit the page it copies and
// every page above it in the copy: a copy under a page every member may open
// is published to every member. Anyone else's copy is theirs alone, private
// and in no space (the coordinator's ruling, as for Save as template: a
// viewer's copy is always private).
//
// Which pages are copied is the caller's: only ones the person can open
// (business/Access), and a page under one that isn't copied isn't either.

import (
	"context"
	"errors"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/business/Tree/graph"
	"github.com/akashc777/OneCamp/business/Tree/position"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// DuplicateCap is the most pages one duplicate copies.
const DuplicateCap = 200

// CopyPrefix starts the title of a copied page.
const CopyPrefix = "Copy of "

// Duplicated is what a duplicate made: the copy of the page asked for, and
// how many pages it copied in all, with each copy's source and new parent so
// the caller can give each copy its parent's sharing.
type Duplicated struct {
	UUID   string `json:"doc_uuid"`
	Count  int    `json:"count"`
	Copies []Copy `json:"-"`
}

// Copy is one page made by a duplicate.
type Copy struct {
	SourceUUID string
	UUID       string
	// ParentUUID is the page it's under: one of the other copies, or for the
	// top copy the page it was put under ("" at the top).
	ParentUUID string
	// Shared is a copy that takes the sharing of the page the duplicate goes
	// beside: the person may edit its source and every source above it in
	// the copy. Any other copy is theirs alone.
	Shared bool
}

// DuplicateDocs copies pages for the person: sources are the page and the
// pages under it to copy (the caller's choice, the page first), editable the
// ones of them the person may edit, and under is the page the copy goes right
// after the source under (nil: the top of the person's private pages), which
// the caller names only for a page the person may edit.
func DuplicateDocs(ctx context.Context, by *dgraphStruct.DgraphUser, sourceUUIDs []string, editable map[string]bool, under *dgraphStruct.DgraphDoc) (*Duplicated, error) {
	if len(sourceUUIDs) == 0 {
		return nil, ErrNotFound
	}
	if len(sourceUUIDs) > DuplicateCap {
		return nil, ErrTooMany
	}
	sources, err := domain.GetDocsForCopy(ctx, sourceUUIDs, by.Uid)
	if err != nil {
		return nil, err
	}
	byUUID := make(map[string]*dgraphStruct.DgraphDoc, len(sources))
	for _, s := range sources {
		byUUID[s.Uuid] = s
	}
	root := byUUID[sourceUUIDs[0]]
	if root == nil {
		return nil, ErrNotFound
	}

	unlock, err := graph.Lock(ctx, DocTree.Kind)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Where the top copy goes: right after the page, under its parent.
	var sibs []position.Sibling
	if under != nil {
		sibs, err = graph.Siblings(ctx, DocTree, under.Uuid, siblingCap)
	} else {
		var rows []graph.Row
		rows, err = docKind{}.Top(ctx, by.Uid, domain.SectionPrivate, siblingCap)
		for _, r := range rows {
			sibs = append(sibs, r.Sibling())
		}
		position.Order(sibs)
	}
	if errors.Is(err, graph.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	where := position.Where{}
	if under != nil {
		where.After = root.Uuid
	}
	placed, err := position.Place(sibs, "", where)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	nodes, copies := planCopies(by, sourceUUIDs, byUUID, editable, under, placed.Key, now)
	renumber := placed.Renumber
	if _, err := domain.CreateDocNodes(ctx, nodes, renumber, now); err != nil {
		return nil, err
	}
	unlock()

	for i, n := range nodes {
		// A body is seeded, which indexes the doc with it (UpdateDoc, or the
		// collaboration service's save); a page with none is indexed here.
		if body := byUUID[copies[i].SourceUUID].Body; body != "" {
			if err := seedBody(ctx, n.Doc.Uuid, body, by.Uuid); err != nil {
				helpers.LogErrorWithContext(ctx, "business/DuplicateDocs: the body of %s: %v", n.Doc.Uuid, err)
			}
		} else {
			syncDocSearch(n.Doc.Uuid, false)
			/* AI call omitted in v1 */
		}
		resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
			Kind: resourceEventBusiness.DocCreated, ResourceKind: "doc", ResourceUUID: n.Doc.Uuid,
			Actor: resourceEventBusiness.Actor{UserUUID: by.Uuid}, At: now,
			Data: map[string]string{"duplicate_of": copies[i].SourceUUID},
		})
	}
	return &Duplicated{UUID: copies[0].UUID, Count: len(copies), Copies: copies}, nil
}

// planCopies is the pages a duplicate makes, the page's copy first: sources
// by uuid, in order, the page first. Each copy is by's, with its source's
// title (the page's as "Copy of …"), look and place among its siblings, the
// page's at key, right after the page under under (nil: at the top).
//
// A copy is Shared, taking the space of the page it goes under here, and
// from the caller after the sharing of the page the duplicate goes beside
// (Rules.CopyOnCreate, once for all of them), only when by may edit its
// source and every source above it in the copy (editable). Any other copy is
// private and in no space, theirs alone wherever it sits, and so are the
// copies under it. A page whose parent isn't copied isn't either. Pure.
func planCopies(by *dgraphStruct.DgraphUser, order []string, sources map[string]*dgraphStruct.DgraphDoc, editable map[string]bool, under *dgraphStruct.DgraphDoc, key string, now time.Time) ([]domain.NewDocNode, []Copy) {
	zero := time.Time{}
	no := false
	root := order[0]
	index := map[string]int{} // source uuid -> its copy's index
	var nodes []domain.NewDocNode
	var copies []Copy
	for _, id := range order {
		s := sources[id]
		if s == nil {
			continue
		}
		parentIdx := -1
		if s.Uuid != root {
			if s.Parent == nil {
				continue
			}
			i, ok := index[s.Parent.Uuid]
			if !ok {
				// Its parent isn't copied, so neither is it.
				continue
			}
			parentIdx = i
		}
		title, pos := s.Title, s.Position
		if s.Uuid == root {
			if title != "" {
				title = CopyPrefix + title
			}
			pos = key
		}
		// What it goes under: one of the other copies, or the page's parent.
		var parent *dgraphStruct.DgraphDoc
		parentUUID := ""
		switch {
		case parentIdx >= 0:
			parent, parentUUID = nodes[parentIdx].Doc, copies[parentIdx].UUID
		case under != nil:
			parent, parentUUID = under, under.Uuid
		}
		shared := editable[s.Uuid] && ((parentIdx >= 0 && copies[parentIdx].Shared) || (parentIdx < 0 && under != nil))
		private := true
		var spaceKind, spaceUUID string
		if shared {
			spaceKind, spaceUUID = parent.SpaceKind, parent.SpaceUUID
			if parentIdx < 0 {
				// The page's copy beside the page is as private as its parent;
				// the copies under it take the same sharing after.
				private = parent.IsPrivate != nil && *parent.IsPrivate
			}
		}
		doc := &dgraphStruct.DgraphDoc{
			DType: []string{"Doc"}, Uuid: uuid.NewString(), Title: title, Position: pos,
			Icon: s.Icon, CoverKey: s.CoverKey, CoverOffset: s.CoverOffset, FullWidth: s.FullWidth, Font: s.Font, SmallText: s.SmallText,
			IsPrivate: &private, PublicComment: &no, SpaceKind: spaceKind, SpaceUUID: spaceUUID,
			CreatedBy: &dgraphStruct.DgraphUser{Uid: by.Uid}, Owner: &dgraphStruct.DgraphUser{Uid: by.Uid}, UpdatedBy: &dgraphStruct.DgraphUser{Uid: by.Uid},
			EditingUser: []*dgraphStruct.DgraphUser{{Uid: by.Uid}},
			CreatedAt:   &now, UpdatedAt: &now, DeletedAt: &zero,
		}
		n := domain.NewDocNode{Doc: doc, ParentRef: parentIdx}
		if parentIdx < 0 {
			n.ParentUUID = parentUUID
		}
		c := Copy{SourceUUID: s.Uuid, UUID: doc.Uuid, ParentUUID: parentUUID, Shared: shared}
		index[s.Uuid] = len(nodes)
		nodes = append(nodes, n)
		copies = append(copies, c)
	}
	return nodes, copies
}
