package business

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// correctedHome is a channel's recording holding one corrected line.
func correctedHome() *dgraphStruct.DgraphRecording {
	started := time.Now().Add(-time.Hour)
	off := int64(4200)
	return &dgraphStruct.DgraphRecording{Uid: "0x5", EgressId: "EG_1", StartedAt: &started, ObjectKey: "recordings/EG_1.mp4",
		Channel: &dgraphStruct.DgraphChannel{Uuid: "c1"},
		Transcript: []*dgraphStruct.DgraphTranscript{{Uid: "0x9", Text: "the launch budget is ninety thousand",
			OriginalText: "the lunch budget is nineteen thousand", OffsetMs: &off}}}
}

// M6: a corrected line's entry carries its words as they read now, built
// from the home the correction read, with no read of its own.
func TestACorrectedLinesEntryCarriesTheCorrectedWords(t *testing.T) {
	swap(t, &readEntry, func(context.Context, Written) (*openSearchStruct.OpenSearchTranscript, error) {
		t.Fatal("the entry was read again")
		return nil, nil
	})
	var wrote []*openSearchStruct.OpenSearchTranscript
	swap(t, &writeEntries, func(_ context.Context, es []*openSearchStruct.OpenSearchTranscript) error {
		wrote = append(wrote, es...)
		return nil
	})
	if err := IndexHomeLine(context.Background(), correctedHome(), Written{Key: "EG_1", LineUID: "0x9", SessionKey: "call-RM_1", Room: "ch-c1"}); err != nil {
		t.Fatal(err)
	}
	if len(wrote) != 1 || wrote[0].Uuid != "0x9" || wrote[0].Text != "the launch budget is ninety thousand" || wrote[0].OffsetMs != 4200 {
		t.Fatalf("wrote %+v", wrote)
	}
}

// M8: a corrected line's failed index write logs ids only and wakes the
// backfill, which rewrites the line from the graph.
func TestACorrectedLinesFailedIndexWriteLogsNoWordsAndWakesTheRepair(t *testing.T) {
	swap(t, &writeEntries, func(context.Context, []*openSearchStruct.OpenSearchTranscript) error {
		return errors.New("mapper_parsing_exception: preview 'the launch budget is ninety thousand'")
	})
	woke := 0
	swap(t, &wakeBackfill, func() { woke++ })
	var logs bytes.Buffer
	swap(t, &helpers.Logger, slog.New(slog.NewTextHandler(&logs, nil)))
	err := IndexHomeLine(context.Background(), correctedHome(), Written{Key: "EG_1", LineUID: "0x9"})
	if err == nil || woke != 1 {
		t.Fatalf("err %v, woke %d", err, woke)
	}
	if strings.Contains(logs.String()+err.Error(), "ninety") || strings.Contains(logs.String()+err.Error(), "nineteen") {
		t.Fatalf("the words are in the log %q or the error %q", logs.String(), err)
	}
	if !strings.Contains(logs.String(), "0x9") {
		t.Fatalf("the log %q doesn't name the line", logs.String())
	}
}
