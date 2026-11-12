package router

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	boardController "github.com/akashc777/OneCamp/controllers/Board"
	contentIOController "github.com/akashc777/OneCamp/controllers/ContentIO"
	"github.com/akashc777/OneCamp/helpers"
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
	rt.Doc.With(exportGate, exportTells("doc-export", "doc", auditBusiness.CategoryDocs)).Post("/recordExport", contentIOController.RecordExport)
}

// exportTellsMsg is what a member hears when their export tells (a doc's or a
// board's, for the audit log) pass exportTells' budget: the export waits,
// since an export the log can't record isn't made (V-363).
const exportTellsMsg = "You've exported a lot in the last few minutes. Wait a few minutes and try again."

// exportTellsMax is a member's budget of export tells on one surface in
// UserRateLimit's window.
const exportTellsMax = 60

// exportTells is the budget of an export's tells to the audit log on one
// surface (kind): exportTellsMax a member in UserRateLimit's window. It counts
// by the member who asks, not by the address, so one office's busy exporter
// never costs a colleague behind the same address their audit rows (V-363).
//
// An export held back is recorded too (miro-44): the first tell the budget
// refuses in a member's window writes one "<noun>.export_held" row under
// category, naming the member, before the 429 is written. Later refusals in
// that window write none, so every held burst is in the log and pressing on
// can't flood the chain. The row names no doc or board: the limiter answers
// before the body is read, and a refused tell made no file (the web makes an
// export only after its tell is taken).
func exportTells(kind, noun, category string) func(http.Handler) http.Handler {
	return customMiddleware.UserRateLimit(kind, exportTellsMax, exportTellsMsg, func(r *http.Request) {
		recordExportHeld(r, noun, category)
	})
}

// recordExportHeld writes an export_held row for the member on r. A row that
// can't be written is logged: the export stays held either way.
func recordExportHeld(r *http.Request, noun, category string) {
	ctx := r.Context()
	p := auditBusiness.ContextPrincipal(ctx, "")
	summary := fmt.Sprintf("Exports held back: past %d %s exports in a few minutes", exportTellsMax, noun)
	meta := p.Meta(map[string]interface{}{"limit": exportTellsMax, "surface": noun})
	if err := auditBusiness.RecordForPrincipal(ctx, p.ID, p.Email, p.Kind, noun+".export_held", category, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "router: %s.export_held wasn't recorded: %+v", noun, err)
	}
}

// mountBoardExportTell is /board/recordExport: an export the canvas made in
// the browser, told to the admin audit log (miro-44), under exportTells.
func mountBoardExportTell(r chi.Router) {
	r.With(exportTells("board-export", "board", auditBusiness.CategoryBoards)).Post("/recordExport", boardController.RecordBoardExport)
}
