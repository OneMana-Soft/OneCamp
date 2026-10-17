package router

import docController "github.com/akashc777/OneCamp/controllers/Doc"

// mountDocChrome is a doc's page chrome (B6): its look, what links to it, a
// doc in the trash as its page shows it, and an editor's invites.
//
// It also answers GET /doc/getDocInfo/{doc_uuid}, the one way a doc is opened:
// GetDocPage, the doc with its look, lock and owner. GetDocInfo and router.go's
// line for it went when B6 merged onto the A5×A6 join.
func mountDocChrome(rt docsRouters) {
	rt.Doc.Get("/backlinks/{doc_uuid}", docController.GetDocBacklinks)
	rt.Doc.Get("/trashed/{doc_uuid}", docController.GetTrashedDoc)
	rt.Doc.Post("/look", docController.SetDocLook)
	rt.Doc.Post("/access/invite", docController.InviteToDoc)
	rt.Doc.Get("/getDocInfo/{doc_uuid}", docController.GetDocPage)
}
