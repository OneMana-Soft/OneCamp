package router

import (
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
)

// mountCallQuality mounts A5's Call Quality Telemetry for members: their own
// samples, why a call stuttered, and the install's days. A guest's samples
// come by POST /guest/calls/quality (router.go, beside /guest/calls/act).
func mountCallQuality(rt callsRouters) {
	rt.Member.Post("/calls/{session_id}/quality", callsController.RecordCallQuality)
	rt.Member.Get("/calls/{session_id}/quality", callsController.CallQuality)
	rt.Member.Get("/calls/quality/days", callsController.CallQualityDays)
}
