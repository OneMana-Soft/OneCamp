package router

import entityLinkController "github.com/akashc777/OneCamp/controllers/EntityLink"

// mountConnectedWork is connected work (C1): what live chips show, in docs
// and anywhere else that draws them (/entities, on its own path because chat,
// tables and the calendar ask it too); who an "@" in a doc reaches; and
// to-dos that become tasks and stay in step with them.
func mountConnectedWork(rt docsRouters) {
	rt.Member.Post("/entities", entityLinkController.ReadEntities)
	rt.Doc.Post("/mentionAccess", entityLinkController.MentionAccess)
	rt.Doc.Post("/todoTask", entityLinkController.TodoToTask)
	rt.Doc.Post("/todoTask/check", entityLinkController.CheckTodo)
}
