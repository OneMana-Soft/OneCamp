package controllers

// POST /tables/{id}/export: a view of a table as an Excel workbook
// (business/DataTable xlsxExport.go), written as it streams out.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

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
	h := w.Header()
	h.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	h.Set("Content-Disposition", uploadsafe.BuildDisposition(uploadsafe.SanitiseFileName(export.Name, 80)+".xlsx"))
	h.Set("Cache-Control", "no-store")
	if _, err := export.WriteXLSX(ctx, w); err != nil {
		// The file has begun, so no answer can say so: the connection ends
		// unfinished, and the reader's download fails rather than keeping
		// part of a workbook.
		helpers.LogErrorWithContext(ctx, "controllers/DataTable ExportTable: %v", err)
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
			"msg":      refusal(working),
			"code":     "working_out",
			"progress": map[string]int64{"done": working.Done, "total": working.Total},
		})
	case errors.Is(err, query.ErrBadSpec), errors.Is(err, rowQuery.ErrCantQuery), errors.Is(err, rowQuery.ErrBadCursor), errors.Is(err, rowQuery.ErrTooManyToWorkOut):
		helpers.WriteJSON(w, http.StatusUnprocessableEntity, helpers.Envolope{"msg": refusal(err)})
	case business.IsUnavailable(err):
		helpers.WriteRetryLater(w, "Couldn't load the rows.")
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/DataTable rows by spec: %v", err)
		helpers.WriteRetryLater(w, "Couldn't load the rows.")
	}
}

// refusal is a query's refusal in its own words, for the person whose view
// asked: "Grouped this way, the rows fall into more than 1,000 groups: …".
// The kind it's of ("row query: that can't be asked of this table yet: ") is
// for the logs; the view's notice showed it to its readers (CR-62).
func refusal(err error) string {
	msg := err.Error()
	for _, kind := range []error{rowQuery.ErrCantQuery, query.ErrBadSpec} {
		msg = strings.TrimPrefix(msg, kind.Error()+": ")
	}
	msg = strings.TrimPrefix(strings.TrimPrefix(msg, "row query: "), "query: ")
	r, n := utf8.DecodeRuneInString(msg)
	if n == 0 {
		return msg
	}
	return string(unicode.ToUpper(r)) + msg[n:]
}
