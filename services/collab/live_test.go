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
	if _, err := Live.Append(context.Background(), "d", "<p>x</p>", Options{}); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no secret: %v, want ErrNotConfigured, an ErrUnavailable", err)
	}
	t.Setenv("INTERNAL_SECRET", "s3cret-for-tests")
	t.Setenv(LiveURLEnv, "http://127.0.0.1:1")
	if _, err := Live.Append(context.Background(), "d", "<p>x</p>", Options{}); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotConfigured) {
		t.Errorf("nobody listening: %v, want ErrUnavailable and not ErrNotConfigured (closeLater tries it again)", err)
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

// What a board's comment op (the collaboration service's live/ops/comment.js)
// reads and answers: each change goes to the board's own address with every
// field the op takes, a whole-pin delete names no message, the pin comes back
// with its thread or as gone, and the op's refusals keep their status.
func TestABoardCommentGoesAsItsOpReadsIt(t *testing.T) {
	answers := []struct {
		status int
		data   any
	}{
		{http.StatusOK, map[string]any{"removed": false, "comment": map[string]any{"id": "c-1", "x": 10.5, "y": -4, "authorId": "u-cam", "authorName": "Cam",
			"messages": []map[string]any{{"id": "m-1", "authorId": "u-cam", "authorName": "Cam", "body": "Ship the beta first", "createdAt": 1762950000000}}}}},
		{http.StatusConflict, "the thread is full: at most 200 messages on one comment"},
		{http.StatusOK, map[string]any{"comment": nil, "removed": true}},
		{http.StatusNotFound, "no such comment"},
	}
	n := 0
	_, bodies := fakeLive(t, func(string, map[string]any) (int, any) {
		a := answers[n]
		n++
		return a.status, a.data
	})
	ctx := context.Background()
	x, y := 10.5, -4.0

	added, err := CommentBoard(ctx, "board-1", CommentChange{Action: "add", X: &x, Y: &y, Text: "Ship the beta first"}, "u-cam", "Cam")
	if err != nil || added.Removed || added.Comment == nil || added.Comment.ID != "c-1" || len(added.Comment.Messages) != 1 {
		t.Fatalf("the added pin came back as %+v, %v", added, err)
	}
	if m := added.Comment.Messages[0]; m.ID != "m-1" || m.AuthorID != "u-cam" || m.AuthorName != "Cam" || m.Body != "Ship the beta first" || m.CreatedAt != 1762950000000 {
		t.Errorf("the pin's message decoded as %+v", m)
	}
	_, err = CommentBoard(ctx, "board-1", CommentChange{Action: "reply", CommentID: "c-1", Text: "and the docs"}, "u-cam", "Cam")
	var op *OpError
	if !errors.As(err, &op) || op.StatusCode() != http.StatusConflict || !strings.HasSuffix(err.Error(), "the thread is full: at most 200 messages on one comment") {
		t.Errorf("a full thread's refusal reads %v", err)
	}
	gone, err := CommentBoard(ctx, "board-1", CommentChange{Action: "delete", CommentID: "c-1", MessageID: "m-1"}, "u-cam", "Cam")
	if err != nil || !gone.Removed || gone.Comment != nil {
		t.Errorf("a pin's last message deleted came back as %+v, %v; want removed", gone, err)
	}
	if _, err = CommentBoard(ctx, "board-1", CommentChange{Action: "delete", CommentID: "c-2"}, "u-cam", "Cam"); !errors.Is(err, ErrNoSuchBlock) {
		t.Errorf("a pin that's gone reads %v, want ErrNoSuchBlock", err)
	}

	if len(*bodies) != 4 {
		t.Fatalf("sent %d requests, want 4", len(*bodies))
	}
	for i, want := range []map[string]any{
		{"action": "add", "x": 10.5, "y": -4.0, "text": "Ship the beta first"},
		{"action": "reply", "comment_id": "c-1", "text": "and the docs"},
		{"action": "delete", "comment_id": "c-1", "message_id": "m-1"},
		{"action": "delete", "comment_id": "c-2"},
	} {
		b := (*bodies)[i]
		want["_path"], want["name"] = "/live/boards/board-1/comment", "Cam"
		for k, v := range want {
			if b[k] != v {
				t.Errorf("%s: %s is %v, want %v", want["action"], k, b[k], v)
			}
		}
		if by, _ := b["by"].(map[string]any); len(by) != 1 || by["user_uuid"] != "u-cam" {
			t.Errorf("%s: by %v, want the caller alone", want["action"], b["by"])
		}
		if len(b) != len(want)+1 {
			t.Errorf("%s: sent %v, want only %v and by", want["action"], b, want)
		}
	}
}

// A close that fails is tried again until it lands (miro-33 F3), through the
// real ClosePeople (CR-B92 F1): a turned-off link's open socket learns of it
// from this push, so one failed push no longer leaves it writing. The service
// answering 503, or dropping the connection unanswered, is tried again; a
// close that never lands is tried 1 + closeRetries.Tries times, then given
// up; a server with no live port configured (no INTERNAL_SECRET) once.
func TestCloseLaterTriesAFailedCloseAgainUntilItLands(t *testing.T) {
	old := closeRetries
	t.Cleanup(func() { closeRetries = old })
	closeRetries.First, closeRetries.Cap = time.Millisecond, time.Millisecond
	t.Setenv("INTERNAL_SECRET", "s3cret-for-tests")
	// serve is the live port, failing its n-th close when fails(n) says so:
	// with a 503, or with drop by closing the connection unanswered.
	serve := func(fails func(n int32) bool, drop bool) {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case !fails(n.Add(1)):
				_, _ = io.WriteString(w, `{"msg":"ok","data":{"closed":1}}`)
			case drop:
				if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = c.Close()
				}
			default:
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"msg":"busy"}`)
			}
		}))
		t.Cleanup(srv.Close)
		t.Setenv(LiveURLEnv, srv.URL)
	}
	run := func(want int32) int32 {
		var calls atomic.Int32
		done := make(chan struct{}, 8)
		closeLater("test.close", func(ctx context.Context) error {
			calls.Add(1)
			done <- struct{}{}
			return ClosePeople(ctx, []string{"guest-g1"})
		})
		for i := int32(0); i < want; i++ {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("tried %d times, want %d", calls.Load(), want)
			}
		}
		time.Sleep(50 * time.Millisecond)
		return calls.Load()
	}
	all := int32(1 + closeRetries.Tries)
	serve(func(n int32) bool { return n == 1 }, false)
	if n := run(2); n != 2 {
		t.Errorf("a close the service answered 503 once was tried %d times, want 2 (the retry lands it)", n)
	}
	serve(func(n int32) bool { return n == 1 }, true)
	if n := run(2); n != 2 {
		t.Errorf("a close whose connection the service dropped once was tried %d times, want 2 (the retry lands it)", n)
	}
	serve(func(int32) bool { return true }, false)
	if n := run(all); n != all {
		t.Errorf("a close that never lands was tried %d times, want %d and no more", n, all)
	}
	t.Setenv("INTERNAL_SECRET", "")
	if n := run(1); n != 1 {
		t.Errorf("a server with no live port configured was tried %d times, want once (nothing to close)", n)
	}
}

// A close the service can't be reached for at all (CR-B92's probe P1, kept
// as it ran): the real ClosePeople, connection refused, is tried again until
// the retries run out, as closeTimeout says. MAIN 19:3x: a revoked guest must
// never keep writing because the service was down when its link was turned
// off; a closeLater that gives up on a transport failure dies here.
func TestAnUnreachableCloseIsTriedAgain(t *testing.T) {
	old := closeRetries
	t.Cleanup(func() { closeRetries = old })
	closeRetries.First, closeRetries.Cap = time.Millisecond, time.Millisecond
	t.Setenv("INTERNAL_SECRET", "s3cret-for-tests")
	t.Setenv(LiveURLEnv, "http://127.0.0.1:1")
	var calls atomic.Int32
	var last atomic.Value
	closeLater("probe.close", func(ctx context.Context) error {
		calls.Add(1)
		err := ClosePeople(ctx, []string{"guest-probe"})
		if err != nil {
			last.Store(err.Error())
		}
		return err
	})
	want := int32(1 + closeRetries.Tries)
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end) && calls.Load() < want; {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != want {
		t.Errorf("a close the service can't be reached for was tried %d times, want %d (its error: %v)", n, want, last.Load())
	}
}
