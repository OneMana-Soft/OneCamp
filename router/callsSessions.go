package router

import (
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
)

// mountCallSessions mounts CALLS-1's routes: a call as its people read it, and
// what the people running it do to it.
func mountCallSessions(rt callsRouters) {
	rt.Member.Get("/calls/current", callsController.CurrentCall)
	rt.Member.Get("/calls/{session_id}", callsController.GetCall)
	rt.Member.Post("/calls/{session_id}/act", callsController.ActInCall)
}
