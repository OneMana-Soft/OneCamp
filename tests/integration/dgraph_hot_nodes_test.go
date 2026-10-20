//go:build integration
// +build integration

package integration_test

// Eight people changing one node at once all land, against a real Dgraph.
//
// Dgraph aborts one of two transactions that overlap on a node ("Please
// retry"), and until 19 Oct 2026 these writers sent their upsert once with a
// bare txn.Do (CommitNow) and gave up on an abort: the doc and board sharing
// writers, the project, calendar event and transcript upserts, a doc comment's
// thread, and the Mutate-then-Commit edge deletes. Each now runs through
// dgraphInit.DoCommitNow, which tries an aborted write again in a fresh
// transaction after a jittered wait.
//
// Every test counts the attempts each write took, through a gRPC interceptor
// on the project's graph client, and logs them, so a run shows how many tries
// eight contenders need (release-1011/dgw-HANDOFF.md).
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestHotNode' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	boardAdapter "github.com/akashc777/OneCamp/adapter/Board"
	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	boardDomain "github.com/akashc777/OneCamp/domain/Board"
	calendarDomain "github.com/akashc777/OneCamp/domain/Calendar"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	projectModels "github.com/akashc777/OneCamp/models/dgraph/Project"
	threadComments "github.com/akashc777/OneCamp/models/dgraph/Thread"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// contenders is how many people write to the one node at once.
const contenders = 8

// writeTries counts, per contender, the graph requests that carried a
// mutation: one per try.
type writeTries struct {
	mu sync.Mutex
	by map[int]int
}

type contenderKey struct{}

// countWriteTries points the project's graph client, until t ends, at a
// connection to env's Dgraph that counts each contender's write requests.
func countWriteTries(t *testing.T, env *integration.DgraphEnv) *writeTries {
	t.Helper()
	tries := &writeTries{by: map[int]int{}}
	count := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if r, ok := req.(*api.Request); ok && len(r.GetMutations()) > 0 {
			if i, ok := ctx.Value(contenderKey{}).(int); ok {
				tries.mu.Lock()
				tries.by[i]++
				tries.mu.Unlock()
			}
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	conn, err := grpc.NewClient(env.Endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(count))
	if err != nil {
		t.Fatalf("a counting graph client: %v", err)
	}
	live := dgraphInit.DgraphClient
	dgraphInit.DgraphClient = dgo.NewDgraphClient(api.NewDgraphClient(conn))
	t.Cleanup(func() {
		dgraphInit.DgraphClient = live
		_ = conn.Close()
	})
	return tries
}

// contend runs write for each of the contenders at the same moment and fails
// t for each one that returned an error. It logs how many tries each took.
func contend(t *testing.T, tries *writeTries, write func(ctx context.Context, i int) error) {
	t.Helper()
	start := make(chan struct{})
	errs := make([]error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = write(context.WithValue(context.Background(), contenderKey{}, i), i)
		}(i)
	}
	began := time.Now()
	close(start)
	wg.Wait()
	took := time.Since(began)

	landed := 0
	for i, err := range errs {
		if err != nil {
			t.Errorf("write %d of %d failed: %v", i+1, contenders, err)
			continue
		}
		landed++
	}
	tries.mu.Lock()
	per := make([]int, 0, contenders)
	total, most := 0, 0
	for i := 0; i < contenders; i++ {
		n := tries.by[i]
		per = append(per, n)
		total += n
		if n > most {
			most = n
		}
	}
	tries.mu.Unlock()
	sorted := append([]int(nil), per...)
	sort.Ints(sorted)
	t.Logf("TRIES %s: %d of %d landed in %v; tries per write %v (sorted %v), most %d, total %d",
		t.Name(), landed, contenders, took.Round(time.Millisecond), per, sorted, most, total)
}

// seedUsers makes n people and answers their uids and uuids.
func seedUsers(t *testing.T, dg *integration.DgraphEnv, n int) (uids, uuids []string) {
	t.Helper()
	nodes := make([]map[string]any, n)
	for i := range nodes {
		id := uuid.NewString()
		uuids = append(uuids, id)
		nodes[i] = map[string]any{"uid": fmt.Sprintf("_:u%d", i), "dgraph.type": "User",
			"user_uuid": id, "user_name": fmt.Sprintf("Person %d", i), "user_deleted_at": "0001-01-01T00:00:00Z"}
	}
	got := dg.Mutate(t, nodes)
	for i := 0; i < n; i++ {
		uids = append(uids, got[fmt.Sprintf("u%d", i)])
	}
	return uids, uuids
}

