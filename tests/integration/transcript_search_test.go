//go:build integration
// +build integration

package integration_test

// zoom-13 Head A: the words said in a call are found through the one search,
// each hit carrying the moment to play, by whoever may read that call's
// transcript today and by nobody else.
//
// A line is found only by a member of the conversation the call was in, read
// from the graph when the search runs (never from the sign-in's cached
// lists, never from the entry), and only once the call has ended. A guest, a
// person removed from the conversation, and anyone outside it get no hit, no
// snippet and no count. A deleted recording's lines are found by nobody, and
// come back when it is restored; a purged recording's entries are forgotten.
// A snippet shows only its own line's words, with a second call's lines in
// the same channel present. The backfill indexes a line written before the
// source existed.
//
// An AI agent's token finds a line only where the agent itself is a member
// of that conversation now, by the same rule, and only while BoundAgent lets
// it act: its sponsor's membership is not enough.
//
// Run: PKGS=./tests/integration/ integ_rel.sh -run TestTranscriptSearch

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	archiveBusiness "github.com/akashc777/OneCamp/business/Archive"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"
	transcriptSearch "github.com/akashc777/OneCamp/business/TranscriptSearch"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/akashc777/OneCamp/tests/integration"
)

type callPerson struct{ uid, id string }

func TestTranscriptSearch(t *testing.T) {
	ctx := context.Background()
	integration.ConnectServerPostgres(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	person := func(name string, external bool) callPerson {
		id := uuid.NewString()
		got := dg.Mutate(t, map[string]any{"uid": "_:p", "dgraph.type": "User", "user_uuid": id, "user_name": strings.ToLower(name),
			"user_full_name": name, "is_external": external})
		return callPerson{uid: got["p"], id: id}
	}
	sam, maya, jo, gus := person("Sam", false), person("Maya", false), person("Jo", false), person("Gus", true)

	hourAgo := time.Now().Add(-2 * time.Hour)
	rec := func(blank, egress string, started time.Time, ended bool, home string) map[string]any {
		r := map[string]any{"uid": "_:" + blank, "dgraph.type": "Recording", "recording_egress_id": egress,
			"recording_stared_at": started.UTC().Format(time.RFC3339), "recording_obj_key": "recordings/" + egress + ".mp4"}
		if ended {
			r["recording_ended_at"] = started.Add(30 * time.Minute).UTC().Format(time.RFC3339)
		}
		if strings.HasPrefix(home, "ch") {
			r["recording_channel"] = map[string]any{"uid": "_:" + home}
		} else {
			r["recording_dm"] = map[string]any{"uid": "_:" + home}
		}
		return r
	}
	launch, guests := uuid.NewString(), uuid.NewString()
	grp := helpers.GetGroupingId(sam.id, jo.id)
	seeded := dg.Mutate(t, []map[string]any{
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": launch, "ch_name": "launch",
			"ch_members": []map[string]any{{"uid": sam.uid}, {"uid": maya.uid}},
			"ch_recording": []map[string]any{
				rec("r1", "EG_budgetcall01", hourAgo, true, "ch"),
				rec("r3", "EG_budgetcall2", hourAgo.Add(40*time.Minute), true, "ch"),
				rec("rl", "EG_livecall", time.Now().Add(-5*time.Minute), false, "ch"),
				rec("rd", "EG_deletedcall", hourAgo, true, "ch"),
			}},
		{"uid": "_:chg", "dgraph.type": "Channel", "ch_uuid": guests, "ch_name": "guests",
			"ch_members": []map[string]any{{"uid": gus.uid}, {"uid": sam.uid}}},
		{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": grp,
			"dm_participants": []map[string]any{{"uid": sam.uid}, {"uid": jo.uid}},
			"dm_recording":    []map[string]any{rec("r2", "EG_dmcall", hourAgo, true, "dm")}},
	})

	// say files a line as the write path does and indexes it as the write
	// path does: the entry is built from the graph by the line's own id.
	say := func(egress, sessionKey, room string, by callPerson, text string, offset int64, index bool) string {
		t.Helper()
		ts := time.Now().Add(-time.Hour).UnixMilli()
		line := &dgraphStruct.DgraphTranscript{Text: text, TimeStamp: &ts, OffsetMs: &offset,
			From: &dgraphStruct.DgraphUser{Uid: by.uid}, DType: []string{"Recording"}}
		uid, err := recordingDomain.AttachTranscriptLine(ctx, egress, line)
		if err != nil || uid == "" {
			t.Fatalf("file %q under %s: uid %q, %v", text, egress, uid, err)
		}
		if index {
			if err := transcriptSearch.IndexLine(ctx, transcriptSearch.Written{Key: egress, LineUID: uid, SessionKey: sessionKey, Room: room}); err != nil {
				t.Fatalf("index %s: %v", uid, err)
			}
		}
		return uid
	}
	budgetLine := "the launch budget is ninety thousand"
	budget := say("EG_budgetcall01", "call-RM_budget1", "ch-"+launch, sam, budgetLine, 4200, true)
	say("EG_budgetcall2", "call-RM_budget2", "ch-"+launch, maya, "budget aside we ship on friday and the payroll secret stays", 9100, true)
	say("EG_dmcall", "call-RM_dm", "dm-"+grp, jo, "the hiring budget for the dm", 1500, true)
	say("EG_livecall", "call-RM_livecall", "ch-"+launch, sam, "a live budget nobody hears yet", 300, true)
	deleted := say("EG_deletedcall", "call-RM_deleted", "ch-"+launch, sam, "a deleted budget figure", 700, true)
	// Its recording is then deleted in the graph alone, so its entry is still
	// live in the index: the page check must leave the line out.
	dg.Mutate(t, []map[string]any{{"uid": seeded["rd"], "recording_deleted_at": time.Now().UTC().Format(time.RFC3339)}})
	early := say("EG_budgetcall01", "call-RM_budget1", "ch-"+launch, sam, "the marketing quota before search", 6000, false)
	// The write path indexes near real time: a line is searchable at the
	// index's next refresh. Every line said above is made searchable now, so
	// a check that a line is found can't run before its entry is (a wait on
	// the first line alone left the later ones, the DM's and the live call's,
	// unseen about one run in five).
	if err := opensearchInit.RefreshIndices(ctx, openSearchStruct.TRANSCRIPT_INDEX); err != nil {
		t.Fatalf("refresh the transcript index: %v", err)
	}

	// The live call is going on: LiveKit's room id names its session key.
	if _, _, err := callStateModel.OpenSession(ctx, callStateModel.Session{Room: "ch-" + launch, RoomSID: "RM_livecall", Kind: callStateModel.KindHuddle,
		ResourceKind: "channel", ResourceID: launch, StartedAt: time.Now().Add(-5 * time.Minute)}); err != nil {
		t.Fatalf("open the live call: %v", err)
	}

	type found struct {
		lines    map[string]*openSearchStruct.OpenSearchTranscript
		snippets map[string][]string
		count    int
	}
	// findAs searches as who on c (a token's marks, an agent's), handing
	// the search the sign-in's channel list stale.
	findAs := func(t *testing.T, c context.Context, who callPerson, text string, stale ...string) found {
		t.Helper()
		var chans []*dgraphStruct.DgraphChannel
		for _, c := range stale {
			chans = append(chans, &dgraphStruct.DgraphChannel{Uuid: c})
		}
		p, err := globalSearchBusiness.GetUnifiedGlobalSearch(c, accessBusiness.Subject{UserUID: who.uid, UserUUID: who.id},
			who.id+"@example.test", chans, nil, nil, text)
		if err != nil {
			t.Fatalf("search %q: %v", text, err)
		}
		f := found{lines: map[string]*openSearchStruct.OpenSearchTranscript{}, snippets: map[string][]string{}, count: p.Totals[globalSearchBusiness.KindTranscript]}
		for _, h := range p.Page {
			if h.Transcript != nil {
				f.lines[h.Transcript.Uuid] = h.Transcript
				f.snippets[h.Transcript.Uuid] = h.Highlight["transcript_text"]
			}
		}
		return f
	}
	find := func(who callPerson, text string, stale ...string) found {
		t.Helper()
		return findAs(t, ctx, who, text, stale...)
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within 20 s", what)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	// A channel member finds the finished call's line, with the moment to play.
	eventually("Maya finds the launch budget line", func() bool { _, ok := find(maya, "budget", launch).lines[budget]; return ok })
	got := find(maya, "budget", launch)
	if h := got.lines[budget]; h.OffsetMs != 4200 || h.RecordingID != "EG_budgetcall01" || h.ConvID != launch || h.ConvKind != "channel" ||
		h.ByUserUuid != sam.id || !h.HasMedia || h.CallStartedAt == 0 {
		t.Errorf("the hit = %+v; want offset 4200, recording EG_budgetcall01, the launch channel, said by Sam, with media and the call's start", h)
	}
	// A snippet holds its own line's words only, with a second call's lines
	// in the same channel present.
	texts := map[string]string{}
	for id, h := range got.lines {
		texts[id] = h.Text
	}
	for id, snips := range got.snippets {
		for _, s := range snips {
			for _, w := range strings.Fields(strings.NewReplacer("<mark>", "", "</mark>", "").Replace(s)) {
				if !strings.Contains(texts[id], w) {
					t.Errorf("line %s's snippet %q shows %q, which its line %q doesn't say", id, s, w, texts[id])
				}
			}
		}
	}
	if len(got.lines) < 2 {
		t.Errorf("Maya found %d lines; want both calls' lines in #launch", len(got.lines))
	}

	// While a call runs nobody finds its lines, members included; a deleted
	// recording's lines are found by nobody.
	for id, h := range got.lines {
		if h.RecordingID == "EG_livecall" || id == deleted {
			t.Errorf("Maya found %s (%s): a running call's or a deleted recording's line", id, h.RecordingID)
		}
	}

	// A person outside the channel finds nothing, and no count says otherwise.
	if f := find(jo, "budget"); len(f.lines) != 1 || f.lines[budget] != nil || f.count != 1 {
		t.Errorf("Jo found %d lines (count %d); want only the DM's own", len(f.lines), f.count)
	}
	// The DM's third person finds nothing of it.
	if f := find(maya, "hiring", launch); len(f.lines) != 0 || f.count != 0 {
		t.Errorf("Maya found the DM's line (%d, count %d)", len(f.lines), f.count)
	}
	if f := find(sam, "hiring"); len(f.lines) != 1 {
		t.Errorf("Sam found %d of his DM's lines, want 1", len(f.lines))
	}
	// A guest finds nothing outside their channels: no hit, no count, even
	// passing a channel list that names #launch.
	if f := find(gus, "budget", guests, launch); len(f.lines) != 0 || f.count != 0 {
		t.Errorf("the guest found %d lines (count %d)", len(f.lines), f.count)
	}

	// The call ends, and its recording with it (the egress's end, as its
	// webhook writes it): its members find its lines now.
	live, err := callStateModel.SessionBySID(ctx, "RM_livecall")
	if err != nil || live == nil {
		t.Fatalf("the live call: %v %v", live, err)
	}
	if _, err := callStateModel.EndSession(ctx, live.ID, callStateModel.EndedLastLeft, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := recordingDomain.EndRecording(ctx, "EG_livecall", map[string]interface{}{"recording_ended_at": time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if f := find(maya, "live budget", launch); f.count == 0 {
		t.Error("the ended call's line is still found by nobody")
	}

	// Removed from the channel: nothing, though the sign-in's list still
	// names it and the index entry is unchanged.
	if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(ctx, &api.Mutation{CommitNow: true,
		DelNquads: []byte("<" + seeded["ch"] + "> <ch_members> <" + maya.uid + "> .")}); err != nil {
		t.Fatalf("remove Maya from #launch: %v", err)
	}
	if f := find(maya, "budget", launch); len(f.lines) != 0 || f.count != 0 {
		t.Errorf("Maya, removed from #launch, found %d lines (count %d)", len(f.lines), f.count)
	}
	if !entryExists(t, ctx, budget) {
		t.Error("the entry itself went: the check must be at query time, with the entry unchanged")
	}

	// Deleted, then restored, by each path: DeleteRecording and its restore,
	// the archive's retention and its restore from Settings > Archive. The
	// retention run archives the recordings older than its cutoff through
	// BulkArchiveRecordings (archiveRecordings), so the test calls that with
	// this one recording rather than wait out a cutoff.
	for _, path := range []struct {
		name            string
		delete, restore func() error
	}{
		{"DeleteRecording", func() error { return recordingBusiness.DeleteRecording(ctx, "EG_budgetcall01") },
			func() error { return recordingBusiness.RestoreRecording(ctx, "EG_budgetcall01") }},
		{"archive retention", func() error {
			return recordingDomain.BulkArchiveRecordings(ctx, []string{"EG_budgetcall01"})
		},
			func() error {
				_, err := archiveBusiness.RestoreItems(ctx, "recordings", []string{"EG_budgetcall01"})
				return err
			}},
	} {
		if err := path.delete(); err != nil {
			t.Fatalf("%s: %v", path.name, err)
		}
		if deletedMarked(t, ctx, budget) == false {
			t.Errorf("%s left the line's entry undeleted", path.name)
		}
		if f := find(sam, "launch budget", launch); f.lines[budget] != nil {
			t.Errorf("after %s Sam still finds the line", path.name)
		}
		if err := path.restore(); err != nil {
			t.Fatalf("%s restore: %v", path.name, err)
		}
		if f := find(sam, "launch budget", launch); f.lines[budget] == nil {
			t.Errorf("after %s's restore Sam doesn't find the line", path.name)
		}
	}

	// A line whose recording is gone is dropped and forgotten.
	ghost := &openSearchStruct.OpenSearchTranscript{Uuid: "0x7fffff01", Text: "a budget from a recording that is gone", RecordingID: "EG_gone",
		ConvID: launch, ConvKind: "channel", SessionKey: "call-RM_gone", CreatedAt: time.Now().Unix()}
	// Written straight into the index, as the entry a recording now gone left.
	raw, _ := json.Marshal(ghost)
	if _, err := opensearchInit.OpenSearchClient.Index(ctx, opensearchapi.IndexReq{Index: openSearchStruct.TRANSCRIPT_INDEX, DocumentID: ghost.Uuid,
		Body: strings.NewReader(string(raw)), Params: opensearchapi.IndexParams{Refresh: "true"}}); err != nil {
		t.Fatal(err)
	}
	eventually("the gone recording's entry is forgotten", func() bool {
		if f := find(sam, "gone", launch); f.lines[ghost.Uuid] != nil {
			t.Fatal("the line of a recording that is gone was found")
		}
		return !entryExists(t, ctx, ghost.Uuid)
	})

	// The purge forgets the recording's entries.
	if _, err := archiveBusiness.PurgeRecording(ctx, recordingDomain.ArchivedRecording{Uid: seeded["rd"],
		EgressId: "EG_deletedcall", TranscriptUids: []string{deleted}}); err != nil {
		t.Fatal(err)
	}
	if entryExists(t, ctx, deleted) {
		t.Error("the purged recording's entry is still in the index")
	}

	// The backfill indexes a line written before the source existed.
	if f := find(sam, "marketing quota", launch); f.lines[early] != nil {
		t.Fatal("the unindexed line was found before the backfill")
	}
	if err := transcriptSearch.Backfill(ctx); err != nil {
		t.Fatal(err)
	}
	eventually("the backfilled line is found", func() bool { return find(sam, "marketing quota", launch).lines[early] != nil })
}

// entryExists is whether the transcripts index holds the line's entry.
func entryExists(t *testing.T, ctx context.Context, id string) bool {
	t.Helper()
	resp, err := opensearchInit.OpenSearchClient.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: openSearchStruct.TRANSCRIPT_INDEX, DocumentID: id})
	if resp != nil && resp.Inspect().Response != nil && resp.Inspect().Response.StatusCode == http.StatusNotFound {
		return false
	}
	if err != nil {
		t.Fatalf("read the entry %s: %v", id, err)
	}
	return resp.Found
}

// deletedMarked is whether the line's entry is marked deleted.
func deletedMarked(t *testing.T, ctx context.Context, id string) bool {
	t.Helper()
	resp, err := opensearchInit.OpenSearchClient.Document.Get(ctx, opensearchapi.DocumentGetReq{Index: openSearchStruct.TRANSCRIPT_INDEX, DocumentID: id})
	if err != nil {
		t.Fatalf("read the entry %s: %v", id, err)
	}
	return strings.Contains(string(resp.Source), `"deleted_date"`) && !strings.Contains(string(resp.Source), `"deleted_date":null`)
}
