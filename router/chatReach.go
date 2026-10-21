package router

import conversationController "github.com/akashc777/OneCamp/controllers/Conversation"

// mountChatReach is mentions that reach's routes (chat P3, B5): a channel's
// broadcast rules for the composer and its "Allow @here and @channel"
// setting, and the "@" list's server part (mentionables). Members only; the handlers check the channel.
func mountChatReach(rt chatRouters) {
	rt.Member.Get("/conversations/{kind}/{id}/broadcasts", conversationController.GetBroadcastRules)
	rt.Member.Put("/conversations/{kind}/{id}/broadcasts", conversationController.PutBroadcastRules)
	rt.Member.Get("/conversations/{kind}/{id}/mentionables", conversationController.GetMentionables)
	// Stage 3: the asker's own keywords and VIPs.
	rt.Member.Get("/me/keywords", conversationController.GetMyKeywords)
	rt.Member.Put("/me/keywords", conversationController.PutMyKeywords)
	rt.Member.Get("/me/vips", conversationController.GetMyVIPs)
	rt.Member.Put("/me/vips", conversationController.PutMyVIPs)
}
