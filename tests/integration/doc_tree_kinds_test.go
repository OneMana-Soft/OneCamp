//go:build integration
// +build integration

package integration_test

// The doc tree knows its nodes' kinds (docs F1), on a real graph with the
// production schema:
//   - only a page or a folder holds children (D-30): a page made or moved
//     under a file or a link is refused, a move's preview too, and the
//     move's own write refuses it again;
//   - the doc's page read (getDocInfo), every decision and every tree row
//     carry the kind, colour and link;
//   - a guest's live connection opens pages only.
//
// Run: integ_rel.sh <wt> <log> '^TestTheDocTreeKnowsItsNodesKinds$'

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	treeBusiness "github.com/akashc777/OneCamp/business/Tree"
	"github.com/akashc777/OneCamp/business/Tree/graph"
	domainDoc "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheDocTreeKnowsItsNodesKinds(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	ownerUUID := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{{"uid": "_:owner", "dgraph.type": "User", "user_uuid": ownerUUID, "user_name": "owner"}})
	owner := uids["owner"]
	by := &dgraphStruct.DgraphUser{Uid: owner, Uuid: ownerUUID, UserName: "owner"}
	who := accessBusiness.Subject{UserUID: owner, UserUUID: ownerUUID}

	now := time.Now()
	zero := time.Time{}
	no := false
	node := func(title, kind, key string) *dgraphStruct.DgraphDoc {
		d := &dgraphStruct.DgraphDoc{DType: []string{"Doc"}, Uuid: uuid.NewString(), Title: title, NodeKind: kind, Position: key,
			IsPrivate: &no, PublicComment: &no, CreatedBy: &dgraphStruct.DgraphUser{Uid: owner}, Owner: &dgraphStruct.DgraphUser{Uid: owner},
			EditingUser: []*dgraphStruct.DgraphUser{{Uid: owner}}, CreatedAt: &now, UpdatedAt: &now, DeletedAt: &zero}
		return d
	}
	folder := node("Contracts", domainDoc.NodeFolder, "a0")
	folder.Color = "moss"
	file := node("msa.pdf", domainDoc.NodeFile, "a0")
	link := node("Roadmap", domainDoc.NodeLink, "a1")
	link.Color, link.LinkURL = "sky", "https://docs.google.com/document/d/1"
	page := node("Plan", "", "a2")
	if _, err := domainDoc.CreateDocNodes(ctx, []domainDoc.NewDocNode{
		{Doc: folder, ParentRef: -1}, {Doc: file, ParentRef: 0}, {Doc: link, ParentRef: -1}, {Doc: page, ParentRef: -1},
	}, nil, now); err != nil {
		t.Fatal(err)
	}

	t.Run("a file or a link holds nothing", func(t *testing.T) {
		for _, leaf := range []*dgraphStruct.DgraphDoc{file, link} {
			move := treeBusiness.Move{Kind: accessBusiness.KindDoc, UUID: page.Uuid, ParentUUID: leaf.Uuid}
			if _, err := treeBusiness.Hierarchy.Move(ctx, who, move); !errors.Is(err, treeBusiness.ErrLeaf) {
				t.Errorf("a page moved under the %s: %v, want ErrLeaf", leaf.NodeKind, err)
			}
			move.DryRun = true
			if _, err := treeBusiness.Hierarchy.Move(ctx, who, move); !errors.Is(err, treeBusiness.ErrLeaf) {
				t.Errorf("a move's preview under the %s: %v, want ErrLeaf", leaf.NodeKind, err)
			}
			if _, err := docBusiness.CreateDocInTree(ctx, by, docBusiness.NewDoc{Title: "Inside"}, leaf); !errors.Is(err, graph.ErrLeaf) {
				t.Errorf("a page made under the %s: %v, want ErrLeaf", leaf.NodeKind, err)
			}
			// The move's own write refuses it again, whatever was read first.
			if err := graph.Apply(ctx, docBusiness.DocTree, graph.Move{UUID: page.Uuid, Parent: leaf.Uuid, Key: "a5"}, time.Now()); !errors.Is(err, graph.ErrLeaf) {
				t.Errorf("the write of a move under the %s: %v, want ErrLeaf", leaf.NodeKind, err)
			}
		}
		if _, err := treeBusiness.Hierarchy.Move(ctx, who, treeBusiness.Move{Kind: accessBusiness.KindDoc, UUID: page.Uuid, ParentUUID: folder.Uuid}); err != nil {
			t.Errorf("a page moved into the folder: %v", err)
		}
	})

	t.Run("the page read, decisions and rows carry the kind", func(t *testing.T) {
		got, err := docBusiness.GetDocByDocUUID(ctx, link.Uuid, owner)
		if err != nil || got.NodeKind != domainDoc.NodeLink || got.Color != "sky" || got.LinkURL != link.LinkURL {
			t.Errorf("the link's page read: %+v %v", got, err)
		}
		decided, err := docBusiness.DecideDocs(ctx, docBusiness.Viewer{UID: owner, UUID: ownerUUID}, []string{folder.Uuid, page.Uuid})
		if err != nil || decided[folder.Uuid].NodeKind != domainDoc.NodeFolder || decided[page.Uuid].NodeKind != "" {
			t.Errorf("decisions: %+v %v", decided, err)
		}
		lister := treeBusiness.Hierarchy.(treeBusiness.Lister)
		level, err := lister.Level(ctx, who, accessBusiness.KindDoc, treeBusiness.LevelQuery{Parent: folder.Uuid})
		if err != nil || len(level.Nodes) != 2 {
			t.Fatalf("the folder's level: %+v %v; want the file and the page", level, err)
		}
		for _, n := range level.Nodes {
			d, _ := n.Data.(*dgraphStruct.DgraphDoc)
			if d == nil || (n.UUID == file.Uuid && d.NodeKind != domainDoc.NodeFile) || (n.UUID == page.Uuid && d.NodeKind != "") {
				t.Errorf("a row of the folder: %+v %+v", n, d)
			}
		}
	})

	t.Run("a guest's live connection opens pages only", func(t *testing.T) {
		if err := docBusiness.CheckPageLive(ctx, folder.Uuid); !errors.Is(err, docBusiness.ErrNotAPage) {
			t.Errorf("the folder: %v, want ErrNotAPage", err)
		}
		if err := docBusiness.CheckPageLive(ctx, page.Uuid); err != nil {
			t.Errorf("the page: %v", err)
		}
	})
}
