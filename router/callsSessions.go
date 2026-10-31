package router

import (
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// mountCallSessions mounts CALLS-1's routes: a call as its people read it, and
// what the people running it do to it.
func mountCallSessions(rt callsRouters) {
	rt.Member.Get("/calls/current", callsController.CurrentCall)
	rt.Member.Get("/calls/{session_id}", callsController.GetCall)
	rt.Member.Post("/calls/{session_id}/act", callsController.ActInCall)
	// Its polls and Q&A (calls package A): read, and acted on by whoever is in it.
	rt.Member.Get("/calls/{session_id}/activities", callsController.CallActivities)
	rt.Member.Post("/calls/{session_id}/activities", callsController.DoCallActivity)
	// A token for one of its breakout rooms (calls package C): a room's number
	// and a language tag, bounded as the guests' route is.
	rt.Member.With(customMiddleware.BodyLimit(1<<12)).Post("/calls/{session_id}/breakout-token", callsController.CallBreakoutToken)
}
