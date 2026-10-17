package router

import (
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
)

// B9, doc AI, on the AI-free edition: the same routes as the AI edition's
// docsAI.go, each answering 501 as every AI route on this build does, so a
// client learns the feature isn't here rather than that the route doesn't
// exist. The AI edition's file mounts the real handlers (controllers/AI),
// which this build doesn't have.
func mountDocAI(rt docsRouters) {
	notHere := func(w http.ResponseWriter, _ *http.Request) {
		helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
	}
	rt.Doc.Get("/ai/model", notHere)
	rt.Doc.Get("/ai/starters", notHere)
	rt.Doc.Post("/ai/applied", notHere)
	rt.Doc.Get("/ai/activity", notHere)
	rt.Doc.Post("/ai/tasks", notHere)
	rt.Doc.Post("/ai/tasks/create", notHere)
	rt.Doc.Get("/ai/agent-edits", notHere)
	rt.Doc.Post("/ai/agent-edits/revert", notHere)
}
