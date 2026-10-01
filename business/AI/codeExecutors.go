package business

// codeExecutors.go — read-only code-understanding tools for the agent runner
// and assistant, over the workspace's admin-connected GitHub repo. They reuse
// the existing CodeAgent (issue analysis -> proposed diff) and the GitHub read
// helpers, adding NO new egress path: everything goes through the same
// org-level GitHub integration and AI service chokepoint.
//
// These tools never write. Writing code (committing, opening a PR) is
// deliberately NOT a built-in tool: it should be routed through an
// admin-registered GitHub/coding MCP server so it stays vendor-neutral and
// behind the same confirmation gate as any other write. That keeps the
// "analyze and propose" half fast and safe, and the "apply" half governed.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	codeagent "github.com/akashc777/OneCamp/business/CodeAgent"
	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// RegisterCodeExecutors wires the read-only code tools. Called from
// RegisterToolExecutors alongside the other core tools.
func RegisterCodeExecutors() {
	ai.RegisterExecutor("code_analyze", executeCodeAnalyze)
	ai.RegisterExecutor("repo_summary", executeRepoSummary)
	ai.RegisterExecutor("list_recent_changes", executeListRecentChanges)
	ai.RegisterExecutor("list_commits", executeListCommits)
	ai.RegisterExecutor("read_repo_file", executeReadRepoFile)
	ai.RegisterExecutor("search_repo_code", executeSearchRepoCode)

	// Turn on org-context fusion for the read-only code agent: ground analyses
	// in the workspace's remembered decisions, conventions, and the standing
	// instructions given to the agent in the originating conversation. The
	// provider is permission-scoped, OFF-safe, and bounded (see
	// codeContextProvider), so this is a no-op unless a workspace has the memory
	// layer enabled and relevant context to add.
	codeagent.RegisterOrgContextProvider(codeContextProvider{})
}

// executeReadRepoFile reads a single file from the connected GitHub repo at an
// optional ref (branch/tag/sha; default branch when empty). Read-only grounding
// so a coding agent works from the ACTUAL file before proposing a change (which
// it applies via an MCP write tool). Content is bounded server-side (64KB) so a
// huge file can't blow the model's context budget.
func executeReadRepoFile(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	owner := strings.TrimSpace(action.Params["owner"])
	repo := strings.TrimSpace(action.Params["repo"])
	path := strings.TrimSpace(action.Params["path"])
	ref := strings.TrimSpace(action.Params["ref"])
	if owner == "" || repo == "" || path == "" {
		return "", nil, fmt.Errorf("owner, repo and path are required")
	}

	content, err := githubBusiness.FetchFileContent(ctx, owner, repo, path, ref)
	if err != nil {
		if isGitHubUnavailable(err) {
			return githubNotConnectedMsg, nil, nil
		}
		// A missing file / non-file / binary blob is a benign, actionable
		// result the agent should relay, not a hard run failure.
		return fmt.Sprintf("Couldn't read %s from %s/%s: %s", path, owner, repo, err.Error()), nil, nil
	}

	refLabel := ref
	if refLabel == "" {
		refLabel = "the default branch"
	}
	return fmt.Sprintf("%s/%s:%s (@ %s)\n\"\"\"\n%s\n\"\"\"", owner, repo, path, refLabel, content), nil, nil
}

