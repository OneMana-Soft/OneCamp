package business

import (
	"context"
	"errors"
	"testing"

	domain "github.com/akashc777/OneCamp/domain/Attachment"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

func TestOwnFilesKeepsTheWritersUploadsToThePlace(t *testing.T) {
	prev := ownUploads
	t.Cleanup(func() { ownUploads = prev })
	writer, mine, elsewhere, theirs := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var asked []uuid.UUID
	ownUploads = func(_ context.Context, ids []uuid.UUID, by uuid.UUID) (map[uuid.UUID]domain.Upload, error) {
		asked = ids
		if by != writer {
			t.Errorf("asked about %v's uploads", by)
		}
		return map[uuid.UUID]domain.Upload{
			mine:      {SrcKey: "project", SrcValue: "p1", ObjKey: "stored/key.png"},
			elsewhere: {SrcKey: "project", SrcValue: "p2", ObjKey: "other/key.png"},
		}, nil
	}
	got, err := OwnFiles(context.Background(), writer.String(), "project", "P1", []*dgraphStruct.DgraphAttachment{
		{Uuid: mine.String(), ObjectKey: "forged"}, {Uuid: elsewhere.String()}, {Uuid: theirs.String()}, {Uuid: "not-a-uuid"}, nil, {Uuid: mine.String()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Uuid != mine.String() || got[0].ObjectKey != "stored/key.png" {
		t.Fatalf("kept %+v", got)
	}
	if len(asked) != 4 {
		t.Fatalf("asked about %d ids, want the 4 uuids", len(asked))
	}
	ownUploads = func(context.Context, []uuid.UUID, uuid.UUID) (map[uuid.UUID]domain.Upload, error) {
		return nil, errors.New("down")
	}
	if _, err := OwnFiles(context.Background(), writer.String(), "project", "p1", []*dgraphStruct.DgraphAttachment{{Uuid: mine.String()}}); err == nil {
		t.Fatal("a failed lookup stops the write")
	}
	if got, err := OwnFiles(context.Background(), writer.String(), "project", "p1", nil); err != nil || got != nil {
		t.Fatal("no files asks nothing")
	}
}

// A list sent to several places (a forward) is read once, and each place
// keeps only the files uploaded to it.
func TestOwnUploadsAreReadOnceForEveryPlace(t *testing.T) {
	prev := ownUploads
	t.Cleanup(func() { ownUploads = prev })
	writer, toChannel, toGroup := uuid.New(), uuid.New(), uuid.New()
	reads := 0
	ownUploads = func(context.Context, []uuid.UUID, uuid.UUID) (map[uuid.UUID]domain.Upload, error) {
		reads++
		return map[uuid.UUID]domain.Upload{
			toChannel: {SrcKey: "channel", SrcValue: "c1", ObjKey: "k/channel.png"},
			toGroup:   {SrcKey: "grpChat", SrcValue: "g1", ObjKey: "k/group.png"},
		}, nil
	}
	list := []*dgraphStruct.DgraphAttachment{{Uuid: toChannel.String()}, {Uuid: toGroup.String()}}
	own, err := OwnUploads(context.Background(), writer.String(), list)
	if err != nil {
		t.Fatal(err)
	}
	if got := own.In("channel", "c1", list); len(got) != 1 || got[0].Uuid != toChannel.String() || got[0].ObjectKey != "k/channel.png" {
		t.Errorf("the channel keeps %+v", got)
	}
	if got := own.In("grpChat", "g1", list); len(got) != 1 || got[0].Uuid != toGroup.String() || got[0].ObjectKey != "k/group.png" {
		t.Errorf("the group keeps %+v", got)
	}
	if got := own.In("chat", "c1", list); len(got) != 0 {
		t.Errorf("a DM with the channel's id keeps %+v", got)
	}
	if reads != 1 {
		t.Errorf("%d reads, want one for every place", reads)
	}
}
