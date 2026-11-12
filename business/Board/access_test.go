package business

import (
	"context"
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// ReadableBoard answers only as the board asked for, to one who may read it:
// an id that isn't a uuid, a board the graph hasn't got, one that answers as
// another board, and a private one the person isn't on all read as no board;
// the graph's other errors stay its own.
func TestReadableBoardAnswersOnlyAsTheBoardAskedFor(t *testing.T) {
	old := readBasicBoard
	t.Cleanup(func() { readBasicBoard = old })
	asked, other := uuid.NewString(), uuid.NewString()
	private, open := true, false
	down := errors.New("dgraph is down")
	var answer *dgraphStruct.DgraphBoard
	var answerErr error
	reads := 0
	readBasicBoard = func(_ context.Context, id, _ string) (*dgraphStruct.DgraphBoard, error) {
		reads++
		if id != asked {
			t.Errorf("read %q, want the parsed uuid %q", id, asked)
		}
		return answer, answerErr
	}
	for _, c := range []struct {
		name    string
		id      string
		board   *dgraphStruct.DgraphBoard
		err     error
		want    error
		readsIt bool
	}{
		{"a Dgraph uid is no board's uuid", "0x1a", nil, nil, ErrBoardNotFound, false},
		{"the graph hasn't got it", asked, nil, helpers.ErrNotFound, ErrBoardNotFound, true},
		{"it answers as another board", asked, &dgraphStruct.DgraphBoard{Uuid: other, IsPrivate: &open}, nil, ErrBoardNotFound, true},
		{"a private board the person isn't on", asked, &dgraphStruct.DgraphBoard{Uuid: asked, IsPrivate: &private}, nil, ErrBoardNotFound, true},
		{"the graph's own error", asked, nil, down, down, true},
		{"an open board", asked, &dgraphStruct.DgraphBoard{Uuid: asked, IsPrivate: &open}, nil, nil, true},
	} {
		answer, answerErr, reads = c.board, c.err, 0
		got, err := ReadableBoard(context.Background(), c.id, "0x9", "person")
		if !errors.Is(err, c.want) || (c.want == nil) != (got != nil) {
			t.Errorf("%s: %v, %v; want %v", c.name, got, err, c.want)
		}
		if (reads > 0) != c.readsIt {
			t.Errorf("%s: read the graph %d times", c.name, reads)
		}
	}
}
