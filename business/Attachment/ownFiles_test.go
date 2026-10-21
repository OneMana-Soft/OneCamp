package business

import (
	"context"
	"errors"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

func TestOwnFilesKeepsTheWritersUploadsToThePlace(t *testing.T) {
	prev := ownUploadsIn
	t.Cleanup(func() { ownUploadsIn = prev })
	writer, mine, theirs := uuid.New(), uuid.New(), uuid.New()
	var asked []uuid.UUID
	ownUploadsIn = func(_ context.Context, ids []uuid.UUID, by uuid.UUID, k, v string) (map[uuid.UUID]string, error) {
		asked = ids
		if by != writer || k != "project" || v != "p1" {
			t.Errorf("asked about %v %q %q", by, k, v)
		}
		return map[uuid.UUID]string{mine: "stored/key.png"}, nil
	}
	got, err := OwnFiles(context.Background(), writer.String(), "project", "P1", []*dgraphStruct.DgraphAttachment{
		{Uuid: mine.String(), ObjectKey: "forged"}, {Uuid: theirs.String()}, {Uuid: "not-a-uuid"}, nil, {Uuid: mine.String()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Uuid != mine.String() || got[0].ObjectKey != "stored/key.png" {
		t.Fatalf("kept %+v", got)
	}
	if len(asked) != 3 {
		t.Fatalf("asked about %d ids, want the 3 uuids", len(asked))
	}
	ownUploadsIn = func(context.Context, []uuid.UUID, uuid.UUID, string, string) (map[uuid.UUID]string, error) {
		return nil, errors.New("down")
	}
	if _, err := OwnFiles(context.Background(), writer.String(), "project", "p1", []*dgraphStruct.DgraphAttachment{{Uuid: mine.String()}}); err == nil {
		t.Fatal("a failed lookup stops the write")
	}
	if got, err := OwnFiles(context.Background(), writer.String(), "project", "p1", nil); err != nil || got != nil {
		t.Fatal("no files asks nothing")
	}
}
