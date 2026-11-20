//go:build integration
// +build integration

package integration_test

// Whoever is outside a recording's place is refused before a line of it is
// read (V-1126 FIX: CR-386 G1 and G2, V-1153 folded in). The refusal's
// answer was already the missing one; what it cost wasn't: the export read
// up to 5,000 lines, and the channel's and the chat's transcript pages read
// theirs, before the asker's standing refused them, so a real id took many
// times a made-up one's time. Here the one read each route makes is held to
// binding no recording and no line for an outsider, and a refused correction
// to one audit row (it was two, around a read that decided nothing).

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	recordingController "github.com/akashc777/OneCamp/controllers/Recording"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestNobodyOutsideAPlaceMakesARecordingsLinesBeRead(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "recording-refused-first-integration-secret")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	integration.SetupLiveKit(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	lee := seedCalendarPerson(t, env.PG, dg, "Lee")

	// A recording an hour old in #launch, in Sam and Maya's DM and in their
	// group, each with three lines Maya said; Lee is in none of them.
	n := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	key := func(at string) string { return "EG_" + n + "_" + at }
	began := time.Now().Add(-time.Hour).UTC()
	filed := func(at, edge, place string) []map[string]any {
		lines := make([]map[string]any, 3)
		for i := range lines {
			lines[i] = map[string]any{"uid": "_:" + at + strconv.Itoa(i), "dgraph.type": "Transcript", "transcript_text": "words " + strconv.Itoa(i),
				"transcript_timestamp": began.Add(time.Duration(i+1) * time.Minute).UnixMilli(), "transcript_from": map[string]any{"uid": maya.UserDgraphInfo.Uid}}
		}
		return []map[string]any{{"uid": "_:" + at, "dgraph.type": "Recording", "recording_egress_id": key(at),
			"recording_stared_at": rfc(began), "recording_ended_at": rfc(began.Add(5 * time.Minute)), "recording_status": "ready",
			"recording_obj_key": "recordings/" + key(at) + ".mp4", "recording_duration": 300, "recording_transcript": lines, edge: map[string]any{"uid": place}}}
	}
	people := []map[string]any{{"uid": sam.UserDgraphInfo.Uid}, {"uid": maya.UserDgraphInfo.Uid}}
	launch, group := uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "")
	direct := helpers.GetGroupingId(sam.UserDgraphInfo.Uuid, maya.UserDgraphInfo.Uuid)
	uids := dg.Mutate(t, map[string]any{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": launch, "ch_name": "launch",
		"ch_members": people, "ch_recording": filed("ch", "recording_channel", "_:launch")})
	dg.Mutate(t, map[string]any{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": direct, "dm_participants": people,
		"dm_recording": filed("dm", "recording_dm", "_:dm")})
	dg.Mutate(t, map[string]any{"uid": "_:grp", "dgraph.type": "Dm", "dm_grouping_id": group, "dm_participants": people,
		"dm_recording": filed("grp", "recording_dm", "_:grp")})
	ctx := context.Background()
	in, out := maya.UserDgraphInfo.Uid, lee.UserDgraphInfo.Uid

	// The export's one read: a member's binds the home and its lines, an
	// outsider's neither.
	for _, at := range []string{"ch", "dm", "grp"} {
		if h, err := recordingDomain.ReaderTranscript(ctx, key(at), in, 5000); err != nil || bound(h) != "a home with 3 lines" {
			t.Errorf("export read of the %s recording, Maya (in its place): %s, err %v; want a home with 3 lines", at, bound(h), err)
		}
		if h, err := recordingDomain.ReaderTranscript(ctx, key(at), out, 5000); err != nil || h != nil {
			t.Errorf("export read of the %s recording, Lee (in none): %s, err %v; want no home and no line read", at, bound(h), err)
		}
	}

	// The transcript pages' one read (channel, DM, group): an outsider's
	// binds no recording node, so no line, before the route's 403.
	page := func(at, who string) (*dgraphStruct.DgraphRecording, int, error) {
		var recs []*dgraphStruct.DgraphRecording
		switch at {
		case "ch":
			ch, err := channelDomain.GetChannelRecordingTranscript(ctx, launch, who, key(at), 0, 100)
			if err != nil || ch == nil {
				return nil, 0, err
			}
			recs = ch.Recordings
		default:
			place := direct
			if at == "grp" {
				place = group
			}
			dm, err := chatDomain.GetDMRecordingTranscript(ctx, place, who, key(at), 0, 100)
			if err != nil || dm == nil {
				return nil, 0, err
			}
			recs = dm.Recordings
		}
		if len(recs) == 0 {
			return nil, 0, nil
		}
		return recs[0], len(recs), nil
	}
	for _, at := range []string{"ch", "dm", "grp"} {
		if h, k, err := page(at, in); err != nil || k != 1 || bound(h) != "a home with 3 lines" {
			t.Errorf("transcript page of the %s recording, Maya: %d recordings, %s, err %v; want 1 with 3 lines", at, k, bound(h), err)
		}
		if h, k, err := page(at, out); err != nil || k != 0 {
			t.Errorf("transcript page of the %s recording, Lee: %d recordings (%s), err %v; want no recording node read", at, k, bound(h), err)
		}
	}

	// The correction's one read, in its transaction: an outsider's decide
	// is handed no home and no line.
	for _, c := range []struct {
		name, who, want string
	}{{"Maya (in it)", in, "a home with 1 lines"}, {"Lee (in none)", out, "no home"}} {
		saw := "decide never ran"
		err := recordingDomain.CorrectTranscriptLine(ctx, key("ch"), uids["ch0"], c.who, func(h *dgraphStruct.DgraphRecording) (*dgraphStruct.DgraphTranscript, error) {
			saw = bound(h)
			return nil, nil
		})
		if err != nil || saw != c.want {
			t.Errorf("correction read, %s: decide saw %s, err %v; want %s", c.name, saw, err, c.want)
		}
	}

	// A refused correction writes ONE audit row, _refused, and the route's
	// answer is today's; a made one writes its row once.
	actions := func(line string) string {
		rows, err := env.PG.Query(`SELECT action FROM admin_audit_log WHERE metadata->>'line' = $1 ORDER BY seq`, line)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				t.Fatal(err)
			}
			got = append(got, a)
		}
		return strings.Join(got, ",")
	}
	k := key("ch")
	for _, c := range []struct {
		name, line, was string
		who             *userModels.UserInfo
		code            int
		rows            string
	}{
		{"Lee, in none of its places", uids["ch0"], "words 0", lee, http.StatusNotFound, "transcript_line_corrected_refused"},
		{"Maya, who said it", uids["ch2"], "words 2", maya, http.StatusOK, "transcript_line_corrected"},
	} {
		w := call(t, recordingController.HandleCorrectTranscriptLine, http.MethodPatch, "/calls/recordings/"+k+"/transcript/lines/"+c.line,
			c.who, `{"text":"other words","was":"`+c.was+`"}`, map[string]string{"key": k, "line_uid": c.line})
		if w.Code != c.code {
			t.Errorf("correction, %s: %d %q, want %d", c.name, w.Code, msgOf(w), c.code)
		}
		if got := actions(c.line); got != c.rows {
			t.Errorf("correction, %s: audit rows [%s], want [%s]", c.name, got, c.rows)
		}
	}
}

// bound is what a read bound, without its words: no home, or a home and how
// many lines came with it.
func bound(h *dgraphStruct.DgraphRecording) string {
	if h == nil {
		return "no home"
	}
	return fmt.Sprintf("a home with %d lines", len(h.Transcript))
}
