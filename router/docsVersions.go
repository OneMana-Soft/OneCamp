package router

import (
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// A4, live writes and versions (docs programme, PROGRAM.md §6 A4).
//
// These replace router.go's old lines, deleted at A4's merge (chi keeps the
// last handler registered for a method and pattern without a word):
//   - /doc/updateDoc        docController.UpdateDocBody      -> UpdateDocBodyLive
//   - /docColab/updateDoc   docController.UpdateDocFromCollab -> SaveDocFromCollab
//     (now with a body limit: the service's save is the body and its state)
//   - docBusiness.StartDocSnapshotCleanupLoop(), which did nothing any more: the
//     Snapshot Service's sweeps keep the versions.
//
// /doc/getDocSnapshots and /doc/restoreDocSnapshot stay as they are until the
// history sheet (B8) moves to /doc/versions: their restore is already live.
func mountDocVersions(rt docsRouters) {
	rt.Doc.Get("/versions", docController.ListDocVersions)
	rt.Doc.Post("/versions", docController.TakeDocVersion)
	rt.Doc.Get("/versions/{version_id}", docController.PreviewDocVersion)
	rt.Doc.Post("/versions/{version_id}/name", docController.NameDocVersion)
	rt.Doc.Post("/versions/{version_id}/restore", docController.RestoreDocVersion)

	rt.Doc.Post("/updateDoc", docController.UpdateDocBodyLive)
	rt.Internal.With(customMiddleware.BodyLimit(docColabSaveLimit)).Post("/updateDoc", docController.SaveDocFromCollab)
}

// docColabSaveLimit caps one save from the collaboration service: a body and
// its Yjs state in base64, about 2.4 times the body, for a body up to 16 MiB.
const docColabSaveLimit = 48 << 20
