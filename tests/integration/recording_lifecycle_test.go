//go:build integration
// +build integration

package integration_test

// Archiving, restoring and deleting a recording reach the recording.
//
// Each of those mutations used to name no node, so each made a new node
// holding only the egress id and a time: no recording was ever archived,
// restored or deleted, the purge removed those stray nodes and never a
// recording's file, and a deleted recording stayed listed and playable. The
// reads that open a recording by its egress id didn't ask whether it was
// deleted either.
//
// This runs the real queries and mutations against Dgraph. MinIO is a fake S3
// endpoint that records what it was asked to remove.
//
// Run: go test -tags=integration ./tests/integration/ -run TestRecordingLifecycle -v

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

// fakeS3 answers the two calls a purge makes: HEAD (how big is it) and DELETE.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]int64 // "/bucket/key" -> size
	refuse  map[string]bool  // paths storage refuses to touch
	removed []string
}

func (s *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.Path
	if s.refuse[path] {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
		return
	}
	switch r.Method {
	case http.MethodHead:
		size, ok := s.objects[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(s.objects, path)
		s.removed = append(s.removed, path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func TestRecordingLifecycle(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)

	const bucket = "test-uploads"
	t.Setenv("USER_UPLOAD_BUCKET_NAME", bucket)
	store := &fakeS3{objects: map[string]int64{}, refuse: map[string]bool{}}
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)
	endpoint, _ := url.Parse(srv.URL)
	client, err := minio.New(endpoint.Host, &minio.Options{
		Creds: credentials.NewStaticV4("test", "test-secret", ""), Secure: false, Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	prevMinio := minioInit.MinioClient
	minioInit.MinioClient = client
	t.Cleanup(func() { minioInit.MinioClient = prevMinio })

	channel, grouping := uuid.NewString(), uuid.NewString()[:32]
	started, ended := time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	recording := func(blank, egress, key string, edge map[string]any, transcripts ...string) map[string]any {
		r := map[string]any{"uid": blank, "dgraph.type": "Recording", "recording_egress_id": egress,
			"recording_stared_at": started, "recording_ended_at": ended,
			"recording_obj_key": key, "recording_size": 5000}
		for k, v := range edge {
			r[k] = v
		}
		var ts []map[string]any
		for _, tr := range transcripts {
			ts = append(ts, map[string]any{"uid": tr})
		}
		if len(ts) > 0 {
			r["recording_transcript"] = ts
		}
		return r
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:u", "user_uuid": uuid.NewString(), "user_name": "maya"},
		{"uid": "_:ch", "ch_uuid": channel, "ch_name": "eng", "ch_private": true,
			"ch_members":   []map[string]any{{"uid": "_:u"}},
			"ch_recording": []map[string]any{{"uid": "_:live"}, {"uid": "_:stuck"}}},
		recording("_:live", "EG_live", "recordings/eng/EG_live.mp4", map[string]any{"recording_channel": map[string]any{"uid": "_:ch"}}, "_:t1", "_:t2"),
		// Typed "Recording", as business/LiveKit.SaveTranscript writes a line.
		{"uid": "_:t1", "dgraph.type": "Recording", "transcript_text": "the salary bands", "transcript_timestamp": 1},
		{"uid": "_:t2", "dgraph.type": "Recording", "transcript_text": "for next year", "transcript_timestamp": 2},
		// What the old delete left behind: a node holding only the egress id.
		{"uid": "_:stray", "dgraph.type": "Recording", "recording_egress_id": "EG_live", "recording_deleted_at": ended},
		recording("_:stuck", "EG_stuck", "recordings/eng/EG_stuck.mp4", map[string]any{"recording_channel": map[string]any{"uid": "_:ch"}}),
		{"uid": "_:dm", "dm_grouping_id": grouping, "dm_participants": []map[string]any{{"uid": "_:u"}},
			"dm_recording": []map[string]any{{"uid": "_:call"}}},
		recording("_:call", "EG_call", "recordings/dm/EG_call.mp4", map[string]any{"recording_dm": map[string]any{"uid": "_:dm"}}, "_:t3"),
		{"uid": "_:t3", "dgraph.type": "Recording", "transcript_text": "just us", "transcript_timestamp": 1},
	})
	user := uids["u"]

	nodes := func(egress string) []struct {
		Uid       string     `json:"uid"`
		DeletedAt *time.Time `json:"recording_deleted_at"`
		StartedAt *time.Time `json:"recording_stared_at"`
	} {
		t.Helper()
		res, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, `query q($id: string) {
			q(func: eq(recording_egress_id, $id)) { uid recording_deleted_at recording_stared_at }
		}`, map[string]string{"$id": egress})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Q []struct {
				Uid       string     `json:"uid"`
				DeletedAt *time.Time `json:"recording_deleted_at"`
				StartedAt *time.Time `json:"recording_stared_at"`
			} `json:"q"`
		}
		if err := json.Unmarshal(res.Json, &out); err != nil {
			t.Fatal(err)
		}
		return out.Q
	}
	archived := func(egress string) bool {
		t.Helper()
		for _, n := range nodes(egress) {
			if n.StartedAt != nil {
				return n.DeletedAt != nil && n.DeletedAt.After(time.Unix(0, 0))
			}
		}
		t.Fatalf("no recording %s", egress)
		return false
	}
	opens := func(egress string) bool {
		t.Helper()
		if egress == "EG_call" {
			r, err := recordingDomain.GetDgraphDmRecordingInfoByEgressId(ctx, egress, user)
			dm, terr := chatDomain.GetDMRecordingTranscript(ctx, grouping, user, egress, 0, 50)
			playable := err == nil && r != nil && r.ObjectKey != ""
			readable := terr == nil && dm != nil && len(dm.Recordings) == 1
			if playable != readable {
				t.Fatalf("%s: playable=%v but transcript readable=%v", egress, playable, readable)
			}
			return playable
		}
		r, err := recordingDomain.GetDgraphChannelRecordingInfoByEgressId(ctx, egress, user)
		ch, terr := channelDomain.GetChannelRecordingTranscript(ctx, channel, user, egress, 0, 50)
		playable := err == nil && r != nil && r.ObjectKey != ""
		readable := terr == nil && ch != nil && len(ch.Recordings) == 1
		if playable != readable {
			t.Fatalf("%s: playable=%v but transcript readable=%v", egress, playable, readable)
		}
		return playable
	}

	if !opens("EG_live") || !opens("EG_call") {
		t.Fatal("a live recording can't be opened by its egress id")
	}

	// Archiving reaches the recordings, makes no node, and hides them.
	if err := recordingDomain.BulkArchiveRecordings(ctx, []string{"EG_live", "EG_call", "EG_nothing"}); err != nil {
		t.Fatal(err)
	}
	if !archived("EG_live") || !archived("EG_call") {
		t.Error("archiving didn't reach the recordings")
	}
	if n := len(nodes("EG_live")); n != 2 {
		t.Errorf("EG_live is held by %d nodes after archiving, want the recording and the old stray", n)
	}
	if n := len(nodes("EG_nothing")); n != 0 {
		t.Errorf("archiving an id that names nothing made %d nodes", n)
	}
	if opens("EG_live") || opens("EG_call") {
		t.Error("an archived recording can still be played, or its transcript read, by its egress id")
	}
	// The purge now finds the archived recordings themselves, files and all.
	due, err := recordingDomain.ListRecordingsArchivedBefore(ctx, time.Now().Add(time.Minute), 50)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(due, func(a recordingDomain.ArchivedRecording) bool {
		return a.Uid == uids["call"] && a.ObjectKey == "recordings/dm/EG_call.mp4"
	}) {
		t.Errorf("the purge doesn't find the archived recording's file: %+v", due)
	}

	// Restoring reaches it too.
	if err := recordingDomain.BulkRestoreRecordings(ctx, []string{"EG_live"}); err != nil {
		t.Fatal(err)
	}
	if archived("EG_live") || !opens("EG_live") {
		t.Error("restoring didn't bring the recording back")
	}
	if !archived("EG_call") {
		t.Error("restoring one recording restored another")
	}
	if n := len(nodes("EG_live")); n != 2 {
		t.Errorf("restoring made nodes: EG_live is held by %d", n)
	}

	// Deleting removes the file, then the recording, its transcript, and the
	// stray node the old delete made.
	livePath := "/" + bucket + "/recordings/eng/EG_live.mp4"
	store.objects[livePath] = 5000
	if err := recordingBusiness.DeleteRecording(ctx, "EG_live"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(store.removed, livePath) {
		t.Errorf("the recording's file wasn't removed: storage removed %v", store.removed)
	}
	if n := len(nodes("EG_live")); n != 0 {
		t.Errorf("EG_live is still held by %d nodes after it was deleted", n)
	}
	if opens("EG_live") {
		t.Error("a deleted recording can still be played by its egress id")
	}
	for _, line := range []string{uids["t1"], uids["t2"]} {
		res, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, `query q($t: string) {
			q(func: uid($t)) { transcript_text }
		}`, map[string]string{"$t": line})
		if err != nil {
			t.Fatal(err)
		}
		if string(res.Json) != `{"q":[]}` {
			t.Errorf("the deleted recording's transcript is still there: %s", res.Json)
		}
	}

	// When storage won't remove the file, the recording stays, so the file
	// can still be found and the delete tried again.
	stuckPath := "/" + bucket + "/recordings/eng/EG_stuck.mp4"
	store.objects[stuckPath] = 5000
	store.refuse[stuckPath] = true
	if err := recordingBusiness.DeleteRecording(ctx, "EG_stuck"); err == nil {
		t.Error("a delete whose file storage refused reported success")
	}
	if !opens("EG_stuck") {
		t.Error("the recording went although its file is still in storage")
	}

	// The archive job names hundreds at once: they go in batches, and the one
	// real recording among them, in the second batch, is reached.
	many := make([]string, 450)
	for i := range many {
		many[i] = "EG_none_" + strconv.Itoa(i)
	}
	many[300] = "EG_stuck"
	if err := recordingDomain.BulkArchiveRecordings(ctx, many); err != nil {
		t.Fatal(err)
	}
	if !archived("EG_stuck") {
		t.Error("a recording in the second batch wasn't archived")
	}
	for _, id := range []string{many[0], many[299], many[449]} {
		if n := len(nodes(id)); n != 0 {
			t.Errorf("archiving %s, which names nothing, made %d nodes", id, n)
		}
	}
}
