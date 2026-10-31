package businness

// Making a doc somewhere in the tree (POST /doc/createDoc, the second
// version): under a page (which the caller has checked the person may add
// to), first or last among its siblings or right after one, in a space, and
// from a template. A doc's title may be empty: "Untitled" is a placeholder
// the app shows, never a stored title (D-6).
//
// A subpage copies its parent's privacy and space here; its parent's sharing
// is the Access Resolver's to copy (CopyOnCreate), which the caller asks once
// the doc exists. A template's body is seeded through the Live Doc Writer,
// and written as the doc's first body while that isn't there yet: nobody has
// opened a doc made a moment ago, so no live copy can be overwritten.

import (
	"context"
	"errors"
	"strings"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/business/Tree/graph"
	"github.com/akashc777/OneCamp/business/Tree/position"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// NewDoc is what a new doc starts as, as POST /doc/createDoc takes it. The
// first version's fields (doc_title, doc_private) still read as they did.
type NewDoc struct {
	Title string `json:"doc_title"`
	// Private is the doc's privacy at the top of the tree (true when not
	// said); a subpage takes its parent's.
	Private    *bool  `json:"doc_private,omitempty"`
	ParentUUID string `json:"parent_uuid,omitempty"`
	// Space is where a doc at the top goes: "private", "workspace", or a
	// project's or team's space ("project:<uuid>"). A subpage is in its
	// parent's.
	Space      string `json:"space,omitempty"`
	TemplateID string `json:"template_id,omitempty"`
	// Icon is the emoji an import brings from the tool a page comes from. The
	// app sets a page's icon through its look, so a request never sets this.
	Icon      string `json:"-"`
	AfterUUID string `json:"after_uuid,omitempty"`
	// Place is "first" or "last" (the default) among its siblings, when no
	// AfterUUID is named.
	Place string `json:"place,omitempty"`
	// Placed says the maker puts the page's block in its parent's body itself
	// (the editor's "/page"), so PlaceInParent adds none.
	Placed bool `json:"placed,omitempty"`
	// Kind is the kind of node the request asks for (docs F1): "folder" or
	// "link", or none for a page. The controller maps it onto NodeKind, the
	// kind made, which is never read from a request as it is: a file is made
	// through AddFile.
	Kind     string `json:"node_kind,omitempty"`
	NodeKind string `json:"-"`
	// LinkURL is a link's address, checked by the caller
	// (domain/Doc CheckLinkURL, and again as the node is written).
	LinkURL string `json:"link_url,omitempty"`
	// UUID is the node's id when the server chose it first: a file's, which
	// its version 1 is recorded under before its node is made.
	UUID string `json:"-"`
}

// TemplateSource gives a doc template's body (the editor's HTML). B5 sets
// Templates to the gallery's; until then no template is known.
type TemplateSource interface {
	Body(ctx context.Context, templateID string) (string, error)
}

// ErrNoTemplate is a template nobody has.
var ErrNoTemplate = errors.New("there's no such template")

type noTemplates struct{}

func (noTemplates) Body(context.Context, string) (string, error) { return "", ErrNoTemplate }

// Templates is where a new doc's template body comes from.
var Templates TemplateSource = noTemplates{}

// subtreeCreateCap bounds the siblings read to place a new doc.
const siblingCap = 5000

// CreateDocInTree makes a doc for the person, where in the tree in says.
// parent is the page it goes under (the caller read it, and checked that the
// person may add to it), or nil for the top. ErrNotFound when the parent went
// to the trash meanwhile; ErrNotSibling when after names no sibling.
func CreateDocInTree(ctx context.Context, by *dgraphStruct.DgraphUser, in NewDoc, parent *dgraphStruct.DgraphDoc) (*dgraphStruct.DgraphDoc, error) {
	var template string
	// A template is a page's.
	if in.TemplateID != "" && in.NodeKind == "" {
		body, err := Templates.Body(ctx, in.TemplateID)
		if err != nil {
			return nil, err
		}
		template = body
	}

	unlock, err := graph.Lock(ctx, DocTree.Kind)
	if err != nil {
		return nil, err
	}
	defer unlock()

	newUUID := uuid.NewString()
	if id, err := uuid.Parse(in.UUID); err == nil {
		newUUID = id.String()
	}
	now := time.Now()
	zero := time.Time{}
	private := in.Private == nil || *in.Private
	noPublicComment := false
	doc := &dgraphStruct.DgraphDoc{
		DType:         []string{"Doc"},
		Uuid:          newUUID,
		Title:         strings.TrimSpace(in.Title),
		NodeKind:      in.NodeKind,
		LinkURL:       in.LinkURL,
		Icon:          in.Icon,
		CreatedBy:     &dgraphStruct.DgraphUser{Uid: by.Uid},
		Owner:         &dgraphStruct.DgraphUser{Uid: by.Uid},
		UpdatedBy:     &dgraphStruct.DgraphUser{Uid: by.Uid},
		EditingUser:   []*dgraphStruct.DgraphUser{{Uid: by.Uid}},
		PublicComment: &noPublicComment,
		CreatedAt:     &now,
		UpdatedAt:     &now,
		DeletedAt:     &zero,
	}

	var sibs []position.Sibling
	parentUUID := ""
	if parent != nil {
		parentUUID = parent.Uuid
		parentPrivate := parent.IsPrivate != nil && *parent.IsPrivate
		private = parentPrivate
		doc.SpaceKind, doc.SpaceUUID = parent.SpaceKind, parent.SpaceUUID
		sibs, err = graph.Siblings(ctx, DocTree, parent.Uuid, siblingCap)
		if errors.Is(err, graph.ErrNotFound) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
	} else {
		section, kind, id := TreeSection(in.Space)
		switch section {
		case domain.SectionSpace:
			doc.SpaceKind, doc.SpaceUUID = kind, id
		case domain.SectionWorkspace:
			if in.Space != "" {
				private = false
			}
		case domain.SectionPrivate:
			private = true
		}
		space := in.Space
		if space == "" {
			space = domain.SectionWorkspace
			if private {
				space = domain.SectionPrivate
			}
		}
		rows, err := docKind{}.Top(ctx, by.Uid, space, siblingCap)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			sibs = append(sibs, r.Sibling())
		}
		position.Order(sibs)
	}
	doc.IsPrivate = &private

	placed, err := position.Place(sibs, newUUID, position.Where{After: in.AfterUUID, First: in.AfterUUID == "" && in.Place == "first"})
	if err != nil {
		return nil, err
	}
	doc.Position = placed.Key

	uids, err := domain.CreateDocNodes(ctx, []domain.NewDocNode{{Doc: doc, ParentUUID: parentUUID, ParentRef: -1}}, placed.Renumber, now)
	if err != nil {
		return nil, err
	}
	doc.Uid = uids[0]
	doc.CreatedBy = by

	// A template's body is seeded, which indexes the doc with it; a blank doc
	// is indexed here.
	if template != "" {
		if _, err := seedBodyWith(ctx, newUUID, template, collab.Options{By: collab.By{UserUUID: by.Uuid}}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/CreateDocInTree: seeding the template of %s: %v", newUUID, err)
		}
		doc.Body = template
	} else {
		syncDocSearch(newUUID, false)
		// Only a page is embedded: a folder or a link has no text (docs F1).
		if domain.IsPage(doc) {
			/* AI call omitted in v1 */
		}
	}
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind: resourceEventBusiness.DocCreated, ResourceKind: "doc", ResourceUUID: newUUID,
		Actor: resourceEventBusiness.Actor{UserUUID: by.Uuid}, At: now,
		Data: map[string]string{"parent": parentUUID},
	})
	return doc, nil
}

// EmitMoved records that a doc moved (I14): to a new parent, or ("") to the
// top. A move that changed its space (space, nil for none) says into which
// and out of which, and names its owner when someone else moved it: they are
// told (business/Follow moved.go).
func EmitMoved(ctx context.Context, docUUID, parentUUID, byUUID string, space *SpaceChange) {
	resourceEventBusiness.Bus.Emit(ctx, movedEvent(docUUID, parentUUID, byUUID, space, time.Now()))
}

// movedEvent is EmitMoved's event. Pure.
func movedEvent(docUUID, parentUUID, byUUID string, space *SpaceChange, at time.Time) resourceEventBusiness.Event {
	e := resourceEventBusiness.Event{
		Kind: resourceEventBusiness.DocMoved, ResourceKind: "doc", ResourceUUID: docUUID,
		Actor: resourceEventBusiness.Actor{UserUUID: byUUID}, At: at,
		Data: map[string]string{"parent": parentUUID},
	}
	if space != nil {
		e.Data["space"], e.Data["was"] = space.To.Ref(), space.From.Ref()
		if space.Owner != "" && !strings.EqualFold(space.Owner, byUUID) {
			e.Subjects = []string{strings.ToLower(space.Owner)}
		}
	}
	return e
}
