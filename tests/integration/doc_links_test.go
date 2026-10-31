//go:build integration
// +build integration

package integration_test

// Folders and links made in the tree (docs F1, POST /doc/createDoc's
// business), on Postgres 12 and a real graph: each is made with its kind (a
// link with its address, called by it), a link is named after its preview
// only while it is still called by its address, and a link holds nothing.
//
// Run: integ_rel.sh <wt> <log> '^TestAFolderAndALinkAreMadeAndTheLinkNamedOnce$'

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/business/Tree/graph"
	domainDoc "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAFolderAndALinkAreMadeAndTheLinkNamedOnce(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	owner := uuid.New()
	uids := dg.Mutate(t, []map[string]any{{"uid": "_:owner", "dgraph.type": "User", "user_uuid": owner.String(), "user_name": "owner"}})
	by := &dgraphStruct.DgraphUser{Uid: uids["owner"], Uuid: owner.String(), UserName: "owner"}

	folder, err := docBusiness.CreateDocInTree(ctx, by, docBusiness.NewDoc{Title: "Contracts", Space: "private", NodeKind: docBusiness.NodeFolder}, nil)
	if err != nil || folder.NodeKind != domainDoc.NodeFolder {
		t.Fatalf("a folder: %+v %v", folder, err)
	}
	address := "https://docs.google.com/document/d/1AbC/edit"
	link, err := docBusiness.CreateDocInTree(ctx, by, docBusiness.NewDoc{Title: address, NodeKind: docBusiness.NodeLink, LinkURL: address}, folder)
	if err != nil {
		t.Fatalf("a link in the folder: %v", err)
	}
	rows, err := docBusiness.DocRows(ctx, []string{link.Uuid}, uids["owner"])
	if err != nil || len(rows) != 1 || rows[0].NodeKind != domainDoc.NodeLink || rows[0].LinkURL != address || rows[0].Title != address ||
		rows[0].Parent == nil || rows[0].Parent.Uuid != folder.Uuid {
		t.Fatalf("the link as made: %+v %v", rows, err)
	}

	if err := docBusiness.NameLink(ctx, link.Uuid, address, "Q3 roadmap", docBusiness.Actor{UserUUID: owner.String()}); err != nil {
		t.Fatal(err)
	}
	// Renamed by someone since: a preview read again names nothing.
	if renamed, err := domainDoc.RetitleLink(ctx, link.Uuid, address, "Another title"); err != nil || renamed {
		t.Errorf("a second naming: %v %v", renamed, err)
	}
	// No live doc with the uuid: nothing is named, and nothing went wrong.
	if renamed, err := domainDoc.RetitleLink(ctx, uuid.NewString(), address, "Nowhere"); err != nil || renamed {
		t.Errorf("naming no doc: %v %v", renamed, err)
	}
	rows, err = docBusiness.DocRows(ctx, []string{link.Uuid}, uids["owner"])
	if err != nil || len(rows) != 1 || rows[0].Title != "Q3 roadmap" {
		t.Errorf("the link's title: %+v %v", rows, err)
	}

	if _, err := docBusiness.CreateDocInTree(ctx, by, docBusiness.NewDoc{Title: "Inside"}, rows[0]); !errors.Is(err, graph.ErrLeaf) {
		t.Errorf("a page under the link: %v, want ErrLeaf", err)
	}
}
