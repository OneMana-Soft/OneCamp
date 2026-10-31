package router

import (
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
)

// mountCallsLobby mounts CALLS-2's member route: a meeting's lobby choice.
// The guest side (knocking, waiting) is public and lives in router.go.
func mountCallsLobby(rt callsRouters) {
	rt.Member.Post("/calls/meetings/{meeting_id}/lobby", callsController.SetMeetingLobby)
}
