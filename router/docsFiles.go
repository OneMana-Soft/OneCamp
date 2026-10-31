package router

import (
	"net/http"

	docController "github.com/akashc777/OneCamp/controllers/Doc"
)

// mountDocFiles is F1's routes (docs programme, Docs & Files): a file
// uploaded into the doc tree, a new version of one, and its versions; and
// nodes coloured in bulk. A version downloads through the doc's own file
// route, GET /doc/getFile/{file}/{attachment_id}, and a folder or a link is
// made with POST /doc/createDoc.
func mountDocFiles(rt docsRouters) {
	mountFileRoutes(rt, docController.AddDocFile, docController.AddDocFileVersion, docController.GetDocFileVersions)
	rt.Doc.Post("/color", docController.SetDocColor)
}

// mountFileRoutes puts the file routes where they belong: the two that take
// an upload on Files, past /doc's 8 MiB cap, whose handlers cap the body at
// the workspace's upload limit themselves; the versions' GET on /doc. Apart
// from mountDocFiles so a test can stand in for the handlers.
func mountFileRoutes(rt docsRouters, add, addVersion, versions http.HandlerFunc) {
	rt.Files.Post("/doc/files", add)
	rt.Files.Post("/doc/files/{doc_uuid}/versions", addVersion)
	rt.Doc.Get("/files/{doc_uuid}/versions", versions)
}
