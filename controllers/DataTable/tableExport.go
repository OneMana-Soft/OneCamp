package controllers

// POST /tables/{id}/export: a view of a table as an Excel workbook
// (business/DataTable xlsxExport.go); POST /tables/export: every table the
// reader can open as one, a sheet each (tablesExport.go). Each is written as
// it streams out, and each is one row in the workspace's admin audit log.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	adminAuditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	business "github.com/akashc777/OneCamp/business/DataTable"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
)

// ExportTable POST /tables/{id}/export {fields, widths, spec | rows} → the
// view as an .xlsx file.
func ExportTable(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	id, ok := idParam(r, "id")
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var in business.ExportInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	export, err := business.PrepareExport(ctx, id, actorFrom(r), in)
	if err != nil {
		if business.IsExportInput(err) {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
			return
		}
		WriteQueryErr(w, r, err)
		return
	}
	recordExport(r, "table.exported", "Exported a table to Excel", []string{id.String()})
	sendWorkbook(ctx, w, export.Name, export.WriteXLSX)
}

// ExportTables POST /tables/export → every table the reader can open as one
// .xlsx file, a sheet each; a reader who can open none, or more than one
// workbook holds, is refused in words (422).
func ExportTables(w http.ResponseWriter, r *http.Request) {
	ctx := readerCtx(r)
	export, err := business.PrepareTablesExport(ctx, actorFrom(r))
	if err != nil {
		if business.IsTablesExportRefused(err) {
			helpers.WriteJSON(w, http.StatusUnprocessableEntity, helpers.Envolope{"msg": err.Error()})
			return
		}
		WriteQueryErr(w, r, err)
		return
	}
	tables := export.Tables()
	recordExport(r, "tables.exported", fmt.Sprintf("Exported every table they can open to Excel, %d in one workbook", len(tables)), tables)
	sendWorkbook(ctx, w, "Tables", export.WriteXLSX)
}

// recordExport writes an export's one row in the workspace's admin audit log,
// under the tables' area: who exported, the ids of the tables the workbook
// holds (never their names: a private table's name isn't the log's to show),
// and when. A workbook is the tables' data leaving OneCamp. The row is written
// as the workspace export's is (controllers/ContentIO): handed to the log's
// writer once the export is allowed, before the file streams out; a row that
// can't be written is logged there, and the export goes ahead.
func recordExport(r *http.Request, action, summary string, tables []string) {
	adminAuditBusiness.Record(r, action, adminAuditBusiness.CategoryTables, summary,
		map[string]interface{}{"table_ids": tables, "format": "xlsx"})
}

// sendWorkbook answers an Excel workbook, its file named name, as write
// writes it.
func sendWorkbook(ctx context.Context, w http.ResponseWriter, name string, write func(context.Context, io.Writer) ([]string, error)) {
	h := w.Header()
	h.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	h.Set("Content-Disposition", uploadsafe.BuildDisposition(uploadsafe.SanitiseFileName(name, 80)+".xlsx"))
	h.Set("Cache-Control", "no-store")
	if _, err := write(ctx, w); err != nil {
		// The file has begun, so no answer can say so: the connection ends
		// unfinished, and the reader's download fails rather than keeping
		// part of a workbook. The line never names the file: for one
		// table's export it is the table's name, and a private table's name
		// isn't the log's to show.
		helpers.LogErrorWithContext(ctx, "controllers/DataTable a workbook broke off: %v", err)
		panic(http.ErrAbortHandler)
	}
}

// WriteQueryErr answers a read of a table's rows by a spec that failed, for
// POST /tables/{id}/query and /export alike: a table its reader can't open
// is not found, a spec Row Query can't answer is a 422 in its words, and
// anything else is a 503 to try again.
func WriteQueryErr(w http.ResponseWriter, r *http.Request, err error) {
	var working *rowQuery.StillWorkingOut
	switch {
	case business.IsNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "table not found"})
	case errors.As(err, &working):
		// A formula or a rollup still being filled (airtable-03b): how far, by
		// its code, so the view shows it and asks again; its words for the rest.
		helpers.WriteJSON(w, http.StatusUnprocessableEntity, helpers.Envolope{
			"msg":      rowQuery.Refusal(working),
			"code":     "working_out",
			"progress": map[string]int64{"done": working.Done, "total": working.Total},
		})
	case errors.Is(err, query.ErrBadSpec), errors.Is(err, rowQuery.ErrCantQuery), errors.Is(err, rowQuery.ErrBadCursor), errors.Is(err, rowQuery.ErrTooManyToWorkOut):
		helpers.WriteJSON(w, http.StatusUnprocessableEntity, helpers.Envolope{"msg": rowQuery.Refusal(err)})
	case business.IsUnavailable(err):
		helpers.WriteRetryLater(w, "Couldn't load the rows.")
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/DataTable rows by spec: %v", err)
		helpers.WriteRetryLater(w, "Couldn't load the rows.")
	}
}
