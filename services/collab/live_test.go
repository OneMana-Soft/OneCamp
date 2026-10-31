package collab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLive is the collaboration service's live port: it records each request
// and answers what the test says.
func fakeLive(t *testing.T, answer func(op string, body map[string]any) (int, any)) (*int32, *[]map[string]any) {
	t.Helper()
	var calls int32
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("X-Internal-Secret") != "s3cret-for-tests" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		body["_path"] = r.URL.Path
		bodies = append(bodies, body)
		status, data := answer(parts[len(parts)-1], body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{"msg": "ok", "data": data})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"msg": data})
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv(LiveURLEnv, srv.URL)
	t.Setenv("INTERNAL_SECRET", "s3cret-for-tests")
	return &calls, &bodies
}

// A write goes to the doc's op on the live port, attributed as the caller says,
// and its answer comes back whole.
func TestAWriteReachesItsOpAndSaysWhatItDid(t *testing.T) {
	at := time.Date(2026, 10, 12, 9, 41, 7, 0, time.UTC)
	_, bodies := fakeLive(t, func(op string, _ map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"blocks": 2, "removed": 1, "block_ids": []string{"b1"}, "lineage": "L", "persisted": true, "persisted_at": at}
	})
	r, err := Live.ReplaceBlocks(context.Background(), "doc-1", []string{"b0"}, "<p>x</p>", Options{By: By{UserUUID: "sam", AgentUUID: "captain"}, AgentEdit: true, RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Blocks != 2 || r.Removed != 1 || r.Lineage != "L" || !r.Persisted || !r.PersistedAt.Equal(at) || r.BlockIDs[0] != "b1" {
		t.Errorf("result %+v", r)
	}
	b := (*bodies)[0]
	if b["_path"] != "/live/docs/doc-1/replaceBlocks" || b["html"] != "<p>x</p>" || b["agent_edit"] != true || b["run_id"] != "run-1" {
		t.Errorf("request %v", b)
	}
	by := b["by"].(map[string]any)
	if by["user_uuid"] != "sam" || by["agent_uuid"] != "captain" {
		t.Errorf("by %v", by)
	}
}

// A quiet write (a copy's body, an import's) says so on the wire, so the save
// that stores it tells nobody whom it mentions; any other write sends no quiet
// at all.
func TestAQuietWriteSaysSoOnTheWire(t *testing.T) {
	_, bodies := fakeLive(t, func(string, map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"blocks": 1, "persisted": true}
	})
	if _, err := Live.Seed(context.Background(), "doc-1", "<p>x</p>", Options{By: By{UserUUID: "sam"}, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := Live.Seed(context.Background(), "doc-2", "<p>x</p>", Options{By: By{UserUUID: "sam"}}); err != nil {
		t.Fatal(err)
	}
	if q, ok := (*bodies)[0]["quiet"]; !ok || q != true {
		t.Errorf("a quiet seed sent quiet %v (present %v)", q, ok)
	}
	if q, ok := (*bodies)[1]["quiet"]; ok {
		t.Errorf("a loud seed sent quiet %v", q)
	}
}

// A refusal is an answer: never retried, and told apart by kind.
func TestARefusalIsNeverRetriedAndSaysWhatItIs(t *testing.T) {
	for _, c := range []struct {
		status int
		msg    string
		want   error
	}{
		{404, "no such block", ErrNoSuchBlock},
		{404, "no such doc", ErrNoSuchDoc},
		{409, "the doc already has content", ErrHasContent},
		{410, "the doc is in the trash", ErrDocInTrash},
		{400, "html is required", ErrRefused},
		{503, "the doc could not be opened just now", ErrUnavailable},
	} {
		calls, _ := fakeLive(t, func(string, map[string]any) (int, any) { return c.status, c.msg })
		_, err := Live.Seed(context.Background(), "doc-1", "<p>x</p>", Options{})
		if !errors.Is(err, c.want) {
			t.Errorf("%d %s: got %v, want %v", c.status, c.msg, err, c.want)
		}
		var op *OpError
		if !errors.As(err, &op) || op.StatusCode() != c.status || !strings.HasSuffix(err.Error(), c.msg) {
			t.Errorf("%d: the refusal reads %v", c.status, err)
		}
		if *calls != 1 {
			t.Errorf("%d: sent %d times; a write is never repeated", c.status, *calls)
		}
	}
}

// With no secret, or nobody listening, a write is unavailable rather than made
// some other way.
func TestNoServiceIsUnavailable(t *testing.T) {
	t.Setenv("INTERNAL_SECRET", "")
	if _, err := Live.Append(context.Background(), "d", "<p>x</p>", Options{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("no secret: %v", err)
	}
	t.Setenv("INTERNAL_SECRET", "s3cret-for-tests")
	t.Setenv(LiveURLEnv, "http://127.0.0.1:1")
	if _, err := Live.Append(context.Background(), "d", "<p>x</p>", Options{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("nobody listening: %v", err)
	}
}

// SnapshotFirst keeps the body exactly as the write found it, through the
// Snapshot Service's hook, and the result names the version.
func TestTheVersionBeforeAWriteIsKept(t *testing.T) {
	fakeLive(t, func(string, map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"blocks": 1, "persisted": true, "before": map[string]any{"html": "<p>before</p>", "hash": "h", "lineage": "L"}}
	})
	var kept []Version
	prev := KeepVersion
	KeepVersion = func(_ context.Context, v Version) (string, error) {
		kept = append(kept, v)
		return "version-1", nil
	}
	t.Cleanup(func() { KeepVersion = prev })
	r, err := Live.ReplaceAll(context.Background(), "doc-1", "<p>after</p>", Options{By: By{UserUUID: "sam", AgentUUID: "captain"}, AgentEdit: true, RunID: "run-9", SnapshotFirst: true, SnapshotName: "Before the agent", SnapshotReason: "before_write"})
	if err != nil {
		t.Fatal(err)
	}
	if r.VersionID != "version-1" || len(kept) != 1 {
		t.Fatalf("kept %v, result %+v", kept, r)
	}
	v := kept[0]
	if v.DocUUID != "doc-1" || v.HTML != "<p>before</p>" || v.Hash != "h" || v.Lineage != "L" || !v.Agent || v.RunID != "run-9" || v.Name != "Before the agent" || v.By.AgentUUID != "captain" {
		t.Errorf("version %+v", v)
	}
	// No SnapshotFirst, nothing kept.
	if _, err := Live.Append(context.Background(), "doc-1", "<p>x</p>", Options{}); err != nil || len(kept) != 1 {
		t.Errorf("a write without SnapshotFirst kept a version (%v)", err)
	}
}

// Read, Reauth and Render reach their ops.
func TestReadReauthAndRender(t *testing.T) {
	_, bodies := fakeLive(t, func(op string, _ map[string]any) (int, any) {
		switch op {
		case "read":
			return http.StatusOK, map[string]any{"html": "<p>live</p>", "hash": "h", "lineage": "L", "open": true}
		case "render":
			return http.StatusOK, map[string]any{"html": "<p>state</p>", "open": false}
		}
		return http.StatusOK, map[string]any{"closed": 1}
	})
	c, err := Read(context.Background(), "doc-1")
	if err != nil || c.HTML != "<p>live</p>" || !c.Open {
		t.Errorf("read %+v %v", c, err)
	}
	if err := Live.Reauth(context.Background(), "doc-1", []string{"sam"}); err != nil {
		t.Error(err)
	}
	html, open, err := Render(context.Background(), "doc-1", "c3RhdGU=")
	if err != nil || html != "<p>state</p>" || open {
		t.Errorf("render %q %v %v", html, open, err)
	}
	if got := (*bodies)[1]["user_uuids"].([]any); len(got) != 1 || got[0] != "sam" {
		t.Errorf("reauth sent %v", got)
	}
}

// A board is asked again at its own address, and the people named go
// everywhere at once on theirs; naming nobody asks nothing.
func TestABoardAndPeopleAreClosedAtTheirOwnAddresses(t *testing.T) {
	calls, bodies := fakeLive(t, func(string, map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"closed": 1}
	})
	ctx := context.Background()
	if err := ReauthBoard(ctx, "board-1", []string{"sam"}); err != nil {
		t.Fatal(err)
	}
	if err := ClosePeople(ctx, []string{"sam", "guest-g1"}); err != nil {
		t.Fatal(err)
	}
	if err := ClosePeople(ctx, nil); err != nil || atomic.LoadInt32(calls) != 2 {
		t.Fatalf("closing nobody asked the service (%d calls), %v", atomic.LoadInt32(calls), err)
	}
	board, people := (*bodies)[0], (*bodies)[1]
	if board["_path"] != "/live/boards/board-1/reauth" || len(board["user_uuids"].([]any)) != 1 {
		t.Errorf("the board's reauth went as %v", board)
	}
	if got := people["user_ids"].([]any); people["_path"] != "/live/people/close" || len(got) != 2 || got[1] != "guest-g1" {
		t.Errorf("closing people went as %v", people)
	}
}

// What edit proposals (A9) build on: the applyProposal op's wire, and a
// refusal whose message comes back intact behind "live <op>: <status> ", with
// its status readable through any StatusCode() int.
func TestTheProposalSeamsStayAsA9ReadsThem(t *testing.T) {
	_, bodies := fakeLive(t, func(string, map[string]any) (int, any) {
		return http.StatusConflict, `{"conflicts":["h2"]}`
	})
	_, err := Live.ApplyProposal(context.Background(), "doc-1", "p-1", []string{"h1", "h2"}, Options{
		By: By{UserUUID: "asker", AgentUUID: "agent-1"}, AgentEdit: true, SnapshotFirst: true, RunID: "run-1",
	})
	if err == nil || err.Error() != `live applyProposal: 409 {"conflicts":["h2"]}` {
		t.Fatalf("the refusal reads %v", err)
	}
	var coded interface{ StatusCode() int }
	if !errors.As(err, &coded) || coded.StatusCode() != http.StatusConflict {
		t.Errorf("no StatusCode() behind %v", err)
	}
	b := (*bodies)[0]
	if b["_path"] != "/live/docs/doc-1/applyProposal" || b["proposal_id"] != "p-1" || b["agent_edit"] != true || b["snapshot_first"] != true || b["run_id"] != "run-1" {
		t.Errorf("request %v", b)
	}
	if hunks, _ := b["hunk_ids"].([]any); len(hunks) != 2 || hunks[1] != "h2" {
		t.Errorf("hunk_ids %v", b["hunk_ids"])
	}
	if by, _ := b["by"].(map[string]any); by["user_uuid"] != "asker" || by["agent_uuid"] != "agent-1" {
		t.Errorf("by %v", b["by"])
	}
}
