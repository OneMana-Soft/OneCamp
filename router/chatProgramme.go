package router

import (
	"github.com/go-chi/chi/v5"

	// The chat kinds of the Access Resolver (channel, dm, group, thread,
	// call) register from business/Conversation's init: imported here so the
	// server always has them, whichever chat package merges first.
	_ "github.com/akashc777/OneCamp/business/Conversation"
)

// The chat programme's routes (design/chat-vs-slack/PROGRAM.md §3.6 and §9),
// on the docs programme's pattern (docsProgramme.go): router.go is frozen and
// calls mountChatProgramme once, after every other route; each chat package
// adds ONE line to mountChatProgramme, written by the integrator at its merge,
// and keeps its own routes in router/chat<Area>.go as
// func mount<Area>(rt chatRouters).
//
// THE ROUTERS CARRY THEIR PROTECTION (publicSurface_test.go): chatRouters holds
// only routers behind the member group or /admin, never the root.
//
// A ROUTE SWAP (a package replacing a handler in controllers/Post, Chat or
// Channel) deletes the old line from router.go and registers the new handler
// on the same sub-router here: chi keeps the last handler registered for a
// method and pattern without a word.
type chatRouters struct {
	// Member is the root behind the member group's middleware, for an area
	// with a path of its own (/conversations, /read, /notify, /presence).
	Member chi.Router
	// Admin is /admin: workspace admins only.
	Admin chi.Router
	// Channel, Post, DM and Group are router.go's /ch, /po, /dm and /groupChat,
	// for route swaps and for routes that belong beside today's.
	Channel chi.Router
	Post    chi.Router
	DM      chi.Router
	Group   chi.Router
}

// newChatRouters builds the routers a chat package may mount on. root is the
// top-level router, used only to open the member group.
func newChatRouters(root, admin, channel, post, dm, group chi.Router) chatRouters {
	return chatRouters{
		Member: membersOnly(root), Admin: admin,
		Channel: channel, Post: post, DM: dm, Group: group,
	}
}

// mountChatProgramme mounts every merged package of the chat programme: one
// line per package, in merge order, and nothing else.
func mountChatProgramme(rt chatRouters) {
	mountConversations(rt) // A9: the conversation service and group conversations
}
