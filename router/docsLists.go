package router

import docController "github.com/akashc777/OneCamp/controllers/Doc"

// mountDocLists is the docs home's lists (A3): Recent, Mine, Shared with
// me, Workspace, Starred and Archived, each with its exact count.
func mountDocLists(rt docsRouters) {
	rt.Doc.Get("/list", docController.GetDocList)
}
