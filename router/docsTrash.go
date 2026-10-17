package router

import docController "github.com/akashc777/OneCamp/controllers/Doc"

// mountDocTrash is the trash for members (A3): a page and its subpages
// deleted as one, listed, restored together, and deleted for good. It also
// takes over POST /doc/deleteDoc, which now deletes the subtree: the
// integrator deletes router.go's line for it when this mounts.
func mountDocTrash(rt docsRouters) {
	rt.Doc.Post("/delete", docController.DeleteDocTree)
	rt.Doc.Post("/deleteDoc", docController.DeleteDocTree)
	rt.Doc.Get("/trash", docController.GetDocTrash)
	rt.Doc.Post("/restore", docController.RestoreDocFromTrash)
	rt.Doc.Post("/purge", docController.PurgeDocForGood)
}
