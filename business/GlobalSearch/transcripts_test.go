package business

// zoom-13 Head A: the transcript source. Its reach is the conversations the
// asker is in now, read from the graph at query time (never the sign-in's
// cached channel list), less every call still going on; a read that fails
// finds no transcript and leaves every other kind as it was. Each page is
// checked against the graph and fails closed.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/akashc777/OneCamp/models/taskread"
	aiService "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

func transcriptSource(t *testing.T) source {
	t.Helper()
	for _, s := range sources {
		if s.kind == KindTranscript {
			if s.index != openSearchStruct.TRANSCRIPT_INDEX {
				t.Fatalf("the transcript source reads %s", s.index)
			}
			return s
		}
	}
	t.Fatal("no transcript source")
	return source{}
}

func reachJSON(t *testing.T, r Reach) string {
	t.Helper()
	raw, err := json.Marshal(sourceQuery(transcriptSource(t), r, part{always: true}))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// M1, M2, M6, M7: the reach is the graph's conversations, never the sign-in's
// list, and leaves out every running call by its session key and, for a
// line with none, by its room.
func TestATranscriptIsReachedThroughTheConversationsReadNow(t *testing.T) {
	r := Reach{UserUUID: "U", Channels: []string{"stale-channel"},
		Calls: &CallReach{Places: []string{"ch-1", "grp-1"}, RunningKeys: []string{"call-RM_live"}, RunningRooms: []string{"room-live"}}}
	q := reachJSON(t, r)
	for _, want := range []string{`"transcript_conv_id":["ch-1","grp-1"]`, `"transcript_session_key":["call-RM_live"]`, `"transcript_room":["room-live"]`, `"must_not"`} {
		if !strings.Contains(q, want) {
			t.Errorf("the reach %s lacks %s", q, want)
		}
	}
	if strings.Contains(q, "stale-channel") {
		t.Errorf("the reach %s reads the sign-in's cached channel list", q)
	}
	// Someone in no conversation (a guest, a removed member) reaches none.
	none := reachJSON(t, Reach{UserUUID: "G", Channels: []string{"ch-1"}, Calls: &CallReach{}})
	if !strings.Contains(none, `"transcript_conv_id":[]`) || strings.Contains(none, "ch-1") {
		t.Errorf("a person in no conversation reaches %s", none)
	}
}

func TestATranscriptReadThatFailsFindsNoTranscriptAndEverythingElse(t *testing.T) {
	allReady(t)
	swap(t, &taskread.Decide, func(context.Context, string) (map[string]bool, error) { return nil, nil })
	var asked []string
	swap(t, &searchIndex, func(_ context.Context, indices []string, _ []byte) (opensearchModels.SearchAnswer, error) {
		asked = indices
		return opensearchModels.SearchAnswer{}, nil
	})
	for _, c := range []struct {
		name   string
		places func(context.Context, string) ([]string, error)
		open   func(context.Context) (CallReach, error)
		finds  bool
	}{
		{"the places read fails", func(context.Context, string) ([]string, error) { return nil, errors.New("down") },
			func(context.Context) (CallReach, error) { return CallReach{}, nil }, false},
		{"the running calls read fails", func(context.Context, string) ([]string, error) { return []string{"ch-1"}, nil },
			func(context.Context) (CallReach, error) { return CallReach{}, errors.New("down") }, false},
		{"both read", func(context.Context, string) ([]string, error) { return []string{"ch-1"}, nil },
			func(context.Context) (CallReach, error) { return CallReach{}, nil }, true},
	} {
		swap(t, &callPlacesOf, c.places)
		swap(t, &openCallsOf, c.open)
		asked = nil
		if _, err := Search(context.Background(), sample, launch(), Page{}); err != nil {
			t.Fatalf("%s: the search failed: %v", c.name, err)
		}
		if contains(asked, openSearchStruct.TRANSCRIPT_INDEX) != c.finds || !contains(asked, openSearchStruct.POST_INDEX) {
			t.Errorf("%s: the search asked %v; transcripts %v, posts always", c.name, asked, c.finds)
		}
	}
}

// F1: a read of the calls going on that comes back full is a failed one
// (a call past the cap would go unseen), so no transcript is found and every
// other kind still is; one call short of the cap, transcripts are found.
func TestAFullReadOfTheCallsGoingOnFindsNoTranscript(t *testing.T) {
	allReady(t)
	swap(t, &taskread.Decide, func(context.Context, string) (map[string]bool, error) { return nil, nil })
	var asked []string
	swap(t, &searchIndex, func(_ context.Context, indices []string, _ []byte) (opensearchModels.SearchAnswer, error) {
		asked = indices
		return opensearchModels.SearchAnswer{}, nil
	})
	swap(t, &callPlacesOf, func(context.Context, string) ([]string, error) { return []string{"ch-1"}, nil })
	swap(t, &openCallsOf, openCalls)
	for _, c := range []struct {
		open  int
		finds bool
	}{{openCallsMost, false}, {openCallsMost - 1, true}} {
		swap(t, &openSessionsOf, func(_ context.Context, limit int) ([]callStateModel.Session, error) {
			if limit != openCallsMost {
				t.Errorf("the calls going on were read %d at most; want %d", limit, openCallsMost)
			}
			return make([]callStateModel.Session, c.open), nil
		})
		asked = nil
		if _, err := Search(context.Background(), sample, launch(), Page{}); err != nil {
			t.Fatalf("%d calls going on: the search failed: %v", c.open, err)
		}
		if contains(asked, openSearchStruct.TRANSCRIPT_INDEX) != c.finds || !contains(asked, openSearchStruct.POST_INDEX) {
			t.Errorf("%d calls going on: the search asked %v; transcripts %v, posts always", c.open, asked, c.finds)
		}
	}
}

// M8: a snippet is its own line's words: the only field matched and marked
// is the line's text (and the speaker's name matched, never marked).
func TestATranscriptSnippetIsItsOwnLine(t *testing.T) {
	s := transcriptSource(t)
	for _, f := range s.text(sample) {
		if name := strings.SplitN(f, "^", 2)[0]; name != "transcript_text" && name != "transcript_by_user_full_name" {
			t.Errorf("transcripts match %s, a field wider than the line", f)
		}
	}
	marked := highlight["fields"].(map[string]any)
	for f := range marked {
		if strings.HasPrefix(f, "transcript") && f != "transcript_text" {
			t.Errorf("the highlight marks %s", f)
		}
	}
	if _, ok := marked["transcript_text"]; !ok {
		t.Error("the line's text isn't marked")
	}
}

func transcriptHit(line, key, conv string) *openSearchStruct.GlobalSearchOpenSearchResp {
	return &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.TRANSCRIPT_TYPE,
		Transcript: &openSearchStruct.OpenSearchTranscript{Uuid: line, RecordingID: key, ConvID: conv}}
}

