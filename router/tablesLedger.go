package router

import (
	rowLedgerController "github.com/akashc777/OneCamp/controllers/RowLedger"
)

// mountRowLedger mounts the row ledger's routes (tables A2) beside today's
// table routes, on router.go's /tables behind the member group: batches,
// moves, undo and redo, a row's history, and the trash. /tables/trash is a
// path of its own, which chi matches before /tables/{id}.
func mountRowLedger(rt tablesRouters) {
	rt.Tables.Post("/{id}/rows/batch", rowLedgerController.Batch)
	rt.Tables.Post("/{id}/rows/move", rowLedgerController.MoveRows)
	rt.Tables.Post("/{id}/undo", rowLedgerController.Undo)
	rt.Tables.Post("/{id}/redo", rowLedgerController.Redo)
	rt.Tables.Get("/{id}/rows/{rowId}/history", rowLedgerController.RowHistory)
	rt.Tables.Get("/{id}/trash", rowLedgerController.Trash)
	rt.Tables.Post("/{id}/trash/restore", rowLedgerController.RestoreFromTrash)
	rt.Tables.Get("/trash", rowLedgerController.TablesTrash)
	rt.Tables.Post("/{id}/restore", rowLedgerController.RestoreTable)
}
