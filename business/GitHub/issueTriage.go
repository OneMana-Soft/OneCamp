package business

import (
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
)

// Seam for tests: triage taking in an issue's task just made.
var arriveInTriage = triageBusiness.Arrive

// issueOpened is the status a new issue's task starts in, and whether it
// waits in its project's triage (linear-21 H1): it does where the link sets
// no status for new issues; a rule that does says where the team wants it,
// so the task is the team's work at once. Pure.
func issueOpened(link *githubLinkModel.GitHubLink) (status string, waits bool) {
	if s := ruleTarget(parseAutomationRules(link), "issue_opened"); s != "" {
		return s, false
	}
	return dgraphStruct.TASK_STATUS_TODO, true
}

// issueRequester is who asked, as triage keeps an issue's opener: by their
// GitHub login, nobody the workspace knows. Pure.
func issueRequester(login string) triageBusiness.Requester {
	if login == "" {
		return triageBusiness.Requester{}
	}
	return triageBusiness.Requester{Name: "@" + login + " on GitHub"}
}
