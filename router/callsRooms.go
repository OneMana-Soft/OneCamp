package router

import (
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
)

// mountCallRooms mounts CALLS-2's member routes for a personal meeting room:
// the member's own room, opened, changed and closed by them alone. Guests come
// in at its address, on the public room routes in router.go.
func mountCallRooms(rt callsRouters) {
	rt.Member.Get("/calls/room", callsController.GetMyRoom)
	rt.Member.Post("/calls/room", callsController.OpenMyRoom)
	rt.Member.Patch("/calls/room", callsController.ChangeMyRoom)
	rt.Member.Delete("/calls/room", callsController.CloseMyRoom)
}
