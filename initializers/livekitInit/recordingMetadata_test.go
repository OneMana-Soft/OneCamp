package livekitInit

import (
	"encoding/json"
	"testing"
)

// Whoever started a recording is named in the room's metadata as valid JSON,
// whatever their name holds.
func TestRecordingMetadataIsValidJSON(t *testing.T) {
	var got struct {
		IsRecording bool   `json:"isRecording"`
		StartedBy   string `json:"recordingStartedBy"`
		EgressID    string `json:"egressID"`
	}
	name := `Sam "the" \ Rivera`
	meta, err := mergeRoomMetadata("", recordingKeys(true, name, "EG_1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(meta), &got); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if !got.IsRecording || got.StartedBy != name || got.EgressID != "EG_1" {
		t.Fatalf("got %+v", got)
	}
}

// CALL-BUG-2. The room's metadata carries the transcription switch and its
// notice as well as the recording. StopRecording wrote the whole metadata as
// {"isRecording": false}, which switched the notice off for everyone while
// the call was still being transcribed. Each feature now changes only its own
// keys.
func TestRecordingKeysLeaveTheTranscriptionSwitchAlone(t *testing.T) {
	on, err := mergeRoomMetadata("", map[string]any{"transcription": map[string]any{"on": true, "by": "Maya"}})
	if err != nil {
		t.Fatal(err)
	}
	recording, err := mergeRoomMetadata(on, recordingKeys(true, "Sam", "EG_1"))
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := mergeRoomMetadata(recording, recordingKeys(false, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stopped), &got); err != nil {
		t.Fatal(err)
	}
	if string(got["transcription"]) != `{"by":"Maya","on":true}` {
		t.Errorf("stopping a recording changed the transcription switch: %s", stopped)
	}
	if string(got["isRecording"]) != "false" {
		t.Errorf("stopping a recording must say so: %s", stopped)
	}
	if _, ok := got["recordingStartedBy"]; ok {
		t.Errorf("a stopped recording still names who started it: %s", stopped)
	}
	if _, ok := got["egressID"]; ok {
		t.Errorf("a stopped recording still names its egress: %s", stopped)
	}
}

// Metadata an older server wrote, or none, counts as no keys rather than a
// reason to refuse a change.
func TestMergeRoomMetadataStartsFromAnythingUnreadable(t *testing.T) {
	for _, current := range []string{"", "not json", `["a list"]`, `{"isRecording": false}`} {
		got, err := mergeRoomMetadata(current, map[string]any{"transcription": map[string]any{"on": false}})
		if err != nil {
			t.Fatalf("%q: %v", current, err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal([]byte(got), &keys); err != nil || keys["transcription"] == nil {
			t.Errorf("%q became %q", current, got)
		}
	}
}