// M3: the page check drops a deleted recording's line (and has its entries
// marked again), a line its recording no longer holds or whose recording is
// gone (and forgets it), a line of a conversation the asker left, and a call
// not yet ended; a check that fails drops every transcript and nothing else.
func TestAPageOfTranscriptsIsCheckedAgainstTheGraph(t *testing.T) {
	var forgot []string
	swap(t, &forgetFn, func(gone []*openSearchStruct.GlobalSearchOpenSearchResp) {
		for _, h := range gone {
			_, id := hitKey(h)
			forgot = append(forgot, id)
		}
	})
	var repaired []string
	swap(t, &repairEntry, func(kind, id string) { repaired = append(repaired, kind+":"+id) })
	swap(t, &goRepairs, func(f func()) { f() })
	homes := map[string]recordingDomain.TranscriptHome{
		"EG_ok":      {ConvID: "ch-1", Ended: true, Lines: map[string]bool{"0x1": true, "0x5": true}},
		"EG_deleted": {ConvID: "ch-1", Ended: true, Deleted: true, Lines: map[string]bool{"0x2": true}},
		"EG_running": {ConvID: "ch-1", Lines: map[string]bool{"0x6": true}},
		// Ended, and its entry agrees with the graph, but in a conversation
		// the asker isn't in now.
		"EG_away": {ConvID: "ch-9", Ended: true, Lines: map[string]bool{"0x7": true}},
	}
	var err error
	swap(t, &transcriptHomesOf, func(context.Context, []string, []string) (map[string]recordingDomain.TranscriptHome, error) {
		return homes, err
	})
	reach := Reach{UserUUID: "U", Calls: &CallReach{Places: []string{"ch-1"}}}
	post := &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.POST_TYPE, Post: &openSearchStruct.OpenSearchPost{Uuid: "p"}}
	page := []*openSearchStruct.GlobalSearchOpenSearchResp{
		transcriptHit("0x1", "EG_ok", "ch-1"), transcriptHit("0x2", "EG_deleted", "ch-1"), transcriptHit("0x3", "EG_gone", "ch-1"),
		transcriptHit("0x4", "EG_ok", "ch-1"), transcriptHit("0x5", "EG_ok", "ch-2"), transcriptHit("0x6", "EG_running", "ch-1"),
		transcriptHit("0x7", "EG_away", "ch-9"), post,
	}
	kept := keepReadableTranscriptHits(context.Background(), reach, page)
	if len(kept) != 2 || kept[0].Transcript == nil || kept[0].Transcript.Uuid != "0x1" || kept[1] != post {
		t.Errorf("kept %d hits; want the live line 0x1 and the post", len(kept))
	}
	if strings.Join(forgot, ",") != "0x3,0x4" {
		t.Errorf("forgot %v; want the gone recording's line and the line its recording no longer holds", forgot)
	}
	if strings.Join(repaired, ",") != repairTranscript+":EG_deleted" {
		t.Errorf("repaired %v; want the deleted recording's entries marked again", repaired)
	}
	err = errors.New("graph down")
	if kept := keepReadableTranscriptHits(context.Background(), reach, page); len(kept) != 1 || kept[0] != post {
		t.Errorf("a failed check kept %d hits; want the post alone", len(kept))
	}
}

