package router

import (
	entityLinkController "github.com/akashc777/OneCamp/controllers/EntityLink"
	whoKnowsController "github.com/akashc777/OneCamp/controllers/WhoKnows"
	workGraphController "github.com/akashc777/OneCamp/controllers/WorkGraph"
)

// mountConnectedWork is connected work (C1): what live chips show, in docs
// and anywhere else that draws them (/entities, on its own path because chat,
// tables and the calendar ask it too); who an "@" in a doc reaches; and
// to-dos that become tasks and stay in step with them; and the work graph
// (DG5): what a thing is connected to, and how two things are connected
// (/graph, members only: every node is decided as the reader); and who knows
// about something (DG9, /people/who-knows, members only: counts and evidence
// come only from what the asker can read, and anyone may leave themselves
// out with /people/who-knows/me).
func mountConnectedWork(rt docsRouters) {
	rt.Member.Post("/entities", entityLinkController.ReadEntities)
	rt.Member.Get("/graph/connected", workGraphController.GetConnected)
	rt.Member.Get("/graph/path", workGraphController.GetPath)
	rt.Member.Get("/people/who-knows", whoKnowsController.GetWhoKnows)
	rt.Member.Get("/people/who-knows/me", whoKnowsController.GetMyWhoKnows)
	rt.Member.Put("/people/who-knows/me", whoKnowsController.SetMyWhoKnows)
	rt.Doc.Post("/mentionAccess", entityLinkController.MentionAccess)
	rt.Doc.Post("/todoTask", entityLinkController.TodoToTask)
	rt.Doc.Post("/todoTask/check", entityLinkController.CheckTodo)
}
