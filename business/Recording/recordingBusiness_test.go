package business

import (
	"context"
	"errors"
	"slices"
	"testing"

	domain "github.com/akashc777/OneCamp/domain/Recording"
)

// Deleting a recording purges every node holding its egress id (the recording,
// and any stray node the old soft delete made), and reports what stopped it.
func TestDeleteRecordingPurgesEveryNodeOfIt(t *testing.T) {
	prevR, prevP := recordingsToPurge, purgeRecording
	t.Cleanup(func() { recordingsToPurge, purgeRecording = prevR, prevP })
	ctx := context.Background()

	recordingsToPurge = func(_ context.Context, egressId string) ([]domain.ArchivedRecording, error) {
		return []domain.ArchivedRecording{
			{Uid: "0x1", EgressId: egressId, ObjectKey: "rec/" + egressId + ".mp4"},
			{Uid: "0x2", EgressId: egressId},
		}, nil
	}
	var purged []string
	purgeRecording = func(_ context.Context, r domain.ArchivedRecording) (int64, error) {
		purged = append(purged, r.Uid)
		return 0, nil
	}
	if err := DeleteRecording(ctx, "EG_1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(purged, []string{"0x1", "0x2"}) {
		t.Errorf("purged %v", purged)
	}

	purgeRecording = func(context.Context, domain.ArchivedRecording) (int64, error) {
		return 0, errors.New("storage says no")
	}
	if err := DeleteRecording(ctx, "EG_1"); err == nil {
		t.Error("a purge that failed was reported as a delete")
	}

	recordingsToPurge = func(context.Context, string) ([]domain.ArchivedRecording, error) {
		return nil, errors.New("graph unreachable")
	}
	purged = nil
	purgeRecording = func(_ context.Context, r domain.ArchivedRecording) (int64, error) {
		purged = append(purged, r.Uid)
		return 0, nil
	}
	if err := DeleteRecording(ctx, "EG_1"); err == nil || len(purged) != 0 {
		t.Errorf("a lookup that failed: err=%v, purged %v", err, purged)
	}
}
