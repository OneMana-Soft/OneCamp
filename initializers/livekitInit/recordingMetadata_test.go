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
	if err := json.Unmarshal([]byte(recordingMetadata(name, "EG_1")), &got); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if !got.IsRecording || got.StartedBy != name || got.EgressID != "EG_1" {
		t.Fatalf("got %+v", got)
	}
}