// executeSearchRepoCode runs GitHub code search scoped to the connected repo and
// returns the matching file paths, so a coding agent can locate where a symbol,
// string, or pattern lives before reading it. Read-only; best-effort (GitHub
// code search only indexes the default branch and is rate-limited).
func executeSearchRepoCode(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	owner := strings.TrimSpace(action.Params["owner"])
	repo := strings.TrimSpace(action.Params["repo"])
	query := strings.TrimSpace(action.Params["query"])
	if owner == "" || repo == "" || query == "" {
		return "", nil, fmt.Errorf("owner, repo and query are required")
	}

	hits, err := githubBusiness.SearchCode(ctx, owner, repo, query, parseIntDefault(action.Params["limit"], 10))
	if err != nil {
		if isGitHubUnavailable(err) {
			return githubNotConnectedMsg, nil, nil
		}
		return "", nil, fmt.Errorf("could not search the repository right now")
	}
	if len(hits) == 0 {
		return fmt.Sprintf("No code matching %q found in %s/%s (note: GitHub code search indexes only the default branch).", query, owner, repo), nil, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Files in %s/%s matching %q (%d):\n", owner, repo, query, len(hits)))
	for _, h := range hits {
		sb.WriteString("• " + h.Path + "\n")
	}
	sb.WriteString("\nUse read_repo_file to read any of these before proposing a change.")
	return sb.String(), nil, nil
}

// githubNotConnectedMsg is returned (as a result, not an error) when the
// workspace has no GitHub integration, so the agent relays an actionable note
// instead of failing the run.
const githubNotConnectedMsg = "The workspace's GitHub integration isn't connected (or lacks access to that repo). An admin can connect it under Settings, then try again."

func executeCodeAnalyze(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	owner := strings.TrimSpace(action.Params["owner"])
	repo := strings.TrimSpace(action.Params["repo"])
	title := strings.TrimSpace(action.Params["title"])
	body := action.Params["body"]
	ref := strings.TrimSpace(action.Params["ref"])
	if owner == "" || repo == "" || title == "" {
		return "", nil, fmt.Errorf("owner, repo and title are required")
	}

	// Ground the analysis in the org's context, scoped to the acting user's
	// permissions (OFF-safe: no-op unless a context provider is registered).
	// When the run is inside a channel, thread that scope too so the provider
	// can fuse the originating discussion — the "why" a repo-only agent can't
	// see.
	actx := codeagent.WithActor(ctx, userUUID)
	if chID := ai.ChannelBudgetID(ctx); chID != "" {
		actx = codeagent.WithConversation(actx, chID, "")
	}
	analysis, err := codeagent.AnalyzeIssue(actx, owner, repo, title, body, ref, false)
	if err != nil {
		if isGitHubUnavailable(err) {
			return githubNotConnectedMsg, nil, nil
		}
		return "", nil, fmt.Errorf("code analysis could not be completed right now")
	}

	var sb strings.Builder
	sb.WriteString(analysis.Answer)
	if len(analysis.FilesConsidered) > 0 {
		sb.WriteString("\n\nFiles considered: ")
		sb.WriteString(strings.Join(analysis.FilesConsidered, ", "))
	}
	if analysis.Partial {
		sb.WriteString("\n\n(Note: only part of the repo could be retrieved, so this analysis may be incomplete.)")
	}
	sb.WriteString("\n\nThis is a proposed fix for a human to review and apply — I have not changed any code.")
	return sb.String(), nil, nil
}

func executeRepoSummary(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	owner := strings.TrimSpace(action.Params["owner"])
	repo := strings.TrimSpace(action.Params["repo"])
	if owner == "" || repo == "" {
		return "", nil, fmt.Errorf("owner and repo are required")
	}

	branch, berr := githubBusiness.GetDefaultBranch(ctx, owner, repo)
	if berr != nil {
		if isGitHubUnavailable(berr) {
			return githubNotConnectedMsg, nil, nil
		}
		return "", nil, fmt.Errorf("could not read the repository right now")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Repository %s/%s (default branch: %s)\n", owner, repo, branch))

	if paths, truncated, terr := githubBusiness.ListRepoTree(ctx, owner, repo, branch, 4000); terr == nil && len(paths) > 0 {
		sb.WriteString("\nTop-level structure:\n")
		for _, e := range topLevelEntries(paths, 25) {
			sb.WriteString("• " + e + "\n")
		}
		if truncated {
			sb.WriteString("(repository tree is large; only part was read)\n")
		}
	}

	if prs, perr := githubBusiness.ListMergedPullRequests(ctx, owner, repo, 14, 10); perr == nil && len(prs) > 0 {
		sb.WriteString(fmt.Sprintf("\nRecently merged (last 14 days, %d):\n", len(prs)))
		for _, p := range prs {
			sb.WriteString(fmt.Sprintf("• #%d %s\n", p.Number, strings.TrimSpace(p.Title)))
		}
	}
	return sb.String(), nil, nil
}

func executeListRecentChanges(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	owner := strings.TrimSpace(action.Params["owner"])
	repo := strings.TrimSpace(action.Params["repo"])
	days := parseIntDefault(action.Params["days"], 14)
	if owner == "" || repo == "" {
		return "", nil, fmt.Errorf("owner and repo are required")
	}

	prs, err := githubBusiness.ListMergedPullRequests(ctx, owner, repo, days, 30)
	if err != nil {
		if isGitHubUnavailable(err) {
			return githubNotConnectedMsg, nil, nil
		}
		return "", nil, fmt.Errorf("could not read recent changes right now")
	}
	if len(prs) == 0 {
		return fmt.Sprintf("No pull requests were merged into %s/%s in the last %d day(s).", owner, repo, days), nil, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Merged into %s/%s in the last %d day(s) (%d):\n", owner, repo, days, len(prs)))
	for _, p := range prs {
		line := fmt.Sprintf("• #%d %s — %s", p.Number, strings.TrimSpace(p.Title), strings.TrimSpace(p.Author))
		if len(p.Labels) > 0 {
			line += " [" + strings.Join(p.Labels, ", ") + "]"
		}
		sb.WriteString(line + "\n")
	}
	return sb.String(), nil, nil
}

// topLevelEntries reduces a flat list of repo blob paths to a deduped, sorted
func executeListCommits(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	owner := strings.TrimSpace(action.Params["owner"])
	repo := strings.TrimSpace(action.Params["repo"])
	branch := strings.TrimSpace(action.Params["branch"])
	days := parseIntDefault(action.Params["days"], 0) // 0 = no time bound (latest commits)
	if owner == "" || repo == "" {
		return "", nil, fmt.Errorf("owner and repo are required")
	}

	commits, err := githubBusiness.ListRecentCommits(ctx, owner, repo, branch, days, 30)
	if err != nil {
		if isGitHubUnavailable(err) {
			return githubNotConnectedMsg, nil, nil
		}
		return "", nil, fmt.Errorf("could not read commits right now")
	}
	branchLabel := branch
	if branchLabel == "" {
		branchLabel = "the default branch"
	}
	scope := ""
	if days > 0 {
		scope = fmt.Sprintf(" in the last %d day(s)", days)
	}
	if len(commits) == 0 {
		return fmt.Sprintf("No commits were found on %s of %s/%s%s.", branchLabel, owner, repo, scope), nil, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Commits on %s of %s/%s%s (%d):\n", branchLabel, owner, repo, scope, len(commits)))
	for _, c := range commits {
		short := c.SHA
		if len(short) > 7 {
			short = short[:7]
		}
		line := fmt.Sprintf("• %s %s", short, c.Message)
		if c.Author != "" {
			line += " — " + c.Author
		}
		if c.Date != "" {
			line += " (" + c.Date + ")"
		}
		sb.WriteString(line + "\n")
	}
	return sb.String(), nil, nil
}

// topLevelEntries reduces a flat list of repo blob paths to a deduped, sorted
// set of top-level entries (a root file, or a top directory shown as "dir/"),
// capped at max. Gives a quick orientation without dumping the whole tree.
func topLevelEntries(paths []string, max int) []string {
	seen := make(map[string]bool)
	var out []string
	for _, p := range paths {
		p = strings.TrimPrefix(p, "/")
		if p == "" {
			continue
		}
		entry := p
		if i := strings.Index(p, "/"); i >= 0 {
			entry = p[:i] + "/"
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
	}
	sort.Strings(out)
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// isGitHubUnavailable reports whether an error reflects the workspace GitHub
// integration being absent/unauthorized, so the tool can return a friendly,
// actionable note rather than a hard error.
func isGitHubUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "integration") ||
		strings.Contains(msg, "not connected") ||
		strings.Contains(msg, "no github") ||
		strings.Contains(msg, "401") ||
		strings.Contains(msg, "403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden")
}
