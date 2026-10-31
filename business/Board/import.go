package business

// A board brought in from another tool (business/Import, W-2), drawn on the
// server: made private to whoever imports it (CreateBoard), then given its
// drawing as its first saved state, saved as the collaboration service saves
// a board, so every client and guest opens it as any other.

import (
	"context"
	"fmt"

	adapter "github.com/akashc777/OneCamp/adapter/Board"
	"github.com/akashc777/OneCamp/business/Board/scene"
)

// DrawBoard saves elements and files as the board's state: the Yjs update the
// collaboration service opens it from, in the clean style (O4), with what a
// person sees counted in its snippet, and maker named as the maker of every
// note on it (the notes' CSV says who wrote each one). It takes the save every
// change to a board takes (UpdateBoardFromCollab): the state in object
// storage, its pointer in the graph, and a version kept for its history.
func DrawBoard(ctx context.Context, boardUUID string, elements []scene.Element, files []scene.File, contributors []string, maker scene.Maker) error {
	state, live, err := scene.State(uint32(scene.Seed(boardUUID)), elements, files, map[string]any{"style": "clean"},
		scene.NoteAuthors(scene.NotesMadeBy(elements, maker)))
	if err != nil {
		return fmt.Errorf("drawing board %s: %w", boardUUID, err)
	}
	return UpdateBoardFromCollab(ctx, &adapter.InputBoardCollabUpdate{
		BoardUuid: boardUUID, State: state, Snippet: fmt.Sprintf("%d elements", live), Contributors: contributors,
	})
}
