package router

import docController "github.com/akashc777/OneCamp/controllers/Doc"

// B1, block interactions (docs programme, PROGRAM.md §7 B1): "Move to…" for
// blocks. They are appended to the chosen doc through the Live Doc Writer;
// the browser then deletes them from their own doc in one transaction
// (components/docEditor/blockMove.ts).
func mountDocBlocks(rt docsRouters) {
	rt.Doc.Post("/blocks/append", docController.AppendDocBlocks)
}
