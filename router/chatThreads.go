package router

import conversationController "github.com/akashc777/OneCamp/controllers/Conversation"

// mountChatThreads is threads you follow's routes (chat P2): the Threads view's
// page and unread count, marking them all read, and following or reading one
// thread, named by its root message's uuid. Members only. /chat-threads, not
// /threads: that is anchored threads' (docsThreads.go), whose service refuses
// a chat thread's row (no Resource is registered for its kind).
func mountChatThreads(rt chatRouters) {
	rt.Member.Get("/chat-threads", conversationController.GetMyThreads)
	rt.Member.Get("/chat-threads/unread", conversationController.GetThreadsUnread)
	rt.Member.Post("/chat-threads/read-all", conversationController.PostThreadsReadAll)
	rt.Member.Get("/chat-threads/{root}/follow", conversationController.GetThreadFollow)
	rt.Member.Put("/chat-threads/{root}/follow", conversationController.PutThreadFollow)
	rt.Member.Put("/chat-threads/{root}/read", conversationController.PutThreadRead)
}
