package router

import docHomeController "github.com/akashc777/OneCamp/controllers/DocHome"

// mountDocsHome is the docs home (B5): search over docs' bodies, a template
// into an empty doc, and a doc saved as a template. The docs home's lists are
// A3's (mountDocLists); the old title-only searches (/doc/searchPrivate and
// /doc/searchPublic) and the two old lists (/doc/getPrivateDoc and
// /doc/getPublicDoc) stay in router.go until the integrator deletes them.
func mountDocsHome(rt docsRouters) {
	rt.Doc.Get("/search", docHomeController.SearchDocs)
	rt.Doc.Post("/applyTemplate", docHomeController.ApplyTemplate)
	rt.Doc.Post("/saveAsTemplate", docHomeController.SaveAsTemplate)
}