// countOf answers count(pred) on the node with uid.
func countOf(t *testing.T, dg *integration.DgraphEnv, uid, pred string) int {
	t.Helper()
	var got struct {
		N []map[string]int `json:"n"`
	}
	if err := json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ n(func: uid(%s)) { count(%s) } }`, uid, pred)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.N) != 1 {
		return 0
	}
	return got.N[0]["count("+pred+")"]
}

// A doc changed by eight people at once.
func TestHotNodeDocSharedWithEightAtOnce(t *testing.T) {
	dg := integration.SetupDgraph(t)
	uids, people := seedUsers(t, dg, contenders+1)
	doc := uuid.NewString()
	docUID := dg.Mutate(t, map[string]any{"uid": "_:d", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Launch plan"})["d"]

	// Eight people sharing it with eight others: different edges of one
	// list, which Dgraph doesn't count as a conflict. Kept as a check.
	t.Run("with eight people", func(t *testing.T) {
		tries := countWriteTries(t, dg)
		contend(t, tries, func(ctx context.Context, i int) error {
			return docDomain.UpdateDocPermissions(ctx, &docAdapter.InputUpdateDocPermissions{DocId: doc, AddEditors: []string{people[i]}})
		})
		if n := countOf(t, dg, docUID, "doc_editing_users"); n != contenders {
			t.Errorf("the doc has %d editors, want %d", n, contenders)
		}
	})
	// Eight owners (or one person's eight tabs) sharing it with the same
	// colleague: one edge, so the writes conflict. Each is told it worked.
	t.Run("with one person", func(t *testing.T) {
		tries := countWriteTries(t, dg)
		contend(t, tries, func(ctx context.Context, i int) error {
			return docDomain.UpdateDocPermissions(ctx, &docAdapter.InputUpdateDocPermissions{DocId: doc, AddViewers: []string{people[contenders]}})
		})
		if n := countOf(t, dg, docUID, "doc_reading_users"); n != 1 {
			t.Errorf("the doc has %d readers, want 1", n)
		}
	})
	// Eight pages made under it at once, by the same person: each page
	// points at the doc and its author, through reversed edges.
	t.Run("eight pages under it", func(t *testing.T) {
		tries := countWriteTries(t, dg)
		contend(t, tries, func(ctx context.Context, i int) error {
			now, zero := time.Now(), time.Time{}
			_, err := docDomain.CreateOrUpdateDgraphDoc(ctx, &dgraphStruct.DgraphDoc{
				DType: []string{"Doc"}, Uuid: uuid.NewString(), Title: fmt.Sprintf("Page %d", i),
				CreatedBy: &dgraphStruct.DgraphUser{Uid: uids[0]}, EditingUser: []*dgraphStruct.DgraphUser{{Uid: uids[0]}},
				Parent: &dgraphStruct.DgraphDoc{Uid: docUID}, CreatedAt: &now, UpdatedAt: &now, DeletedAt: &zero,
			})
			return err
		})
		if n := countOf(t, dg, docUID, "~doc_parent"); n != contenders {
			t.Errorf("the doc has %d pages, want %d", n, contenders)
		}
	})
}

// A board changed by eight people at once.
func TestHotNodeBoardSharedWithEightAtOnce(t *testing.T) {
	dg := integration.SetupDgraph(t)
	_, people := seedUsers(t, dg, contenders+1)
	board := uuid.NewString()
	boardUID := dg.Mutate(t, map[string]any{"uid": "_:b", "dgraph.type": "Board", "board_uuid": board, "board_title": "Roadmap"})["b"]

	t.Run("shared with eight people", func(t *testing.T) {
		tries := countWriteTries(t, dg)
		contend(t, tries, func(ctx context.Context, i int) error {
			return boardDomain.UpdateBoardPermissions(ctx, &boardAdapter.InputUpdateBoardPermissions{BoardId: board, AddEditors: []string{people[i]}})
		})
		if n := countOf(t, dg, boardUID, "board_editing_users"); n != contenders {
			t.Errorf("the board has %d editors, want %d", n, contenders)
		}
	})
	t.Run("shared with one person", func(t *testing.T) {
		tries := countWriteTries(t, dg)
		contend(t, tries, func(ctx context.Context, i int) error {
			return boardDomain.UpdateBoardPermissions(ctx, &boardAdapter.InputUpdateBoardPermissions{BoardId: board, AddViewers: []string{people[contenders]}})
		})
		if n := countOf(t, dg, boardUID, "board_reading_users"); n != 1 {
			t.Errorf("the board has %d readers, want 1", n)
		}
	})
	// The collaboration service saving the canvas for eight editors at once:
	// every save is told it worked, and the board points at one of them.
	t.Run("canvas saved", func(t *testing.T) {
		tries := countWriteTries(t, dg)
		keys := make([]string, contenders)
		for i := range keys {
			keys[i] = "boards/" + uuid.NewString()
		}
		contend(t, tries, func(ctx context.Context, i int) error {
			return boardDomain.SetBoardStateRef(ctx, board, keys[i], fmt.Sprintf("save %d", i))
		})
		var got struct {
			B []struct {
				Key string `json:"board_state_key"`
			} `json:"b"`
		}
		if err := json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ b(func: uid(%s)) { board_state_key } }`, boardUID)), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.B) != 1 || !contains(keys, got.B[0].Key) {
			t.Errorf("the board points at %+v, want one of the eight saves", got.B)
		}
	})
}

