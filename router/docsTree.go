package router

import docController "github.com/akashc777/OneCamp/controllers/Doc"

// mountDocTree is the doc tree (A3): a page's children and path, moves, and
// duplicates. It also takes over POST /doc/createDoc (the second version,
// which places a doc in the tree): the integrator deletes router.go's line
// for it when this mounts.
func mountDocTree(rt docsRouters) {
	rt.Doc.Get("/children", docController.GetDocChildren)
	rt.Doc.Get("/ancestors/{doc_uuid}", docController.GetDocAncestors)
	rt.Doc.Post("/move", docController.MoveDoc)
	rt.Doc.Post("/move/preview", docController.PreviewDocMove)
	rt.Doc.Post("/duplicate", docController.DuplicateDoc)
	rt.Doc.Post("/createDoc", docController.CreateDocInTree)
}
