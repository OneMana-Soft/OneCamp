package business

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// Deleting a recording is soft, as every delete is: it stamps the recording's
// deletion with the archive's own write, and its file stays in storage until
// the workspace's purge setting removes it. Restoring clears the stamp the
// same way (the web's Undo). A write that failed is reported.
func TestADeletedRecordingIsKeptUntilRetentionAndComesBack(t *testing.T) {
	prevA, prevB := archiveRecordings, restoreRecordings
	t.Cleanup(func() { archiveRecordings, restoreRecordings = prevA, prevB })
	ctx := context.Background()

	var archived, restored []string
	archiveRecordings = func(_ context.Context, ids []string) error {
		archived = append(archived, ids...)
		return nil
	}
	restoreRecordings = func(_ context.Context, ids []string) error {
		restored = append(restored, ids...)
		return nil
	}

	if err := DeleteRecording(ctx, "EG_1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(archived, []string{"EG_1"}) {
		t.Errorf("a delete archived %v, want EG_1", archived)
	}
	if err := RestoreRecording(ctx, "EG_1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(restored, []string{"EG_1"}) {
		t.Errorf("a restore restored %v, want EG_1", restored)
	}

	archiveRecordings = func(context.Context, []string) error { return errors.New("graph unreachable") }
	restoreRecordings = archiveRecordings
	if err := DeleteRecording(ctx, "EG_1"); err == nil {
		t.Error("a delete whose write failed was reported as done")
	}
	if err := RestoreRecording(ctx, "EG_1"); err == nil {
		t.Error("a restore whose write failed was reported as done")
	}
}
