package router

import conversationController "github.com/akashc777/OneCamp/controllers/Conversation"

// mountConversations is the conversation service's routes (chat A9): adding
// people to a DM or group (the group of the new set, with the history they
// get), a group's name, hiding one, making one a private channel, and a
// conversation's people. {kind} is channel, dm or group, and {id} the id the
// app names it by (a DM's is both people's ids with a space, escaped).
//
// POST /groupChat/addParticipant is answered here too, over the same service,
// for apps already loaded; router.go no longer registers it.
func mountConversations(rt chatRouters) {
	rt.Member.Get("/conversations/{kind}/{id}/share-choices", conversationController.GetShareChoices)
	rt.Member.Post("/conversations/{kind}/{id}/people", conversationController.AddPeople)
	rt.Member.Post("/conversations/{kind}/{id}/hidden", conversationController.SetHidden)
	rt.Member.Post("/conversations/{kind}/{id}/leave", conversationController.Leave)
	rt.Member.Post("/conversations/{kind}/{id}/name", conversationController.Rename)
	rt.Member.Post("/conversations/{kind}/{id}/private-channel", conversationController.MakePrivateChannel)
	rt.Member.Get("/conversations/{kind}/{id}/members", conversationController.GetMembers)
	rt.Group.Post("/addParticipant", conversationController.AddParticipantToGroupChat)
}
