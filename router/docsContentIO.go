package router

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	boardController "github.com/akashc777/OneCamp/controllers/Board"
	contentIOController "github.com/akashc777/OneCamp/controllers/ContentIO"
	"github.com/akashc777/OneCamp/helpers/idempotency"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// mountContentIO is A7's routes (docs programme): import files as docs and
// follow the job, answer an import's preview (a spreadsheet's, tables S1),
// export a doc, and record an export the browser made.
//
// Exports sit behind the docs.export.members policy: with it off, a member's
// export (and the record of a browser export) answers 403; admins always pass.
// An import's file is sent with the request up to 7 MiB (/doc caps a body at
// 8); a larger one is put straight into storage at a presigned address, then
// started.
//
// The two routes that make an import take Idempotency-Key (docs W3): the web
// sends one with each file, so a resend under it is answered from the first
// answer and imports nothing twice. Starting an uploaded file needs none: a
// job starts once however often it is asked (contentIOBusiness StartUploaded).
func mountContentIO(rt docsRouters) {
	rt.Doc.With(idempotency.Middleware("POST /doc/import", idempotency.InWords)).Post("/import", contentIOController.ImportDoc)
	rt.Doc.With(idempotency.Middleware("POST /doc/import/presign", idempotency.InWords)).Post("/import/presign", contentIOController.PresignImport)
	rt.Doc.Post("/import/{job_id}/start", contentIOController.StartImport)
	rt.Doc.Post("/import/{job_id}/choose", contentIOController.ChooseImport)
	rt.Member.Get("/content-io/jobs/{job_id}", contentIOController.GetJob)
	rt.Member.Get("/content-io/capabilities", contentIOController.GetCapabilities)

	exportGate := policyBusiness.Policies.Require(policyBusiness.DocsExportMembers)
	rt.Doc.With(exportGate).Post("/export", contentIOController.ExportDoc)
	rt.Doc.With(exportGate, exportTells("doc-export")).Post("/recordExport", contentIOController.RecordExport)
}

// exportTellsMsg is what a member hears when their export tells (a doc's or a
// board's, for the audit log) pass exportTells' budget: the export waits,
// since an export the log can't record isn't made (V-363).
const exportTellsMsg = "You've exported a lot in the last few minutes. Wait a few minutes and try again."

// exportTells is the budget of an export's tells to the audit log on one
// surface (kind): 60 a member in UserRateLimit's window. It counts by the
// member who asks, not by the address, so one office's busy exporter never
// costs a colleague behind the same address their audit rows (V-363).
func exportTells(kind string) func(http.Handler) http.Handler {
	return customMiddleware.UserRateLimit(kind, 60, exportTellsMsg)
}

// mountBoardExportTell is /board/recordExport: an export the canvas made in
// the browser, told to the admin audit log (miro-44), under exportTells.
func mountBoardExportTell(r chi.Router) {
	r.With(exportTells("board-export")).Post("/recordExport", boardController.RecordBoardExport)
}
