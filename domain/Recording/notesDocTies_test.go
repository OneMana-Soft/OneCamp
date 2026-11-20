package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A call's notes tie is shaped from Dgraph's answer: a public channel's notes
// are public (a handover is not narrowed), a private channel's people count
// only while they are still in it, a DM's are its participants, an event's
// people come from the calendar, a node with no home is no tie, and of two
// calls naming one doc the narrower tie holds, in either order.
func TestACallsNotesTieIsShapedFromItsHome(t *testing.T) {
	answer := `{
		"ties": [
			{"doc": "pub", "recording_channel": {"uid": "0xa", "private": false, "to_in": 0,
				"maker": {"user_uuid": "maker-a", "in": [{"uid": "0xa"}]}}},
			{"doc": "priv", "recording_channel": {"uid": "0xb", "private": true, "to_in": 1,
				"maker": {"user_uuid": "maker-b", "in": [{"uid": "0xb"}]},
				"mods": [{"user_uuid": "left-b", "in": [{"uid": "0xa"}]}, {"user_uuid": "mod-b", "in": [{"uid": "0xa"}, {"uid": "0xb"}]}]}},
			{"doc": "dm", "recording_dm": {"to_in": 0, "people": [{"user_uuid": "p1"}, {"user_uuid": "p2"}]}},
			{"doc": "ev", "event": "event-1"},
			{"doc": "homeless"},
			{"doc": "two", "recording_channel": {"uid": "0xb", "private": true, "to_in": 0}},
			{"doc": "two", "recording_channel": {"uid": "0xa", "private": false, "to_in": 1}},
			{"doc": "two-public-first", "recording_channel": {"uid": "0xa", "private": false, "to_in": 1}},
			{"doc": "two-public-first", "recording_channel": {"uid": "0xb", "private": true, "to_in": 0}}
		],
		"titles": [{"doc_uuid": "priv", "doc_title": "Budget call notes"}]
	}`
	var r notesDocTiesRead
	if err := json.Unmarshal([]byte(answer), &r); err != nil {
		t.Fatal(err)
	}
	want := map[string]NotesDocTie{
		"pub":  {Doc: "pub", Public: true, Order: []string{"maker-a"}},
		"priv": {Doc: "priv", Title: "Budget call notes", ToIn: true, Order: []string{"maker-b", "mod-b"}},
		"dm":   {Doc: "dm", Order: []string{"p1", "p2"}},
		"ev":   {Doc: "ev", Event: "event-1"},
		"two":  {Doc: "two"},
		// CR-430 F1: Dgraph answers in uid order, not by privacy, so the
		// narrower tie holds whichever is read first.
		"two-public-first": {Doc: "two-public-first"},
	}
	if got := r.ties(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ties\n got %+v\nwant %+v", got, want)
	}
}