// Eight people added to a project at once are all its members, and eight
// taken off at once are all gone (DeleteProjectEdge, a Mutate then Commit).
func TestHotNodeProjectMembersAtOnce(t *testing.T) {
	dg := integration.SetupDgraph(t)
	uids, _ := seedUsers(t, dg, contenders)
	project := uuid.NewString()
	projectUID := dg.Mutate(t, map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project, "project_name": "Launch"})["p"]
	query := fmt.Sprintf(`query { project as var(func: eq(project_uuid, %q)) }`, project)

	t.Run("added", func(t *testing.T) {
		tries := countWriteTries(t, dg)
		contend(t, tries, func(ctx context.Context, i int) error {
			now := time.Now()
			_, err := projectModels.CreateOrUpdateDgraphProject(ctx, &dgraphStruct.DgraphProject{
				Uid: "uid(project)", Uuid: project, DType: []string{"Project"},
				Members: []*dgraphStruct.DgraphUser{{Uid: uids[i]}}, UpdatedAt: &now,
			}, query)
			return err
		})
		if n := countOf(t, dg, projectUID, "project_members"); n != contenders {
			t.Errorf("the project has %d members, want %d", n, contenders)
		}
	})
	t.Run("removed", func(t *testing.T) {
		tries := countWriteTries(t, dg)
		contend(t, tries, func(ctx context.Context, i int) error {
			return projectModels.DeleteProjectEdge(ctx, fmt.Sprintf(`{"uid": %q, "project_members": [{"uid": %q}]}`, projectUID, uids[i]))
		})
		if n := countOf(t, dg, projectUID, "project_members"); n != 0 {
			t.Errorf("the project still has %d members, want none", n)
		}
	})
}

// A doc comment put in a thread by eight people at once: every one of them
// is told it worked, and the comment ends in one of their threads.
func TestHotNodeThreadCommentTaggedEightTimesAtOnce(t *testing.T) {
	dg := integration.SetupDgraph(t)
	comment := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:d", "dgraph.type": "Doc", "doc_uuid": uuid.NewString(), "doc_name": "Spec"},
		{"uid": "_:c", "dgraph.type": "Comment", "comment_uuid": comment, "comment_text": "<p>Is this final?</p>", "comment_doc": map[string]any{"uid": "_:d"}},
	})
	threads := make([]string, contenders)
	for i := range threads {
		threads[i] = uuid.NewString()
	}
	tries := countWriteTries(t, dg)

	contend(t, tries, func(ctx context.Context, i int) error {
		return threadComments.SetCommentThread(ctx, comment, threads[i])
	})
	var got struct {
		C []struct {
			Thread string `json:"comment_thread_id"`
		} `json:"c"`
	}
	if err := json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ c(func: uid(%s)) { comment_thread_id } }`, uids["c"])), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.C) != 1 || !contains(threads, got.C[0].Thread) {
		t.Errorf("the comment's thread is %+v, want one of the eight", got.C)
	}
}

// Eight guests added to one calendar event at once are all on it.
func TestHotNodeEventGuestsAddedAtOnce(t *testing.T) {
	dg := integration.SetupDgraph(t)
	uids, _ := seedUsers(t, dg, contenders)
	event := uuid.NewString()
	eventUID := dg.Mutate(t, map[string]any{"uid": "_:e", "dgraph.type": "Event", "event_uuid": event, "event_title": "Launch review"})["e"]
	tries := countWriteTries(t, dg)

	contend(t, tries, func(ctx context.Context, i int) error {
		now := time.Now()
		_, err := calendarDomain.CreateOrUpdateDgraphEvent(ctx, &dgraphStruct.DgraphEvent{
			Uid: "uid(event)", Uuid: event,
			Participants: []*dgraphStruct.DgraphUser{{Uid: uids[i]}}, UpdatedAt: &now,
		})
		return err
	})
	if n := countOf(t, dg, eventUID, "event_participants"); n != contenders {
		t.Errorf("the event has %d guests, want %d", n, contenders)
	}
}

// Eight transcript lines said at once in one call all reach its recording.
func TestHotNodeTranscriptLinesAtOnce(t *testing.T) {
	dg := integration.SetupDgraph(t)
	uids, _ := seedUsers(t, dg, contenders)
	key := uuid.NewString()
	recUID := dg.Mutate(t, map[string]any{"uid": "_:r", "dgraph.type": "Recording", "recording_egress_id": key, "recording_stared_at": time.Now()})["r"]
	tries := countWriteTries(t, dg)

	contend(t, tries, func(ctx context.Context, i int) error {
		at := time.Now().UnixMilli()
		added, err := recordingDomain.AttachTranscriptLine(ctx, key, &dgraphStruct.DgraphTranscript{
			DType: []string{"Transcript"}, From: &dgraphStruct.DgraphUser{Uid: uids[i]},
			Text: fmt.Sprintf("line %d", i), TimeStamp: &at,
		})
		if err == nil && !added {
			err = fmt.Errorf("the line was not added")
		}
		return err
	})
	if n := countOf(t, dg, recUID, "recording_transcript"); n != contenders {
		t.Errorf("the recording has %d lines, want %d", n, contenders)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