// M4: a transcript hit is a kind, keyed by its line, kept in its index.
func TestATranscriptHitIsItsOwnKind(t *testing.T) {
	h := transcriptHit("0x9", "EG_1", "ch-1")
	if kindOfHit(h) != KindTranscript || indexKind(openSearchStruct.TRANSCRIPT_INDEX) != KindTranscript {
		t.Error("a transcript hit isn't the transcript kind")
	}
	if typ, id := hitKey(h); typ != openSearchStruct.TRANSCRIPT_TYPE || id != "0x9" || hitIndex[typ] != openSearchStruct.TRANSCRIPT_INDEX {
		t.Errorf("hitKey = %s %s", typ, id)
	}
}

// M2b (lead's R1): while a conversation has a call going on, no line said
// there since that call started is found, whether or not the line names its
// call; a line of an earlier, ended call there still is. At query time and
// again in the page check.
func TestALineSaidSinceACallStartedIsntFoundWhileItRuns(t *testing.T) {
	started := time.Unix(1_700_000_000, 0)
	running := runningOf([]callStateModel.Session{{Room: "ch-1", RoomSID: "RM_live", ResourceID: "ch-1", StartedAt: started},
		{Room: "ch-1", RoomSID: "RM_later", ResourceID: "ch-1", StartedAt: started.Add(time.Hour)}})
	if running.RunningSince["ch-1"] != started.Unix() || !contains(running.RunningRooms, "ch-1") || len(running.RunningKeys) != 2 {
		t.Fatalf("the calls going on read as %+v", running)
	}
	running.Places = []string{"ch-1", "ch-2"}
	q := reachJSON(t, Reach{UserUUID: "U", Calls: &running})
	if !strings.Contains(q, `"transcript_conv_id":"ch-1"`) || !strings.Contains(q, `"created_date":{"gte":1700000000}`) {
		t.Errorf("the reach %s doesn't leave out ch-1's lines since its call started", q)
	}
	swap(t, &transcriptHomesOf, func(context.Context, []string, []string) (map[string]recordingDomain.TranscriptHome, error) {
		return map[string]recordingDomain.TranscriptHome{
			"EG_now":     {ConvID: "ch-1", Ended: true, Lines: map[string]bool{"0x1": true}},
			"EG_earlier": {ConvID: "ch-1", Ended: true, Lines: map[string]bool{"0x2": true}},
		}, nil
	})
	now, earlier := transcriptHit("0x1", "EG_now", "ch-1"), transcriptHit("0x2", "EG_earlier", "ch-1")
	now.Transcript.CreatedAt, earlier.Transcript.CreatedAt = started.Unix()+5, started.Unix()-600
	kept := keepReadableTranscriptHits(context.Background(), Reach{UserUUID: "U", Calls: &running},
		[]*openSearchStruct.GlobalSearchOpenSearchResp{now, earlier})
	if len(kept) != 1 || kept[0] != earlier {
		t.Errorf("kept %d lines; want the earlier call's alone", len(kept))
	}
	// A channel call's room is "ch-<uuid>" and its line's conversation is the
	// channel's uuid, the call's resource: that is what a line is held by.
	channel := runningOf([]callStateModel.Session{{Room: "ch-X", RoomSID: "RM_x", ResourceID: "X", StartedAt: started}})
	if channel.RunningSince["X"] != started.Unix() {
		t.Errorf("a channel call going on reads as %+v; want channel X running since it started", channel)
	}
	channel.Places = []string{"X"}
	swap(t, &transcriptHomesOf, func(context.Context, []string, []string) (map[string]recordingDomain.TranscriptHome, error) {
		return map[string]recordingDomain.TranscriptHome{"EG_x": {ConvID: "X", Ended: true, Lines: map[string]bool{"0x3": true}}}, nil
	})
	said := transcriptHit("0x3", "EG_x", "X")
	said.Transcript.CreatedAt = started.Unix() + 5
	if kept := keepReadableTranscriptHits(context.Background(), Reach{UserUUID: "U", Calls: &channel},
		[]*openSearchStruct.GlobalSearchOpenSearchResp{said}); len(kept) != 0 {
		t.Errorf("a line said in channel X since its call started was kept")
	}
}

