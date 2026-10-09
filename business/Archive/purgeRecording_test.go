package business

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
)

// One recording, removed for good: its file from the uploads bucket first, then
// its nodes; when storage won't remove the file, the nodes stay.
func TestPurgeRecordingRemovesTheFileFirst(t *testing.T) {
	t.Setenv("USER_UPLOAD_BUCKET_NAME", "uploads-under-test")
	prevR, prevD := removeObject, deleteRecordingNodes
	t.Cleanup(func() { removeObject, deleteRecordingNodes = prevR, prevD })

	var steps []string
	refuse := false
	removeObject = func(_ context.Context, bucket, key string) (int64, error) {
		if refuse {
			return 0, errors.New("storage says no")
		}
		steps = append(steps, "remove "+bucket+"/"+key)
		return 7, nil
	}
	deleteRecordingNodes = func(_ context.Context, r recordingDomain.ArchivedRecording) error {
		steps = append(steps, "delete "+r.Uid)
		return nil
	}

	size, err := PurgeRecording(context.Background(), recordingDomain.ArchivedRecording{Uid: "0x1", EgressId: "EG_a", ObjectKey: "rec/a.mp4"})
	if err != nil || size != 7 {
		t.Fatalf("size=%d err=%v", size, err)
	}
	if want := []string{"remove uploads-under-test/rec/a.mp4", "delete 0x1"}; !slices.Equal(steps, want) {
		t.Errorf("steps %v, want %v", steps, want)
	}

	// A node with no file (what the old soft delete left) just goes.
	steps = nil
	if _, err := PurgeRecording(context.Background(), recordingDomain.ArchivedRecording{Uid: "0x2", EgressId: "EG_a"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"delete 0x2"}; !slices.Equal(steps, want) {
		t.Errorf("steps %v, want %v", steps, want)
	}

	steps, refuse = nil, true
	_, err = PurgeRecording(context.Background(), recordingDomain.ArchivedRecording{Uid: "0x3", EgressId: "EG_b", ObjectKey: "rec/b.mp4"})
	if err == nil || !strings.Contains(err.Error(), "EG_b") {
		t.Errorf("a refused file: err=%v, want one naming the recording", err)
	}
	if len(steps) != 0 {
		t.Errorf("the node went although its file stayed: %v", steps)
	}
}
