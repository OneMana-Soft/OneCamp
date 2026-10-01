package business

// Route GitHub code changes through the governed code_pr path.
//
// Symptom this fixes: an agent asked to "open a PR / change the README" reaches
// for the connected GitHub MCP's raw Contents-API tools (create_or_update_file /
// push_files) and hand-rolls a commit. That path forces the MODEL to carry
// GitHub's per-file blob `sha` across turns; a stale/absent sha makes GitHub
// reject the write, and a weak model then asks the human "what's the SHA?" and
// stalls — a PR never gets opened. Meanwhile the workspace already has the
// governed code_pr orchestrator (clone → edit → verify → open PR, no manual sha)
// which was built for exactly this.
//
// So when code_pr is enabled we (a) make sure the agent can actually reach it
// and (b) refuse the raw GitHub file-content writes with a redirect to code_pr.
// Reads (list_commits, get_file_contents), comments, issues and labels are
// untouched — only the file-content writes that construct a commit/PR by hand.

import "strings"

// codePRToolName is the registry name of the governed code-PR tool (mirrors the
// ai package's tool registration).
const codePRToolName = "code_pr"

// gitHubContentWriteVerbs are the normalized names of raw GitHub file-content
// write tools (GitHub MCP / GitHub Copilot MCP). Matched as normalized
// substrings so they hit across naming styles and server prefixes
// ("create_or_update_file", "create-or-update-file", "github_push_files", …).
var gitHubContentWriteVerbs = []string{
	"createorupdatefile",
	"pushfiles",
	"updatefile",
	"deletefile",
}

// normalizeToolName lowercases a tool name and strips separators so matching is
// naming-style- and prefix-agnostic.
func normalizeToolName(s string) string {
	return strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(strings.ToLower(s))
}

// isGitHubContentWriteTool reports whether a tool name is a raw GitHub
// file-content write (constructs a commit/PR by hand). These are the calls
// code_pr should own when the feature is enabled.
func isGitHubContentWriteTool(name string) bool {
	n := normalizeToolName(name)
	for _, v := range gitHubContentWriteVerbs {
		if strings.Contains(n, v) {
			return true
		}
	}
	return false
}

// hasGitHubContentWriteTool reports whether the agent's allow-list contains any
// raw GitHub file-content write tool (so we know to offer code_pr as the safe
// alternative and to redirect those calls).
func hasGitHubContentWriteTool(allow map[string]bool) bool {
	for name := range allow {
		if isGitHubContentWriteTool(name) {
			return true
		}
	}
	return false
}