// R5: an AI agent's token finds a line only where the agent's own account
// is in the conversation too, and none while BoundAgent refuses it; an agent
// id that isn't one finds none.
func TestAnAgentFindsOnlyWhereItIsAMemberToo(t *testing.T) {
	agent := uuid.New()
	swap(t, &callPlacesOf, func(_ context.Context, who string) ([]string, error) {
		if who == "bot" {
			return []string{"ch-2", "ch-3"}, nil
		}
		return []string{"ch-1", "ch-2"}, nil
	})
	swap(t, &openCallsOf, func(context.Context) (CallReach, error) { return CallReach{}, nil })
	swap(t, &agentAccountOf, func(_ context.Context, id uuid.UUID) (string, error) {
		if id != agent {
			t.Errorf("the account of %s was read", id)
		}
		return "bot", nil
	})
	may := true
	swap(t, &agentMayAct, func(context.Context, uuid.UUID) bool { return may })
	asAgent := context.WithValue(context.Background(), helpers.ApiAgentIDContextKey, agent.String())
	if c, err := callReach(asAgent, Reach{UserUUID: "U"}); err != nil || strings.Join(c.Places, ",") != "ch-2" {
		t.Errorf("the agent reaches %+v (%v); want ch-2, where both are", c, err)
	}
	// An agent run (WithAgentBudget, no token) is bounded the same way.
	asRun := aiService.WithAgentBudget(context.Background(), agent.String(), 0)
	if c, err := callReach(asRun, Reach{UserUUID: "U"}); err != nil || strings.Join(c.Places, ",") != "ch-2" {
		t.Errorf("the agent's run reaches %+v (%v); want ch-2, where both are", c, err)
	}
	if c, err := callReach(context.Background(), Reach{UserUUID: "U"}); err != nil || strings.Join(c.Places, ",") != "ch-1,ch-2" {
		t.Errorf("the sponsor alone reaches %+v (%v)", c, err)
	}
	may = false
	if c, err := callReach(asAgent, Reach{UserUUID: "U"}); err == nil || c != nil {
		t.Errorf("an agent BoundAgent refuses reaches %+v", c)
	}
	if c, err := callReach(asRun, Reach{UserUUID: "U"}); err == nil || c != nil {
		t.Errorf("a run of an agent BoundAgent refuses reaches %+v", c)
	}
	if c, err := callReach(context.Background(), Reach{UserUUID: "U", Who: accessBusiness.Subject{AgentUUID: "not-an-id"}}); err == nil || c != nil {
		t.Errorf("an agent id that isn't one reaches %+v", c)
	}
}

// R2: a hit never carries the fields its reach is checked by.
func TestATranscriptHitCarriesNoReachFields(t *testing.T) {
	for _, f := range []string{"transcript_session_key", "transcript_room"} {
		if !contains(unreturned, f) {
			t.Errorf("hits carry %s", f)
		}
	}
}
