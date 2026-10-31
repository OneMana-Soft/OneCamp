package router

import (
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	contentIOController "github.com/akashc777/OneCamp/controllers/ContentIO"
	"github.com/akashc777/OneCamp/helpers/idempotency"
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
	rt.Doc.With(exportGate).Post("/recordExport", contentIOController.RecordExport)
}
