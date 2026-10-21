package router

import conversationController "github.com/akashc777/OneCamp/controllers/Conversation"

// mountPins is pinned messages' routes (chat B25): a conversation's pins, and
// pinning or unpinning one of its messages. {kind} is channel, dm or group,
// and {id} the id the app names it by (a DM's is both people's ids with a
// space, escaped).
func mountPins(rt chatRouters) {
	rt.Member.Get("/conversations/{kind}/{id}/pins", conversationController.GetPins)
	rt.Member.Post("/conversations/{kind}/{id}/pins", conversationController.SetPin)
}
