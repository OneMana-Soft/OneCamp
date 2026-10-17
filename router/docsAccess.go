package router

import docController "github.com/akashc777/OneCamp/controllers/Doc"

// mountDocAccess is who may do what with a doc (A5): my level and why, the
// sharing panel and its changes, handing a doc on, an admin's recovery, the
// lock, and asking for access.
//
// It also takes over three routes router.go still declares: the share
// dialog's GET /doc/getDocPermissions and POST /doc/updateDocPermissions, and
// POST /doc/removeComment, which a doc's owner, its full-access people and
// admins may now use on anyone's comment. Mounted after router.go's routes
// (mountDocsProgramme is its last line), these are the handlers that answer: a
// method and pattern registered twice answers with the last handler, silently.
// The integrator deletes router.go's three lines and the handlers they name.
// The live socket's POST /docColab/authorize stays in router.go (its group's
// middleware isn't one docsRouters carries): docController.DocCollabAuthorize
// delegates to docController.AuthorizeDocSocket, which the integrator swaps in
// there.
func mountDocAccess(rt docsRouters) {
	rt.Doc.Get("/access/requests", docController.ListDocAccessRequests)
	rt.Doc.Get("/access/{doc_uuid}", docController.GetDocAccess)
	rt.Doc.Post("/access/update", docController.UpdateDocAccess)
	rt.Doc.Post("/access/preview", docController.PreviewDocAccess)
	rt.Doc.Post("/access/transfer", docController.TransferDoc)
	rt.Doc.Post("/access/request", docController.RequestDocAccess)
	rt.Doc.Post("/access/answer", docController.AnswerDocAccess)
	rt.Doc.Post("/lock", docController.LockDoc)
	rt.Admin.Post("/docs/recover", docController.RecoverDoc)

	// Route swaps.
	rt.Doc.Get("/getDocPermissions", docController.GetDocSharing)
	rt.Doc.Post("/updateDocPermissions", docController.UpdateDocSharing)
	rt.Doc.Post("/removeComment", docController.RemoveDocComment)
}
