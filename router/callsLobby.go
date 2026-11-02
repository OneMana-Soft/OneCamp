package router

import (
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
)

// mountCallsLobby mounts the member routes a meeting's host runs it by: its
// lobby choice (CALLS-2) and its recording (meet-02, head D). The guest side
// (knocking, waiting) is public and lives in router.go.
func mountCallsLobby(rt callsRouters) {
	rt.Member.Post("/calls/meetings/{meeting_id}/lobby", callsController.SetMeetingLobby)
	rt.Member.Post("/calls/meetings/{meeting_id}/recording", callsController.SetMeetingRecording)
}
