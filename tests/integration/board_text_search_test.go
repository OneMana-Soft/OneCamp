//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	boardAdapter "github.com/akashc777/OneCamp/adapter/Board"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	"github.com/akashc777/OneCamp/business/Board/scene"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	boardDomain "github.com/akashc777/OneCamp/domain/Board"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	openSearchBoardModels "github.com/akashc777/OneCamp/models/openSearch/Board"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

// W6-B1 (miro-05): what a board says is found in search, with what matched
// marked, and by the AI, by the people who may read the board and no one else;
// made public, by everyone; saved again, by its new words only.
func TestABoardsWordsAreFoundByWhoMayReadIt(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	// Boards keep their state in object storage, as installs do.
	integration.SetupMinio(t, "board-text-test")
	model := httptest.NewServer(embedOnlyModel{})
	defer model.Close()
	t.Setenv("OLLAMA_HOST", model.URL)
	if _, err := env.PG.Exec(`UPDATE ai_settings SET embedding_dimension = $1, rate_limit_per_min = 1000`, privateDocEmbeddingDim); err != nil {
		t.Fatal(err)
	}
	if err := ai.InitAIService(); err != nil || !ai.GetService().IsEnabled() {
		t.Fatalf("the AI did not come up: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.PG.Exec(`UPDATE ai_settings SET enabled = false`)
		_ = ai.ReloadAIService(context.Background())
	})
	if err := opensearchInit.RecreateAIEmbeddingsIndex(ctx, privateDocEmbeddingDim); err != nil {
		t.Fatal(err)
	}

	people := map[string]uuid.UUID{"maya": uuid.New(), "ada": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	uids := dg.Mutate(t, nodes)
	maya := &dgraphStruct.DgraphUser{Uid: uids["maya"], Uuid: people["maya"].String(), UserName: "maya"}

	// marked is what search marks for who in the board's words, "" when it
	// doesn't find the board.
	marked := func(who, text, board string) string {
		t.Helper()
		p, err := globalSearchBusiness.GetUnifiedGlobalSearch(ctx, accessBusiness.Subject{UserUUID: people[who].String()}, people[who].String()+"@example.test", nil, nil, nil, text)
		if err != nil {
			t.Fatalf("searching %q as %s: %v", text, who, err)
		}
		countsOnlyWhatShows(t, who+" searching "+text, p)
		for _, h := range p.Page {
			if h.Board != nil && h.Board.Uuid == board {
				return strings.Join(h.Highlight["board_text"], " ") + " "
			}
		}
		return ""
	}
	inAI := func(who, text, board string) bool {
		t.Helper()
		hits, err := ai.SearchSimilar(ctx, text, people[who].String(), nil, nil, nil, 20)
		if err != nil {
			t.Fatalf("AI search %q as %s: %v", text, who, err)
		}
		for _, h := range hits {
			if h.ContentType == ai.BoardContentType && h.ContentUUID == board {
				return strings.Contains(h.ContentText, text)
			}
		}
		return false
	}
	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("timed out waiting until %s", what)
	}
	save := func(board, words string) {
		t.Helper()
		frame := scene.Frame("f1", "Logistics", 0, 0, 400, 400, 1)
		note := scene.Text("t1", 10, 10, 300, words, scene.TextStyle{}, 1)
		note["frameId"] = "f1"
		state, _, err := scene.State(11, []scene.Element{frame, note}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := boardBusiness.UpdateBoardFromCollab(ctx, &boardAdapter.InputBoardCollabUpdate{BoardUuid: board, State: state,
			Snippet: "2 elements"}); err != nil {
			t.Fatal(err)
		}
	}

	// The 409: a board's text that reaches search before the board's own entry
	// no longer makes that entry's create fail, leaving no title and no access.
	early := uuid.NewString()
	if err := openSearchBoardModels.SetBoardTextsInOpenSearch(ctx, []openSearchBoardModels.BoardText{{BoardUUID: early, Text: "Book the venue"}}); err != nil {
		t.Fatal(err)
	}
	if err := openSearchBoardModels.CreateBoardInOpenSearch(ctx, &openSearchStruct.OpenSearchBoard{Uuid: early, BoardTitle: "Early",
		BoardCreatedByUserUuid: maya.Uuid, BoardPrivate: true}); err != nil {
		t.Fatalf("the board's entry after its text: %v", err)
	}
	resp, err := opensearchInit.OpenSearchClient.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: openSearchStruct.BOARD_INDEX, DocumentID: early})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Inspect().Response.Body)
	var got struct {
		Source map[string]any `json:"_source"`
	}
	_ = json.Unmarshal(raw, &got)
	if got.Source["board_title"] != "Early" || got.Source["board_created_by_user_id"] != maya.Uuid || got.Source["board_text"] != "Book the venue" {
		t.Fatalf("the entry after its text came first: %v", got.Source)
	}

	board, err := boardBusiness.CreateBoard(ctx, maya, &boardAdapter.InputCreateBoard{BoardTitle: "Offsite", BoardPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	save(board.Uuid, "Book the venue")
	eventually("maya finds her private board by its words, marked", func() bool {
		return strings.Contains(marked("maya", "venue", board.Uuid), "<mark>venue</mark>")
	})
	eventually("maya's AI finds what her private board says", func() bool { return inAI("maya", "venue", board.Uuid) })
	if marked("ada", "venue", board.Uuid) != "" || inAI("ada", "venue", board.Uuid) {
		t.Fatal("ada found maya's private board by its words")
	}

	no := false
	if err := boardBusiness.UpdateBoard(ctx, &boardAdapter.InputUpdateBoard{BoardId: board.Uuid, IsPrivate: &no}); err != nil {
		t.Fatal(err)
	}
	eventually("ada finds the board made public, in search and the AI", func() bool {
		return marked("ada", "venue", board.Uuid) != "" && inAI("ada", "venue", board.Uuid)
	})

	save(board.Uuid, "Book the hall")
	eventually("a save replaces the board's words", func() bool {
		return strings.Contains(marked("maya", "hall", board.Uuid), "<mark>hall</mark>") && marked("maya", "venue", board.Uuid) == "" &&
			inAI("maya", "hall", board.Uuid) && !inAI("maya", "venue", board.Uuid)
	})
	// Boards drawn before board text existed: the backfill writes their words,
	// a page at a time, from their states in object storage, by who may read
	// them, and leaves a board whose text is written already as it is.
	retro, err := boardBusiness.CreateBoard(ctx, maya, &boardAdapter.InputCreateBoard{BoardTitle: "Retro", BoardPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	save(retro.Uuid, "Pin the agenda")
	if got, err := boardDomain.GetBoardsForText(ctx, []string{retro.Uuid, "not-a-uuid"}); err != nil || len(got) != 1 || got[0].State != "" ||
		got[0].StateKey == "" || got[0].Title != "Retro" || got[0].CreatedBy == nil || got[0].CreatedBy.Uuid != maya.Uuid {
		t.Fatalf("the retro as the backfill reads it (its state in object storage): %v, %+v", err, got)
	}
	eventually("maya finds the retro by its words", func() bool { return marked("maya", "agenda", retro.Uuid) != "" })
	if err := redisStore.Delete(ctx, registry.BoardTextDigest, []string{retro.Uuid}); err != nil {
		t.Fatal(err)
	}
	if err := openSearchBoardModels.SetBoardTextsInOpenSearch(ctx, []openSearchBoardModels.BoardText{{BoardUUID: retro.Uuid},
		{BoardUUID: board.Uuid, Text: "Kept words"}}); err != nil {
		t.Fatal(err)
	}
	if err := ai.DeleteEmbeddings(ctx, ai.BoardContentType, []string{retro.Uuid}); err != nil {
		t.Fatal(err)
	}
	eventually("the retro's words are gone, as if drawn before", func() bool {
		return marked("maya", "agenda", retro.Uuid) == "" && !inAI("maya", "agenda", retro.Uuid) && marked("maya", "kept", board.Uuid) != ""
	})
	if err := boardBusiness.BackfillBoardText(ctx); err != nil {
		t.Fatal(err)
	}
	eventually("the backfill wrote the retro's words, for maya", func() bool {
		return strings.Contains(marked("maya", "agenda", retro.Uuid), "<mark>agenda</mark>") && inAI("maya", "agenda", retro.Uuid)
	})
	if marked("ada", "agenda", retro.Uuid) != "" || inAI("ada", "agenda", retro.Uuid) {
		t.Fatal("ada found maya's private retro by the words the backfill wrote")
	}
	if marked("maya", "kept", board.Uuid) == "" || marked("maya", "hall", board.Uuid) != "" {
		t.Fatal("the backfill wrote again a board whose text was written already")
	}
}
