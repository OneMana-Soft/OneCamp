//go:build integration
// +build integration

package integration_test

// A row names who made the doc's last change beside when (docs F1,
// DOCS-LEAD-2's ruling: a row's "Maya · 3h ago"), on a real graph with the
// production schema:
//   - a file's new version (domain.TouchDoc) writes when and who together,
//     for a person in the graph, both or neither;
//   - a doc in the trash takes neither;
//   - a tree's level (its rows' read, docRowFields) carries the last editor,
//     named, in the same read.
//
// Run: integ_rel.sh <wt> <log> '^TestARowNamesItsLastEditor$'

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	treeBusiness "github.com/akashc777/OneCamp/business/Tree"
	domainDoc "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestARowNamesItsLastEditor(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	ownerUUID, mayaUUID := uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": ownerUUID, "user_name": "owner"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": mayaUUID, "user_name": "Maya", "user_full_name": "Maya Chen"},
	})
	owner := uids["owner"]
	who := accessBusiness.Subject{UserUID: owner, UserUUID: ownerUUID}

	now := time.Now().UTC().Truncate(time.Second)
	zero := time.Time{}
	trashed := now.Add(-time.Hour)
	no := false
	node := func(title, kind, key string, deleted *time.Time) *dgraphStruct.DgraphDoc {
		return &dgraphStruct.DgraphDoc{DType: []string{"Doc"}, Uuid: uuid.NewString(), Title: title, NodeKind: kind, Position: key,
			IsPrivate: &no, PublicComment: &no, CreatedBy: &dgraphStruct.DgraphUser{Uid: owner}, Owner: &dgraphStruct.DgraphUser{Uid: owner},
			UpdatedBy: &dgraphStruct.DgraphUser{Uid: owner}, EditingUser: []*dgraphStruct.DgraphUser{{Uid: owner}}, CreatedAt: &now, UpdatedAt: &now, DeletedAt: deleted}
	}
	folder := node("Contracts", domainDoc.NodeFolder, "a0", &zero)
	file := node("msa.pdf", domainDoc.NodeFile, "a0", &zero)
	gone := node("old.pdf", domainDoc.NodeFile, "a1", &trashed)
	if _, err := domainDoc.CreateDocNodes(ctx, []domainDoc.NewDocNode{{Doc: folder, ParentRef: -1}, {Doc: file, ParentRef: 0}, {Doc: gone, ParentRef: -1}}, nil, now); err != nil {
		t.Fatal(err)
	}

	type changed struct {
		At *time.Time `json:"doc_updated_at"`
		By *struct {
			UUID string `json:"user_uuid"`
			Name string `json:"user_name"`
		} `json:"doc_updated_by"`
	}
	read := func(t *testing.T, docUUID string) changed {
		t.Helper()
		got, err := dgexec.List[changed](ctx, `query q($id: string) {
			got(func: eq(doc_uuid, $id)) { doc_updated_at doc_updated_by { user_uuid user_name } }
		}`, map[string]string{"$id": docUUID}, "got")
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: %+v %v", docUUID, got, err)
		}
		return got[0]
	}
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	t.Run("a new version names its uploader beside its time, in one write", func(t *testing.T) {
		if err := domainDoc.TouchDoc(ctx, file.Uuid, mayaUUID, at); err != nil {
			t.Fatal(err)
		}
		got := read(t, file.Uuid)
		if got.At == nil || !got.At.Equal(at) || got.By == nil || got.By.UUID != mayaUUID || got.By.Name != "Maya" {
			t.Errorf("the file's last change: %+v %+v, want Maya at %v", got.At, got.By, at)
		}
	})

	t.Run("both or neither: nobody the graph knows, a doc in the trash", func(t *testing.T) {
		later := at.Add(time.Hour)
		if err := domainDoc.TouchDoc(ctx, file.Uuid, uuid.NewString(), later); err != nil {
			t.Fatal(err)
		}
		if got := read(t, file.Uuid); got.At == nil || !got.At.Equal(at) || got.By == nil || got.By.UUID != mayaUUID {
			t.Errorf("a touch by nobody the graph knows changed the file: %+v %+v", got.At, got.By)
		}
		if err := domainDoc.TouchDoc(ctx, gone.Uuid, mayaUUID, later); err != nil {
			t.Fatal(err)
		}
		if got := read(t, gone.Uuid); got.At == nil || !got.At.Equal(now) || got.By == nil || got.By.UUID != ownerUUID {
			t.Errorf("a doc in the trash took a touch: %+v %+v", got.At, got.By)
		}
	})

	t.Run("a tree's rows carry the last editor, named, in the same read", func(t *testing.T) {
		lister := treeBusiness.Hierarchy.(treeBusiness.Lister)
		level, err := lister.Level(ctx, who, accessBusiness.KindDoc, treeBusiness.LevelQuery{Parent: folder.Uuid})
		if err != nil || len(level.Nodes) != 1 {
			t.Fatalf("the folder's level: %+v %v; want the file", level, err)
		}
		d, _ := level.Nodes[0].Data.(*dgraphStruct.DgraphDoc)
		if d == nil || d.UpdatedBy == nil || d.UpdatedBy.Uuid != mayaUUID || d.UpdatedBy.UserName != "Maya" || d.UpdatedBy.UserFullName != "Maya Chen" {
			t.Errorf("the file's row names %+v as its last editor", d)
		}
	})
}
