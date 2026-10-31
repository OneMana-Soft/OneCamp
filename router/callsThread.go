package router

import (
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
)

// mountCallThread mounts CALLS-3b's call thread surface for members; a guest's
// is /guest/calls/thread (router.go, beside /guest/calls/act).
func mountCallThread(rt callsRouters) {
	rt.Member.Get("/calls/{session_id}/thread", callsController.CallThread)
	rt.Member.Post("/calls/{session_id}/thread", callsController.ReplyInCallThread)
}
