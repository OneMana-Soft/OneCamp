//go:build integration
// +build integration

package integration_test

// Who finds a doc in global search, against a real OpenSearch mapped as the
// server maps it, a real graph and a real Postgres.
//
// The docs index was patched through a shape that leaves out false and empty
// values, and global search trusted it:
//   - no public doc said it was public, so nobody but its editors found one;
//   - a doc made public again stayed private in the index;
//   - someone a doc stopped being shared with kept finding its title and text;
//   - a new comment on a doc was written as deleted, and with no one to find
//     it, its doc's owner included.
// Now entries are written whole from the doc, every doc hit is checked against
// the graph before it is shown, and a startup repair rewrites what the index
// already holds.
//
// Run: go test -tags=integration ./tests/integration/ -run TestGlobalSearchFindsDocsByWhoMayReadThemNow -v

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestGlobalSearchFindsDocsByWhoMayReadThemNow(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	// Every index, mapped as the server maps them at boot.
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	client := opensearchInit.OpenSearchClient

	people := map[string]uuid.UUID{"owner": uuid.New(), "member": uuid.New(), "reader": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	uids := dg.Mutate(t, nodes)
	person := func(name string) *dgraphStruct.DgraphUser {
		return &dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].String(), UserName: name}
	}
	info := func(name string) *userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserDgraphInfo = *person(name)
		return &u
	}

	// finds: does a search for text, as the person, return the object?
	finds := func(who, text, id string) bool {
		t.Helper()
		p, err := globalSearchBusiness.GetUnifiedGlobalSearch(ctx, accessBusiness.Subject{UserUUID: people[who].String()}, people[who].String()+"@example.test", nil, nil, nil, text)
		if err != nil {
			t.Fatalf("searching %q as %s: %v", text, who, err)
		}
		for _, h := range p.Page {
			switch {
			case h.Doc != nil && h.Doc.Uuid == id, h.Comment != nil && h.Comment.Uuid == id:
				return true
			}
		}
		return false
	}
	// eventually waits for a search to give the answer wanted (entries are
	// written in the background, and the index shows them within a second).
	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("timed out waiting until %s", what)
	}
	entry := func(index, id string) map[string]any {
		t.Helper()
		resp, err := client.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: index, DocumentID: id})
		if err != nil {
			return nil
		}
		raw, _ := io.ReadAll(resp.Inspect().Response.Body)
		var got struct {
			Found  bool           `json:"found"`
			Source map[string]any `json:"_source"`
		}
		_ = json.Unmarshal(raw, &got)
		if !got.Found {
			return nil
		}
		return got.Source
	}
	write := func(index, id string, source map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(source)
		if _, err := client.Index(ctx, opensearchapi.IndexReq{Index: index, DocumentID: id, Body: strings.NewReader(string(raw)),
			Params: opensearchapi.IndexParams{Refresh: "true"}}); err != nil {
			t.Fatal(err)
		}
	}

	handbook, err := docBusiness.CreateDoc(ctx, person("owner"), &docAdapter.InputCreateDoc{DocTitle: "Engineering handbook", DocPrivate: false})
	if err != nil {
		t.Fatal(err)
	}
	salaries, err := docBusiness.CreateDoc(ctx, person("owner"), &docAdapter.InputCreateDoc{DocTitle: "Salaries review", DocPrivate: true})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a public doc is found by everyone", func(t *testing.T) {
		eventually("the member finds the public handbook", func() bool { return finds("member", "handbook", handbook.Uuid) })
		if finds("member", "salaries", salaries.Uuid) {
			t.Error("a member finds a private doc that isn't shared with them")
		}
		if !finds("owner", "salaries", salaries.Uuid) {
			t.Error("the owner doesn't find their own private doc")
		}
	})

	t.Run("someone a doc stops being shared with stops finding it", func(t *testing.T) {
		if err := docBusiness.UpdateDocPermissions(ctx, docAdapter.InputUpdateDocPermissions{DocId: salaries.Uuid, AddViewers: []string{people["reader"].String()}}, uids["owner"]); err != nil {
			t.Fatal(err)
		}
		eventually("the reader finds the doc shared with them", func() bool { return finds("reader", "salaries", salaries.Uuid) })
		if err := docBusiness.UpdateDocPermissions(ctx, docAdapter.InputUpdateDocPermissions{DocId: salaries.Uuid, RemoveViewers: []string{people["reader"].String()}}, uids["owner"]); err != nil {
			t.Fatal(err)
		}
		eventually("the entry no longer names the reader", func() bool {
			e := entry("docs", salaries.Uuid)
			readers, ok := e["doc_reading_users"].([]any)
			return ok && len(readers) == 0
		})
		if finds("reader", "salaries", salaries.Uuid) {
			t.Error("the reader still finds the doc after it stopped being shared with them")
		}
	})

	t.Run("an entry still behind is checked against the doc", func(t *testing.T) {
		// As an index that missed the change would hold it: the reader on it.
		stale := entry("docs", salaries.Uuid)
		stale["doc_reading_users"] = []string{people["reader"].String()}
		write("docs", salaries.Uuid, stale)
		if finds("reader", "salaries", salaries.Uuid) {
			t.Error("global search showed a private doc on the index's word, without asking the doc")
		}
	})

	t.Run("a doc made public is found by everyone", func(t *testing.T) {
		public := false
		if err := docBusiness.UpdateDoc(ctx, &docAdapter.InputUpdateDoc{DocId: salaries.Uuid, IsPrivate: &public}); err != nil {
			t.Fatal(err)
		}
		eventually("the member finds the doc made public", func() bool { return finds("member", "salaries", salaries.Uuid) })
	})

	t.Run("a new comment on a doc is found by those who may read the doc", func(t *testing.T) {
		private := true
		if err := docBusiness.UpdateDoc(ctx, &docAdapter.InputUpdateDoc{DocId: salaries.Uuid, IsPrivate: &private}); err != nil {
			t.Fatal(err)
		}
		eventually("the doc is private again", func() bool { return entry("docs", salaries.Uuid)["doc_private"] == true })
		doc, err := docBusiness.GetBasicDgraphDocByUUID(ctx, salaries.Uuid, uids["owner"])
		if err != nil {
			t.Fatal(err)
		}
		c, err := docBusiness.CreateDocComment(ctx, doc, info("owner"), &docAdapter.CreateOrUpdateDocCommentInput{DocUuid: salaries.Uuid, CommentBody: "<p>budget freeze until March</p>"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		eventually("the owner finds their comment on their private doc", func() bool { return finds("owner", "budget freeze", c.Uuid) })
		if finds("member", "budget freeze", c.Uuid) {
			t.Error("a member finds a comment on a private doc that isn't shared with them")
		}
		hb, err := docBusiness.GetBasicDgraphDocByUUID(ctx, handbook.Uuid, uids["member"])
		if err != nil {
			t.Fatal(err)
		}
		pc, err := docBusiness.CreateDocComment(ctx, hb, info("member"), &docAdapter.CreateOrUpdateDocCommentInput{DocUuid: handbook.Uuid, CommentBody: "<p>onboarding checklist link</p>"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		eventually("everyone finds a comment on a public doc", func() bool { return finds("reader", "onboarding checklist", pc.Uuid) })
	})

	t.Run("a deleted doc is found by nobody", func(t *testing.T) {
		// Deleting is putting in the trash (A3), as the app's delete does.
		owner := info("owner")
		if _, err := docBusiness.TrashSubtree(ctx, handbook.Uuid, owner.UserDgraphInfo.Uid, owner.UserDgraphInfo.Uuid, owner.UserPostgresInfo.EmailID); err != nil {
			t.Fatal(err)
		}
		eventually("the handbook's entry says it is deleted", func() bool { return entry("docs", handbook.Uuid)["deleted_date"] != nil })
		if finds("owner", "handbook", handbook.Uuid) {
			t.Error("its owner finds a deleted doc")
		}
	})

	t.Run("the startup repair rewrites what the index holds", func(t *testing.T) {
		roadmap, err := docBusiness.CreateDoc(ctx, person("owner"), &docAdapter.InputCreateDoc{DocTitle: "Product roadmap", DocPrivate: false})
		if err != nil {
			t.Fatal(err)
		}
		old, err := docBusiness.CreateDoc(ctx, person("owner"), &docAdapter.InputCreateDoc{DocTitle: "Launch checklist", DocPrivate: false})
		if err != nil {
			t.Fatal(err)
		}
		eventually("both new docs have entries", func() bool { return entry("docs", roadmap.Uuid) != nil && entry("docs", old.Uuid) != nil })
		// The state an install is in before the repair: an entry written the
		// old way (no doc_private), one the index never had, and a comment on
		// a live doc written as deleted with none of its doc's sharing.
		write("docs", old.Uuid, map[string]any{"doc_uuid": old.Uuid, "doc_title": "Launch checklist", "doc_created_by_user_id": people["owner"].String(),
			"doc_editing_users": []string{people["owner"].String()}, "created_date": time.Now().Unix()})
		if _, err := client.Document.Delete(ctx, opensearchapi.DocumentDeleteReq{Index: "docs", DocumentID: roadmap.Uuid, Params: opensearchapi.DocumentDeleteParams{Refresh: "true"}}); err != nil {
			t.Fatal(err)
		}
		rd, err := docBusiness.GetBasicDgraphDocByUUID(ctx, roadmap.Uuid, uids["owner"])
		if err != nil {
			t.Fatal(err)
		}
		c, err := docBusiness.CreateDocComment(ctx, rd, info("owner"), &docAdapter.CreateOrUpdateDocCommentInput{DocUuid: roadmap.Uuid, CommentBody: "<p>quarterly milestones agreed</p>"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		eventually("the comment's entry is written", func() bool { return entry("comments", c.Uuid) != nil })
		ce := entry("comments", c.Uuid)
		ce["deleted_date"] = -1
		delete(ce, "comment_doc_private")
		delete(ce, "comment_doc_created_by_user_id")
		write("comments", c.Uuid, ce)
		if finds("member", "launch checklist", old.Uuid) || finds("member", "roadmap", roadmap.Uuid) || finds("member", "quarterly milestones", c.Uuid) {
			t.Fatal("the broken entries were findable before the repair; the test isn't testing it")
		}

		rep, ran, err := docBusiness.SearchRepair.Run(ctx)
		if err != nil || !ran {
			t.Fatalf("the repair: ran %v, %v (%s)", ran, err, rep)
		}
		t.Logf("repair: %s", rep)
		if rep.Docs != 4 || rep.Live != 3 || rep.Deleted != 1 || rep.Revived != 1 {
			t.Errorf("the repair wrote %s; want 4 doc entries (3 live, 1 deleted) and 1 comment findable again", rep)
		}
		eventually("the repaired docs and comment are found", func() bool {
			return finds("member", "launch checklist", old.Uuid) && finds("member", "roadmap", roadmap.Uuid) && finds("member", "quarterly milestones", c.Uuid)
		})
		if finds("owner", "handbook", handbook.Uuid) {
			t.Error("the repair brought a deleted doc back")
		}

		// Recorded, so later starts skip it; and run again, it finds nothing to change.
		if _, ran, err := docBusiness.SearchRepair.Run(ctx); err != nil || ran {
			t.Errorf("a second start ran the repair again (%v, %v)", ran, err)
		}
		again, err := docBusiness.RepairDocSearch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if again.Docs != 4 || again.Children != 0 || again.Revived != 0 {
			t.Errorf("run again: %s; want the same 4 entries rewritten and nothing else to change", again)
		}
	})
}
