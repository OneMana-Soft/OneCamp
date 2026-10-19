package router

import threadController "github.com/akashc777/OneCamp/controllers/Thread"

// mountThreads mounts anchored threads (docs programme A8; business/Thread):
// members only, on the programme's member router (CSRF, VerifyAuth and an
// 8 MiB body cap). Every permission is the threads service's, through the
// Access Resolver.
func mountThreads(rt docsRouters) {
	rt.Member.Get("/threads", threadController.ListThreads)
	rt.Member.Post("/threads", threadController.CreateThread)
	rt.Member.Get("/threads/{thread_id}", threadController.GetThread)
	rt.Member.Post("/threads/{thread_id}/replies", threadController.ReplyToThread)
	rt.Member.Post("/threads/{thread_id}/resolve", threadController.ResolveThread)
	rt.Member.Post("/threads/{thread_id}/reopen", threadController.ReopenThread)
	rt.Member.Post("/threads/{thread_id}/reanchor", threadController.ReanchorThread)
	rt.Member.Post("/threads/{thread_id}/read", threadController.MarkThreadRead)
	// A comment threads keep (a table's row's): edit, remove, react. A doc's
	// comments change through the doc's own comment routes.
	rt.Member.Post("/threads/{thread_id}/comments/{comment_id}/edit", threadController.EditThreadComment)
	rt.Member.Post("/threads/{thread_id}/comments/{comment_id}/remove", threadController.RemoveThreadComment)
	rt.Member.Post("/threads/{thread_id}/comments/{comment_id}/react", threadController.ReactToThreadComment)
	rt.Member.Post("/threads/{thread_id}/comments/{comment_id}/unreact", threadController.UnreactToThreadComment)
}
