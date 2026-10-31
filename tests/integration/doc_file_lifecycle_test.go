//go:build integration
// +build integration

package integration_test

// A doc tree file's versions follow the file (docs F1), on Postgres 12 and a
// real graph:
//   - a level's file facts are its newest version (FileFactsOf);
//   - a duplicate's copy of a file takes its newest version, on the same
//     stored object, as its own version 1;
//   - a delete for good takes the file's versions and archives their
//     attachments rows, so the attachments purge takes the bytes, while the
//     copy's row on the same object stays live.
//
// Run: integ_rel.sh <wt> <log> '^TestAFilesVersionsFollowItsCopyAndItsPurge$'

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	domainDoc "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	docFileModels "github.com/akashc777/OneCamp/models/postgres/DocFile"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAFilesVersionsFollowItsCopyAndItsPurge(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	owner := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'owner', 'Owner', NOW(), NOW())`, owner, owner.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	uids := dg.Mutate(t, []map[string]any{{"uid": "_:owner", "dgraph.type": "User", "user_uuid": owner.String(), "user_name": "owner"}})
	ownerUID := uids["owner"]
	by := &dgraphStruct.DgraphUser{Uid: ownerUID, Uuid: owner.String(), UserName: "owner"}

	now := time.Now()
	zero := time.Time{}
	yes, no := true, false
	node := func(title, kind, key string) *dgraphStruct.DgraphDoc {
		return &dgraphStruct.DgraphDoc{DType: []string{"Doc"}, Uuid: uuid.NewString(), Title: title, NodeKind: kind, Position: key,
			IsPrivate: &yes, PublicComment: &no, CreatedBy: &dgraphStruct.DgraphUser{Uid: ownerUID}, Owner: &dgraphStruct.DgraphUser{Uid: ownerUID},
			EditingUser: []*dgraphStruct.DgraphUser{{Uid: ownerUID}}, CreatedAt: &now, UpdatedAt: &now, DeletedAt: &zero}
	}
	folder, file := node("Contracts", domainDoc.NodeFolder, "a0"), node("msa.pdf", domainDoc.NodeFile, "a0")
	if _, err := domainDoc.CreateDocNodes(ctx, []domainDoc.NewDocNode{{Doc: folder, ParentRef: -1}, {Doc: file, ParentRef: 0}}, nil, now); err != nil {
		t.Fatal(err)
	}
	fileID := uuid.MustParse(file.Uuid)
	seedFileVersion(t, env.PG, fileID, 1, "msa.pdf", owner)
	seedFileVersion(t, env.PG, fileID, 2, "msa-signed.pdf", owner)

	facts, err := docBusiness.FileFactsOf(ctx, []*dgraphStruct.DgraphDoc{folder, file})
	if err != nil || len(facts) != 1 || facts[file.Uuid].Name != "msa-signed.pdf" || facts[file.Uuid].Versions != 2 {
		t.Fatalf("the level's file facts: %+v %v", facts, err)
	}

	dup, err := docBusiness.DuplicateDocs(ctx, by, []string{folder.Uuid, file.Uuid}, map[string]bool{folder.Uuid: true, file.Uuid: true}, nil)
	if err != nil || dup.Count != 2 {
		t.Fatalf("a duplicate of the folder: %+v %v", dup, err)
	}
	var fileCopy string
	for _, c := range dup.Copies {
		if c.SourceUUID == file.Uuid {
			fileCopy = c.UUID
		}
	}
	rows, err := docBusiness.DocRows(ctx, []string{fileCopy}, ownerUID)
	if err != nil || len(rows) != 1 || rows[0].NodeKind != domainDoc.NodeFile {
		t.Fatalf("the file's copy: %+v %v", rows, err)
	}
	copied, err := docBusiness.FileFactsOf(ctx, rows)
	if err != nil || copied[fileCopy] == nil || copied[fileCopy].Versions != 1 || copied[fileCopy].Name != "msa-signed.pdf" {
		t.Fatalf("the copy's file: %+v %v", copied, err)
	}
	var sameObject bool
	if err := env.PG.QueryRow(`SELECT (SELECT obj_key FROM attachments WHERE id = $1::uuid) = (SELECT obj_key FROM attachments WHERE id = $2::uuid)`,
		copied[fileCopy].AttachmentID, facts[file.Uuid].AttachmentID).Scan(&sameObject); err != nil || !sameObject {
		t.Errorf("the copy's version isn't on its source's object: %v %v", sameObject, err)
	}

	// The file to the trash, and gone for good.
	if _, err := docBusiness.TrashSubtree(ctx, file.Uuid, ownerUID, owner.String(), owner.String()+"@example.test"); err != nil {
		t.Fatalf("to the trash: %v", err)
	}
	if _, err := docBusiness.PurgeDoc(ctx, file.Uuid, ownerUID, owner.String(), false); err != nil {
		t.Fatalf("gone for good: %v", err)
	}
	if left, err := docFileModels.CurrentOf(ctx, []uuid.UUID{fileID}); err != nil || len(left) != 0 {
		t.Errorf("the purged file still has versions: %+v %v", left, err)
	}
	var archived, live int
	if err := env.PG.QueryRow(`SELECT count(*) FILTER (WHERE deleted_at IS NOT NULL), count(*) FILTER (WHERE deleted_at IS NULL)
		FROM attachments WHERE src_value IN ($1, $2)`, file.Uuid, fileCopy).Scan(&archived, &live); err != nil {
		t.Fatal(err)
	}
	if archived != 2 || live != 1 {
		t.Errorf("after the purge, %d of the file's rows archived and %d live; want its 2 archived and the copy's 1 live", archived, live)
	}
}
