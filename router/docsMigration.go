package router

import (
	// The other tools' exports the doc import reads (Notion, Confluence,
	// Google Takeout), registered with it.
	_ "github.com/akashc777/OneCamp/business/ContentIO/adapters"
	contentIOController "github.com/akashc777/OneCamp/controllers/ContentIO"
)

// mountDocsMigration is C4's routes (docs programme): an admin exports the
// workspace's docs as a zip and downloads it, and a doc's owner lets the doc
// into those exports or takes it out (controllers/ContentIO).
func mountDocsMigration(rt docsRouters) {
	rt.Admin.Get("/docs/exports", contentIOController.ListWorkspaceExports)
	rt.Admin.Post("/docs/exports", contentIOController.StartWorkspaceExport)
	rt.Admin.Get("/docs/exports/{job_id}", contentIOController.GetWorkspaceExport)
	rt.Doc.Get("/exportConsent", contentIOController.GetExportConsent)
	rt.Doc.Post("/exportConsent", contentIOController.SetExportConsent)
}
