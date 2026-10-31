package router

import (
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
)

// mountCallInvites mounts HU13's member routes: an invite into a call going
// on, and the door a member comes in by as themselves (a meeting's link, a
// room's address, a call's invite). Guests come in at the same links, on the
// public meeting and room routes in router.go.
func mountCallInvites(rt callsRouters) {
	rt.Member.Post("/calls/invite", callsController.InviteToCall)
	rt.Member.Post("/calls/door", callsController.EnterByDoor)
}
