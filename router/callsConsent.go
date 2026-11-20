package router

import (
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
)

// mountCallConsent mounts zoom-09's answer to a recorded call's notice for
// members: their own OK or Leave, written to the admin audit log. A guest's
// comes by POST /guest/calls/consent (router.go, beside /guest/calls/act).
func mountCallConsent(rt callsRouters) {
	rt.Member.Post("/calls/{session_id}/consent", callsController.RecordCallConsent)
}
