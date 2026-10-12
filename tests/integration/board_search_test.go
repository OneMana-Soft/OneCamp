//go:build integration
// +build integration

package integration_test

// Who finds a board, and its comments, in global search, against a real
// OpenSearch mapped as the server maps it, a real graph and a real Postgres.
//
// A board comment's entry carries a copy of its board's privacy and sharing
// from when the comment was written, and global search trusted it: a comment
// on a board that was public stayed findable by everyone after the board was
// made private, and a deleted board's comments by everyone when the delete's
// cascade missed them. Now every hit on a board, or on a comment on one, is
// checked against the board before it is shown, and a change to the board's
// privacy or sharing rewrites its comments' copies.
//
// Run: go test -tags=integration ./tests/integration/ -run TestGlobalSearchFindsBoardCommentsByWhoMayReadTheBoardNow -v

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	boardAdapter "github.com/akashc777/OneCamp/adapter/Board"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestGlobalSearchFindsBoardCommentsByWhoMayReadTheBoardNow(t *testing.T) {
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

	people := map[string]uuid.UUID{"owner": uuid.New(), "member": uuid.New()}
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

	// finds: does a search for text, as the person, return the board or the
	// comment (by id)?
	finds := func(t *testing.T, who, text, id string) bool {
		t.Helper()
		p, err := globalSearchBusiness.GetUnifiedGlobalSearch(ctx, people[who].String(), people[who].String()+"@example.test", nil, nil, nil, text)
		if err != nil {
			t.Fatalf("searching %q as %s: %v", text, who, err)
		}
		for _, h := range p.Page {
			switch {
			case h.Board != nil && h.Board.Uuid == id, h.Comment != nil && h.Comment.Uuid == id:
				return true
			}
		}
		return false
	}
	// eventually waits until cond holds (entries are written in the
	// background, and the index shows them within a second).
	eventually := func(t *testing.T, what string, cond func() bool) {
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
	write := func(t *testing.T, index, id string, source map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(source)
		if _, err := client.Index(ctx, opensearchapi.IndexReq{Index: index, DocumentID: id, Body: strings.NewReader(string(raw)),
			Params: opensearchapi.IndexParams{Refresh: "true"}}); err != nil {
			t.Fatal(err)
		}
	}
	// comment writes a comment on a board as its owner, as the canvas does,
	// and answers the id its search entry is written under.
	comment := func(t *testing.T, board, yjsID, text string) string {
		t.Helper()
		if err := boardBusiness.SyncBoardComment(ctx, person("owner"), people["owner"], board, yjsID, text, nil); err != nil {
			t.Fatal(err)
		}
		return uuid.NewSHA1(uuid.MustParse("b0a4d1e2-0000-4000-8000-000000000001"), []byte(board+":"+yjsID)).String()
	}

	plans, err := boardBusiness.CreateBoard(ctx, person("owner"), &boardAdapter.InputCreateBoard{BoardTitle: "Reorg plans", BoardPrivate: false})
	if err != nil {
		t.Fatal(err)
	}
	merge := comment(t, plans.Uuid, "yjs-1", "merge the two platform teams")

	t.Run("a comment on a public board is found by everyone", func(t *testing.T) {
		eventually(t, "the member finds the comment", func() bool { return finds(t, "member", "platform", merge) })
		eventually(t, "the member finds the board", func() bool { return finds(t, "member", "reorg", plans.Uuid) })
	})

	t.Run("a board made private takes its comments out of everyone else's search", func(t *testing.T) {
		private := true
		if err := boardBusiness.UpdateBoard(ctx, &boardAdapter.InputUpdateBoard{BoardId: plans.Uuid, IsPrivate: &private}); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the board's entry says it is private", func() bool { return entry("boards", plans.Uuid)["board_private"] == true })
		eventually(t, "the comment's entry says its board is private", func() bool {
			e := entry("comments", merge)
			return e != nil && e["comment_board_public"] == false
		})
		if finds(t, "member", "platform", merge) {
			t.Error("the member still finds a comment on a board made private")
		}
		if !finds(t, "owner", "reorg", plans.Uuid) {
			t.Error("the owner doesn't find their own private board")
		}
	})

	t.Run("entries still behind are checked against the board", func(t *testing.T) {
		// As an index that missed the change would hold them: public.
		stale := entry("comments", merge)
		if stale == nil {
			t.Fatal("the comment has no entry")
		}
		stale["comment_board_public"] = true
		write(t, "comments", merge, stale)
		staleBoard := entry("boards", plans.Uuid)
		if staleBoard == nil {
			t.Fatal("the board has no entry")
		}
		staleBoard["board_private"] = false
		write(t, "boards", plans.Uuid, staleBoard)
		if finds(t, "member", "platform", merge) {
			t.Error("global search showed a private board's comment on the index's word, without asking the board")
		}
		if finds(t, "member", "reorg", plans.Uuid) {
			t.Error("global search showed a private board on the index's word, without asking the board")
		}
	})

	t.Run("a deleted board's comments are found by nobody", func(t *testing.T) {
		retro, err := boardBusiness.CreateBoard(ctx, person("owner"), &boardAdapter.InputCreateBoard{BoardTitle: "Sprint retro", BoardPrivate: false})
		if err != nil {
			t.Fatal(err)
		}
		lessons := comment(t, retro.Uuid, "yjs-2", "deploy freeze lessons learned")
		eventually(t, "the member finds the comment on the live board", func() bool { return finds(t, "member", "lessons", lessons) })
		if err := boardBusiness.DeleteBoard(ctx, retro.Uuid); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the comment's entry says it is deleted", func() bool {
			e := entry("comments", lessons)
			return e != nil && e["deleted_date"] != nil
		})
		// As an index the delete's cascade missed would hold it: live.
		stale := entry("comments", lessons)
		delete(stale, "deleted_date")
		delete(stale, "deleted_by")
		write(t, "comments", lessons, stale)
		if finds(t, "member", "lessons", lessons) {
			t.Error("global search showed a deleted board's comment")
		}
		if finds(t, "owner", "lessons", lessons) {
			t.Error("its owner finds a deleted board's comment")
		}
	})
}
