package businness

// Blocks moved into a doc from another (docs programme B1, "Move to…" in the
// block menu): they are appended at the target's end through the Live Doc
// Writer, so whoever has it open sees them arrive, and the browser that moved
// them deletes them from their own doc once this has answered
// (components/docEditor/blockMove.ts).

import (
	"context"
	"errors"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	"github.com/akashc777/OneCamp/services/collab"
)

// ErrNoBlocks is a move with nothing in it.
var ErrNoBlocks = errors.New("no blocks to move")

// AppendBlocksLive adds blocks, given as the editor's JSON, at the end of a
// doc. They are written as the codec writes them: held to the editor's
// schema, with no unsafe link or image (read back through the HTML reader),
// and without block ids, which they are given where they land.
func AppendBlocksLive(ctx context.Context, docUUID string, content *contentcodec.Node, by collab.By) (collab.Result, error) {
	doc, err := contentcodec.FromHTML(contentcodec.ToHTML(contentcodec.Normalize(content, nil)), contentcodec.HTMLOptions{})
	if err != nil {
		return collab.Result{}, err
	}
	if blankDoc(doc) {
		return collab.Result{}, ErrNoBlocks
	}
	return collab.Live.Append(ctx, docUUID, contentcodec.ToHTML(doc), collab.Options{By: by})
}

// blankDoc is a doc of one empty paragraph: what Normalize makes of nothing.
func blankDoc(doc *contentcodec.Node) bool {
	return doc == nil || len(doc.Content) == 0 || (len(doc.Content) == 1 && doc.Content[0].Type == "paragraph" && len(doc.Content[0].Content) == 0)
}
