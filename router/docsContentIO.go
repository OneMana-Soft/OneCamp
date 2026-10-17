package router

import (
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	contentIOController "github.com/akashc777/OneCamp/controllers/ContentIO"
)

// mountContentIO is A7's routes (docs programme): import files as docs and
// follow the job, export a doc, and record an export the browser made.
//
// Exports sit behind the docs.export.members policy: with it off, a member's
// export (and the record of a browser export) answers 403; admins always pass.
// An import's file is sent with the request up to 7 MiB (/doc caps a body at
// 8); a larger one is put straight into storage at a presigned address, then
// started.
func mountContentIO(rt docsRouters) {
	rt.Doc.Post("/import", contentIOController.ImportDoc)
	rt.Doc.Post("/import/presign", contentIOController.PresignImport)
	rt.Doc.Post("/import/{job_id}/start", contentIOController.StartImport)
	rt.Member.Get("/content-io/jobs/{job_id}", contentIOController.GetJob)
	rt.Member.Get("/content-io/capabilities", contentIOController.GetCapabilities)

	exportGate := policyBusiness.Policies.Require(policyBusiness.DocsExportMembers)
	rt.Doc.With(exportGate).Post("/export", contentIOController.ExportDoc)
	rt.Doc.With(exportGate).Post("/recordExport", contentIOController.RecordExport)
}
