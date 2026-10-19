package router

import (
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
)

// mountRowPages mounts a row's page (tables T2, rows as pages) on router.go's
// /tables behind the member group: reading it with the row's record, and
// making it the first time someone writes in it. Its live editing rides the
// docs' session (/docColab/session), which opens a row's page by its row.
func mountRowPages(rt tablesRouters) {
	rt.Tables.Get("/{id}/rows/{rowId}/page", dataTableController.GetRowPage)
	rt.Tables.Post("/{id}/rows/{rowId}/page", dataTableController.EnsureRowPage)
}
