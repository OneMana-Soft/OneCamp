package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	bulkModel "github.com/akashc777/OneCamp/models/openSearch/Bulk"
)

// perTextEmbedder gives each text its own vector, and records each call.
type perTextEmbedder struct {
	calls [][]string
	err   error
}

func (e *perTextEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls = append(e.calls, texts)
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(i + 1), 0, 0}
	}
	return out, nil
}

func (e *perTextEmbedder) Dimensions() int { return 3 }

// Several boards' entries: their vectors asked for a chunk at a time, and
// every entry written in one bulk request with its board's access.
func TestBoardEntriesAreEmbeddedInChunksAndWrittenInOneRequest(t *testing.T) {
	e := &perTextEmbedder{}
	old, oldUpsert := servicePtr.Load(), upsertEntries
	servicePtr.Store(enabledService(e))
	t.Cleanup(func() { servicePtr.Store(old); upsertEntries = oldUpsert })
	var writes [][]bulkModel.Upsert
	upsertEntries = func(_ context.Context, index string, docs []bulkModel.Upsert) error {
		if index != AI_EMBEDDINGS_INDEX {
			t.Errorf("written to %s", index)
		}
		writes = append(writes, docs)
		return nil
	}
	var boards []BoardContent
	for i := 0; i < 20; i++ {
		boards = append(boards, BoardContent{BoardUUID: fmt.Sprint("b", i), Text: fmt.Sprint("Board ", i),
			Access: BoardAccess{CreatedBy: "maya", Reading: []string{"ben"}}})
	}
	boards = append(boards, BoardContent{BoardUUID: "blank", Text: "  "})
	if err := EmbedBoardContents(context.Background(), boards); err != nil {
		t.Fatal(err)
	}
	if len(e.calls) != 2 || len(e.calls[0]) != storeEmbeddingsChunk || len(e.calls[1]) != 4 {
		t.Fatalf("the model was asked %d times: %v", len(e.calls), e.calls)
	}
	if len(writes) != 1 || len(writes[0]) != 20 || writes[0][0].ID != BoardContentType+":b0" || writes[0][19].ID != BoardContentType+":b19" {
		t.Fatalf("written: %d requests, %+v", len(writes), writes)
	}
	var last map[string]any
	if err := json.Unmarshal(writes[0][19].Doc.(json.RawMessage), &last); err != nil {
		t.Fatal(err)
	}
	if last["content_text"] != "Board 19" || last["board_public"] != false || last["board_created_by_user_id"] != "maya" ||
		fmt.Sprint(last["board_reading_users"]) != "[ben]" || fmt.Sprint(last["board_editing_users"]) != "[]" ||
		fmt.Sprint(last["embedding"]) != "[4 0 0]" {
		t.Fatalf("the last entry: %v", last)
	}

	e.err, writes = errors.New("model down"), nil
	if err := EmbedBoardContents(context.Background(), boards[:2]); err == nil || len(writes) != 0 {
		t.Fatalf("a model that failed: %v, %d writes", err, len(writes))
	}
}
