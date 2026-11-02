package domain

import "testing"

// An event's "Recordings & notes" say which calls were recorded (head D), so a
// recording is offered to play and a call that was only transcribed isn't.
func TestAnEventsArtefactsSayWhichCallsWereRecorded(t *testing.T) {
	raw := []byte(`{"artefacts":[{"recording_egress_id":"EG_1","lines":3},{"recording_egress_id":"call:s1","recording_transcript_only":true,"lines":5}]}`)
	got, err := parseEventCallArtefacts(raw)
	if err != nil || len(got) != 2 {
		t.Fatalf("parseEventCallArtefacts = %+v, %v; want two calls", got, err)
	}
	if !got[0].Recorded || got[1].Recorded {
		t.Errorf("recorded = %v, %v; want the recording's true and the transcript's false", got[0].Recorded, got[1].Recorded)
	}
	if got[0].Key != "EG_1" || got[0].Lines != 3 || got[1].Key != "call:s1" || got[1].Lines != 5 {
		t.Errorf("calls = %+v; want each with its own key and lines", got)
	}
}
