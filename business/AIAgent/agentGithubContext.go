package business

// GitHub repo grounding for agent runs.
//
// Symptom this fixes: an agent asked to "check the onecamp-fe repo" calls a
// connected GitHub MCP server's search_repositories with the BARE name
// "onecamp-fe". GitHub's search is GLOBAL, so it returns many repos with that
// name across all of GitHub — the agent can't tell which is the workspace's, so
// it either errors on a repo-qualified call (search_commits needs owner/repo)
// or stops to ask "which repository?". Connecting the MCP gives the agent the
// TOOLS + a token, but NOT the knowledge of which repo/owner is yours.
//
// OneCamp already knows: the github_links table maps each project to its real
// repo (repo_owner/repo_name), set when an admin links a repository. We inject
// those exact owner/name pairs into the run context so the agent qualifies
// GitHub tool calls to "owner/name" directly — no global search, no ambiguity,
// no needless "which repo?" question. Fully generic across the built-in GitHub
// tools and any GitHub MCP server.

import (
	"context"
	"strings"

	"github.com/google/uuid"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	"github.com/akashc777/OneCamp/helpers"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
)

// maxGitHubContextRepos caps how many linked repos are listed so the grounding
// stays compact (a workspace with many links can't balloon the prompt / TPM).
const maxGitHubContextRepos = 20

// buildGitHubContext returns a system-prompt snippet listing the connected
// GitHub repositories this run may read, as exact owner/name pairs, so an
// agent resolves a repo by name deterministically instead of a global search
// or a "which repo?" question. Only a repository linked to a project the
// sponsor is in, and the asker too when someone else asked, is named
// (business/AI RepoReader, the code tools' own rule): a run is never told of
// repositories its tools refuse, or of what other projects link. Best-effort:
// returns "" when nothing is listed or a lookup fails, so a run is never
// blocked by it.
func buildGitHubContext(ctx context.Context, sponsorUUID string) string {
	links, err := githubBusiness.GetAllLinkedRepos(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentRunner: list linked github repos failed: %v", err)
		return ""
	}
	if len(links) == 0 {
		return ""
	}
	readsFor, err := aiBusiness.RepoReader(ctx, sponsorUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentRunner: who the linked github repos are read for: %v", err)
		return ""
	}
	return renderGitHubContext(readableRepos(links, readsFor))
}

// readableRepos are the linked repositories mayRead allows, given all the
// projects linking each, as "owner/name" spelled as in the first of its links
// given (GetAllLinkedRepos gives the newest first); names match in any case,
// as GitHub matches them. At most maxGitHubContextRepos.
func readableRepos(links []*githubLinkModel.GitHubLink, mayRead func(projects []uuid.UUID) bool) []string {
	var order []string
	spelled := map[string]string{}
	linking := map[string][]uuid.UUID{}
	for _, l := range links {
		if l == nil {
			continue
		}
		owner, name := strings.TrimSpace(l.RepoOwner), strings.TrimSpace(l.RepoName)
		if owner == "" || name == "" {
			continue
		}
		key := strings.ToLower(owner + "/" + name)
		if _, seen := spelled[key]; !seen {
			spelled[key] = owner + "/" + name
			order = append(order, key)
		}
		linking[key] = append(linking[key], l.ProjectId)
	}
	repos := make([]string, 0, len(order))
	for _, key := range order {
		if !mayRead(linking[key]) {
			continue
		}
		repos = append(repos, spelled[key])
		if len(repos) >= maxGitHubContextRepos {
			break
		}
	}
	return repos
}

// renderGitHubContext builds the system-prompt snippet from a resolved list of
// "owner/name" repos. Split out from buildGitHubContext so the prompt wording
// (the part that actually steers the model) is unit-testable without a DB.
func renderGitHubContext(repos []string) string {
	if len(repos) == 0 {
		return ""
	}
	// The rules below are deliberately explicit because the failure they prevent
	// is subtle and model-agnostic:
	//   1. PRIVATE repos: GitHub's search endpoints (search_repositories,
	//      search/commits, etc.) are PUBLIC-biased and silently omit private
	//      repositories, producing a false "repository not found". Reading a
	//      private repo works only via DIRECT, repo-qualified endpoints
	//      (get-repository / list-commits / list-pull-requests with an explicit
	//      owner + name). So we forbid using a search tool to LOCATE a repo the
	//      workspace already knows, and require addressing it directly.
	//   2. DISAMBIGUATION: if the user's phrasing could match more than one of
	//      the linked repos (or none of them clearly), the agent must ASK the
	//      user to choose via needs_human — listing the candidate owner/name
	//      options in the question — instead of guessing or silently searching.
	//   3. CONNECTOR vs MCP: when both a built-in GitHub tool and a connected
	//      GitHub MCP server can serve the request, either is acceptable, but if
	//      it is unclear which source the user wants, ask rather than assume.
	var b strings.Builder
	b.WriteString("\n\nConnected GitHub repositories in this workspace (these are the ONLY repositories you should operate on unless the user gives a full owner/name explicitly):\n- ")
	b.WriteString(strings.Join(repos, "\n- "))
	b.WriteString("\n\nGitHub rules (follow exactly):\n")
	b.WriteString("- Use the EXACT owner/name listed above. Some of these repositories are PRIVATE.\n")
	b.WriteString("- To read commits / recent activity (\"what was committed\", \"what changed today/this week\", \"latest commits\"), use the list_commits tool with the exact owner and name (and a branch if the user names one). It reads GitHub's DIRECT commits endpoint, works for private repos, and is date-accurate.\n")
	b.WriteString("- Do NOT use a repository or commit SEARCH tool (e.g. search_repositories, search_commits) to find these repos or their commits — GitHub search omits private repositories and lags a search index, so it will falsely report \"not found\" or \"no commits\". Always prefer the direct, repo-qualified tools (list_commits / repo_summary / list_recent_changes, or a repo-qualified get/list call).\n")
	b.WriteString("- When one listed repo clearly matches the name the user mentioned, use it and do NOT ask which repository.\n")
	b.WriteString("- If the user's request could match MORE THAN ONE listed repo, or matches NONE of them clearly, use needs_human to ask the user to choose — list the candidate owner/name options in your question. Do not guess.\n")
	b.WriteString("- If both a built-in GitHub tool and a connected GitHub MCP server can serve the request and it is unclear which the user wants, ask via needs_human rather than assuming.\n")
	b.WriteString("- If a direct repo-qualified call still fails with a not-found or permission error, the connected GitHub token likely lacks access (e.g. missing 'repo' scope for private repositories). Report that plainly via needs_human instead of falling back to a global search.\n")
	return b.String()
}
