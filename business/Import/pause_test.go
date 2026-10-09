package business

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

func TestALongProviderLimitIsWrittenOnTheJobAndClearedWhenWorkGoesOn(t *testing.T) {
	var saved []map[string]any
	announced := 0
	oldSave, oldAnnounce := saveProgress, announcePaused
	t.Cleanup(func() { saveProgress, announcePaused = oldSave, oldAnnounce })
	saveProgress = func(_ context.Context, _ uuid.UUID, patch json.RawMessage) error {
		var m map[string]any
		_ = json.Unmarshal(patch, &m)
		saved = append(saved, m)
		return nil
	}
	announcePaused = func(context.Context, *importModels.Job) { announced++ }

	job := &importModels.Job{Id: uuid.New(), Provider: "monday"}
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	daily := fmt.Errorf("items: %w", &importProvider.ErrRateLimited{RetryAfter: 6 * time.Hour, Reason: "monday.com daily API limit for this plan is used up"})

	// A limit a nap covers is just waited out.
	notePause(context.Background(), job, &importProvider.ErrRateLimited{RetryAfter: 30 * time.Second}, 30*time.Second, now)
	if len(saved) != 0 {
		t.Fatalf("a short limit was written down: %v", saved)
	}

	// monday.com's daily limit: said, with when it lifts, and announced once.
	notePause(context.Background(), job, daily, 6*time.Hour, now)
	notePause(context.Background(), job, daily, 6*time.Hour, now)
	if len(saved) != 2 || saved[0]["paused_until"] != "2026-10-09T14:00:00Z" || saved[0]["pause_reason"] != "monday.com daily API limit for this plan is used up" {
		t.Fatalf("the pause: %v", saved)
	}
	if announced != 1 {
		t.Errorf("announced %d times, want once", announced)
	}

	// The first chunk done afterwards clears it; later ones touch nothing.
	clearPause(context.Background(), job.Id)
	clearPause(context.Background(), job.Id)
	if len(saved) != 3 || saved[2]["paused_until"] != nil || saved[2]["pause_reason"] != nil {
		t.Fatalf("cleared: %v", saved)
	}
	if _, has := saved[2]["paused_until"]; !has {
		t.Error("the clear writes the key, as null")
	}
}
