package router

import (
	aiController "github.com/akashc777/OneCamp/controllers/AI"
)

// B9, doc AI (docs programme, PROGRAM.md §7 B9). AI edition only: the port to
// without-ai drops this file and its line in mountDocsProgramme.
//
// The AI bar's actions themselves stay where they were, on router.go's /ai
// router (/ai/doc/complete and /ai/doc/complete/stream, now with the whole
// action set and the model named first). What the doc adds around them is
// here, under /doc/ai:
//   - the model the bar names before anything is sent;
//   - what an empty doc's AI row offers;
//   - accepted AI edits recorded, and "AI activity on this doc";
//   - a doc's action items proposed as tasks, and made;
//   - an agent's edits listed for the margin chips, and reverted.
func mountDocAI(rt docsRouters) {
	rt.Doc.Get("/ai/model", aiController.DocAIModel)
	rt.Doc.Get("/ai/starters", aiController.DocAIStarters)
	rt.Doc.Post("/ai/applied", aiController.RecordDocAIApplied)
	rt.Doc.Get("/ai/activity", aiController.DocAIActivity)
	rt.Doc.Post("/ai/tasks", aiController.FindDocActionItems)
	rt.Doc.Post("/ai/tasks/create", aiController.CreateDocActionItems)
	rt.Doc.Get("/ai/agent-edits", aiController.ListDocAgentEdits)
	rt.Doc.Post("/ai/agent-edits/revert", aiController.RevertDocAgentEdits)
}
