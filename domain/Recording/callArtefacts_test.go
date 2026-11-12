package domain

import (
	"strings"
	"testing"
	"time"
)

// What each call made is read in one query, its session's node first, then
// the recordings its conversation holds from its window, the earliest first,
// each once; a call that made nothing has no entry.
func TestEachCallKeepsItsSessionThenItsRecordingsOnce(t *testing.T) {
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	calls := []CallWindow{
		{Key: "CS_one", Home: "channel-1", From: at, To: at.Add(time.Hour)},
		{Key: "CS_two", Home: "dm a b", From: at, To: at.Add(time.Hour)},
		{Key: "CS_none", Home: "channel-1", From: at, To: at.Add(time.Hour)},
	}
	query, vars := callArtefactsQuery(calls)
	for _, want := range []string{"$k0: string", "$h1: string", "$t2: string", "s0(func: eq(recording_egress_id, $k0))",
		"c1(func: eq(ch_uuid, $h1))", "d1(func: eq(dm_grouping_id, $h1))", RecordedDuring("$f2", "$t2")} {
		if !strings.Contains(query, want) {
			t.Fatalf("the query has no %q:\n%s", want, query)
		}
	}
	if vars["$k1"] != "CS_two" || vars["$h1"] != "dm a b" || vars["$f0"] != "2026-11-06T09:00:00Z" || vars["$t0"] != "2026-11-06T10:00:00Z" {
		t.Fatalf("the query's variables: %v", vars)
	}

	raw := []byte(`{
		"s0": [{"recording_egress_id": "CS_one", "recording_transcript_only": true, "lines": 4, "recording_notes_doc": "doc-1"}],
		"c0": [{"ch_recording": [
			{"recording_egress_id": "EG_late", "recording_stared_at": "2026-11-06T09:40:00Z"},
			{"recording_egress_id": "EG_early", "recording_stared_at": "2026-11-06T09:10:00Z"},
			{"recording_egress_id": "CS_one", "recording_stared_at": "2026-11-06T09:00:00Z"}]}],
		"d0": [],
		"s1": [],
		"d1": [{"dm_recording": [{"recording_egress_id": "EG_dm", "recording_stared_at": "2026-11-06T09:05:00Z"}]}],
		"s2": [], "c2": [{}], "d2": []
	}`)
	got, err := parseCallArtefacts(raw, calls)
	if err != nil {
		t.Fatal(err)
	}
	one := got["CS_one"]
	if len(one) != 3 || one[0].Key != "CS_one" || one[0].Recorded || one[0].Lines != 4 || one[0].NotesDoc != "doc-1" ||
		one[1].Key != "EG_early" || !one[1].Recorded || one[2].Key != "EG_late" {
		t.Fatalf("the first call kept %+v; want its session, then its recordings by start, each once", one)
	}
	if two := got["CS_two"]; len(two) != 1 || two[0].Key != "EG_dm" {
		t.Fatalf("the DM's call kept %+v", two)
	}
	if _, ok := got["CS_none"]; ok || len(got) != 2 {
		t.Fatalf("a call that made nothing: %+v", got)
	}
	if empty, err := CallArtefacts(t.Context(), nil); err != nil || len(empty) != 0 {
		t.Fatalf("no calls: %v, %v", empty, err)
	}
}
