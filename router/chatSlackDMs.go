package router

import (
	slackImportController "github.com/akashc777/OneCamp/controllers/SlackImport"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// mountSlackDMs is each member bringing their own Slack DMs, group DMs and
// private channels into the team's import (chat item 2,
// business/SlackImport member_read.go). Member is a signed-in session's
// alone: no API token reaches it. Starting a read is refused the demo's
// shared visitor too, whose DMs every visitor after them would read.
func mountSlackDMs(rt chatRouters) {
	rt.Member.Get("/me/slack/dms", slackImportController.HandleMyReads)
	rt.Member.With(customMiddleware.NoPersonalAccountsInDemo).Post("/me/slack/dms", slackImportController.HandleMyRead)
	rt.Member.Get("/me/slack/dms/{jobId}/notes", slackImportController.HandleMyReadNotes)
	rt.Member.Post("/me/slack/dms/{jobId}/stop", slackImportController.HandleStopMyRead)
}
