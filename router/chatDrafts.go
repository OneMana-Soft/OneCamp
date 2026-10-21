package router

import (
	conversationController "github.com/akashc777/OneCamp/controllers/Conversation"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// draftBodyLimit holds a draft's 100,000 characters (business/Draft MaxHTML),
// each up to 4 bytes, with its files and reply beside them.
const draftBodyLimit = 512 << 10

// mountDrafts is drafts kept on the server's routes (chat P5): a member's own
// drafts, every one, and keeping or emptying one. Members only, so a guest's
// link or a connected workspace never reaches anyone's drafts.
func mountDrafts(rt chatRouters) {
	rt.Member.Get("/drafts", conversationController.GetDrafts)
	rt.Member.With(customMiddleware.BodyLimit(draftBodyLimit)).Put("/drafts", conversationController.PutDraft)
	rt.Member.Delete("/drafts", conversationController.DeleteDraft)
}
