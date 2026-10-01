package business

// MCP registry: turns admin-registered MCP servers into live entries in the
// shared AI tool registry. The full dynamic tool set is recomputed from all
// enabled servers' cached tool lists and swapped in atomically
// (ai.SetDynamicTools), so an agent can call an MCP tool exactly like a native
// one.
//
// Trust model: MCP tools call EXTERNAL systems, so they do not carry OneCamp's
// per-call permission re-check. That is acceptable because they are opt-in
// twice — only an admin (mcp.manage / agent.manage) can register a server, and
// the agent's owner must explicitly add the tool to the agent's allow-list.
// The agent's scope + step budget + circuit breaker still bound usage.
//
// Because a server is external, its tool metadata is untrusted: risk is resolved
// host-side by classifyToolRisk, where a provider annotation can only raise the
// risk (read -> write -> destructive), never downgrade it.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/AIMCP"
	ai "github.com/akashc777/OneCamp/services/AI"
)

var rebuildMu sync.Mutex // serializes registry rebuilds

// Start performs the initial registry build at server startup. Inert if no MCP
// servers are registered.
func Start(ctx context.Context) {
	if err := RebuildRegistry(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "AIMCP/Start initial rebuild err: %v", err)
	}
	// An admin's save, test-connection or delete rebuilds the registry IN THIS
	// PROCESS. Every other replica converges from the store, here. Introspection
	// bumps updated_at too, so a refreshed tool list reaches them as well.
	if err := helpers.StartConfigReconciler(ctx, helpers.ConfigReconciler{
		Name:     "mcp-registry",
		Interval: helpers.DefaultConfigReconcileInterval,
		Fingerprint: func(ctx context.Context) (string, error) {
			return postgresInit.RowsFingerprint(ctx, "ai_mcp_servers")
		},
		Apply: RebuildRegistry,
	}); err != nil {
		helpers.LogErrorWithContext(ctx, "AIMCP/Start reconciler: %v", err)
	}
	helpers.MessageLogs.InfoLog.Println("AI MCP registry started")
}

// RebuildRegistry recomputes the dynamic tool set from every enabled server's
// cached tools and swaps it into the AI registry atomically.
func RebuildRegistry(ctx context.Context) error {
	rebuildMu.Lock()
	defer rebuildMu.Unlock()

	servers, err := model.ListEnabledServers(ctx)
	if err != nil {
		return err
	}

	var defs []ai.ToolDef
	execs := map[string]ai.ToolExecutor{}

	// Collected here and published at the end, so an answer can say it was
	// produced without this connector instead of quietly being worse.
	var unreachable []ai.DegradedCapability
	skipped := 0
	for _, s := range servers {
		// A SERVER WHOSE SECRET WILL NOT DECRYPT CONTRIBUTES NO TOOLS.
		//
		// Its tools_cache is still populated from the last successful introspection, so without
		// this the registry advertises a full catalogue that every call then refuses — beta
		// logged "decrypt auth secret failed" and "registered 44 tool(s)" back to back. An agent
		// shown a tool will try it, spending a call and a budget to learn what the listing could
		// have told it. Same argument as admission control on the served MCP catalogue.
		if s.AuthSecretUnreadable {
			skipped++
			// Named WITH the topics its tools cover, taken from the catalogue
			// its last good introspection left behind. That cache is exactly
			// the record of what this connector would be answering questions
			// about, and it is the only thing left to say so once the
			// credential stops working.
			unreachable = append(unreachable, ai.DegradedCapability{
				Name:   s.Name,
				Topics: ai.TopicsForCapability(s.Name, cachedToolNames(s.ToolsCache)),
			})
			helpers.LogErrorWithContext(ctx,
				"AIMCP: server %s (%s) contributes no tools because its auth secret cannot be "+
					"decrypted; re-enter it in admin MCP settings", s.Id, s.Name)
			continue
		}
		var tools []McpTool
		if strings.TrimSpace(s.ToolsCache) != "" {
			if uerr := json.Unmarshal([]byte(s.ToolsCache), &tools); uerr != nil {
				helpers.LogErrorWithContext(ctx, "AIMCP: bad tools_cache for server %s: %v", s.Id, uerr)
				continue
			}
		}
		for _, t := range tools {
			fullName := s.ToolPrefix + t.Name
			params, types := schemaToParams(t.InputSchema)
			// Resolve risk HOST-SIDE (see classifyToolRisk): the server's
			// annotations may only raise the risk, never lower it, so a
			// dishonest readOnlyHint cannot buy auto-run or slip past the
			// protected-branch guard. The same resolved classification drives
			// the registry flags, the description, and the executor.
			risk := classifyToolRisk(t)
			defs = append(defs, ai.ToolDef{
				Name:        fullName,
				Description: mcpToolDescription(s.Name, t, risk.Destructive),
				Parameters:  params,
				ReadOnly:    risk.ReadOnly,
				Destructive: risk.Destructive,
			})
			execs[fullName] = makeExecutor(s, t.Name, types, risk)
		}
	}

	ai.SetDynamicTools(defs, execs)
	// Counts the servers that actually contributed, not the servers considered, and names the
	// shortfall. "registered 44 tool(s) from 1 server(s)" was true and misleading at the same
	// time when that one server was unusable.
	helpers.LogInfoWithContext(ctx, "AIMCP: registered %d tool(s) from %d of %d enabled server(s)"+
		unusableSuffix(skipped), len(defs), len(servers)-skipped, len(servers))
	setUnreachableServers(unreachable)

	return nil
}

// cachedToolNames reads the tool names out of a server's cached catalogue. A
// cache that will not parse yields none, which reads as "we cannot tell what
// this was for" and makes the connector relevant to everything rather than to
// nothing.
func cachedToolNames(cache string) []string {
	if strings.TrimSpace(cache) == "" {
		return nil
	}
	var tools []McpTool
	if err := json.Unmarshal([]byte(cache), &tools); err != nil {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if n := strings.TrimSpace(t.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// makeExecutor builds the ToolExecutor closure for one MCP tool. It coerces the
// agent's string params to the schema's JSON types where known, then calls the
// server. userUUID is unused (external call); the agent allow-list/scope gate
// access upstream.
func makeExecutor(server *model.McpServer, rawTool string, types map[string]string, risk toolRisk) ai.ToolExecutor {
	// Snapshot the connection details so a later edit/delete of the server row
	// doesn't mutate an in-flight call; the next rebuild installs fresh ones.
	srv := *server
	// A tool targets a branch if its schema declares any recognized
	// branch-like parameter (git hosts vary: branch, ref, ...).
	hasBranchParam := false
	for _, k := range branchParamNames {
		if _, ok := types[k]; ok {
			hasBranchParam = true
			break
		}
	}
	return func(ctx context.Context, action ai.ProposedAction, _ string) (string, map[string]string, error) {
		args := make(map[string]interface{}, len(action.Params))
		for k, v := range action.Params {
			args[k] = coerceArg(v, types[k])
		}
		// Governance guard: never let an agent commit directly to a protected
		// (default) branch through a GitHub write tool. An agent asked to "open
		// a PR" must go branch -> commit -> pull request, never push to main —
		// the same protected-branch guarantee the code_pr orchestrator enforces.
		// The runner feeds the refusal back so the model self-corrects into the
		// branch+PR flow. Opt out with AI_ALLOW_AGENT_DEFAULT_BRANCH_WRITE=true.
		if branch, blocked := protectedBranchWrite(&srv, rawTool, risk, hasBranchParam, action.Params); blocked {
			return "", nil, fmt.Errorf(
				"refusing to write directly to the protected branch %q: create a new branch (create_branch) and open a pull request (create_pull_request) instead of committing to the default branch",
				branch)
		}
		client, cerr := NewClient(&srv)
		if cerr != nil {
			// Refused before any request leaves the process: an unreadable secret would
			// otherwise be sent as an empty auth header to an external endpoint.
			return "", nil, cerr
		}
		out, err := client.CallTool(ctx, rawTool, args)
		if err != nil {
			return "", nil, err
		}
		return out, nil, nil
	}
}

// branchParamNames are the schema/argument keys a git-hosting write tool uses
// to name its commit-target branch. Kept ordered for deterministic resolution.
var branchParamNames = []string{"branch", "ref"}

// branchWriteExemptTools are non-read-only tools that carry a branch argument
// which is NOT a commit target, so the protected-branch guard must not fire on
// them: create_branch's branch is the NEW branch's name (creating one named
// "main" fails on the host's side anyway, not a content write to it).
var branchWriteExemptTools = map[string]bool{
	"create_branch": true,
}

// protectedBranchWrite reports whether a tool call would write to a protected
// (default) branch, and must be refused. It is intentionally GENERIC — no
// hardcoded tool list, and NOT limited to GitHub:
//
//   - fires only for a WRITE per the HOST-RESOLVED classification (toolRisk,
//     not the raw readOnlyHint) — so a server that lies about a mutating tool
//     being read-only cannot bypass this guard — while genuine reads
//     (browse/list/get) are never blocked;
//   - fires only when the tool actually declares a branch-like parameter (so a
//     write with no branch target, e.g. create_pull_request, is exempt);
//   - an explicitly-named protected branch (main/master or the configured set)
//     is refused on ANY server, so GitLab/Gitea/Bitbucket/etc. git MCPs are
//     covered exactly like GitHub;
//   - an empty/omitted branch resolves to the repo DEFAULT branch and is
//     refused, but only for a recognized git-hosting server (for a non-git MCP
//     an empty "branch" arg is not meaningfully a default branch);
//   - exempts branch-management tools whose branch arg is not a commit target
//     (create_branch).
//
// This covers push_files, create_or_update_file, delete_file, and any FUTURE
// content-write tool on any git host, without enumerating tool names. Returns a
// readable branch label for the refusal message.
func protectedBranchWrite(srv *model.McpServer, rawTool string, risk toolRisk, hasBranchParam bool, params map[string]string) (string, bool) {
	if truthyEnv("AI_ALLOW_AGENT_DEFAULT_BRANCH_WRITE") {
		return "", false
	}
	if risk.ReadOnly || !hasBranchParam {
		return "", false
	}
	if branchWriteExemptTools[strings.ToLower(strings.TrimSpace(rawTool))] {
		return "", false
	}
	branch := strings.TrimSpace(branchParamValue(params))
	if branch == "" {
		// Empty resolves to the repo default branch on a git host — the most
		// protected target. Only treat it as such for a git-hosting server.
		if isGitHostingServer(srv) {
			return "the default branch", true
		}
		return "", false
	}
	if protectedBranchSet()[strings.ToLower(branch)] {
		return branch, true
	}
	return "", false
}

// branchParamValue returns the first non-empty branch-like argument value.
func branchParamValue(params map[string]string) string {
	for _, k := range branchParamNames {
		if v := strings.TrimSpace(params[k]); v != "" {
			return v
		}
	}
	return ""
}

// protectedBranchSet is the set of branch names an agent may never write to
// directly. Configurable via AI_PROTECTED_BRANCHES (comma-separated); defaults
// to main + master. Lower-cased for case-insensitive matching.
func protectedBranchSet() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("AI_PROTECTED_BRANCHES"))
	if raw == "" {
		return map[string]bool{"main": true, "master": true}
	}
	set := map[string]bool{}
	for _, b := range strings.Split(raw, ",") {
		if b = strings.ToLower(strings.TrimSpace(b)); b != "" {
			set[b] = true
		}
	}
	if len(set) == 0 {
		return map[string]bool{"main": true, "master": true}
	}
	return set
}

// gitHostingMarkers are substrings (in an MCP server's name or URL) that
// identify a git-hosting connector, so the empty-branch = default-branch rule
// only applies where a repo default branch actually exists.
var gitHostingMarkers = []string{
	"github", "gitlab", "gitea", "bitbucket", "gogs", "codeberg", "sourcehut",
	"azure", "devops", // Azure DevOps Repos
}

// isGitHostingServer reports whether an MCP server is a git-hosting connector
// (by name or endpoint). Provider-agnostic: covers GitHub, GitLab, Gitea,
// Bitbucket, Azure DevOps, and similar.
func isGitHostingServer(srv *model.McpServer) bool {
	hay := strings.ToLower(srv.Name + " " + srv.URL)
	for _, m := range gitHostingMarkers {
		if strings.Contains(hay, m) {
			return true
		}
	}
	return false
}

// truthyEnv parses common truthy spellings of an env var.
func truthyEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// toolRisk is the HOST-resolved risk classification of one MCP tool. It is the
// single source of truth for auto-run vs approval and for the protected-branch
// guard; nothing downstream reads the server's annotations directly.
type toolRisk struct {
	// ReadOnly means the tool may auto-run inside the agent loop (its result is
	// fed back to the model without a human in the middle).
	ReadOnly bool
	// Destructive means the write is irreversible/high-risk, so it is never
	// auto-run unattended and is surfaced as such on the approval card.
	Destructive bool
}

// classifyToolRisk resolves an MCP tool's risk LOCALLY, treating the server's
// annotations as untrusted input that may only RAISE risk, never lower it. MCP
// servers are external; a malicious or buggy one that advertises delete_file
// with readOnlyHint=true must not thereby win auto-run or slip past the
// protected-branch guard. The rules, in order (each fails closed):
//
//  1. Destructive evidence wins: a server-declared destructiveHint OR a
//     destructive tool NAME makes the tool a destructive write, whatever the
//     readOnlyHint claims.
//  2. A provider-declared write stays a write: without readOnlyHint=true the
//     tool is a write even if the name looks read-shaped (the server knows
//     side effects the name doesn't show).
//  3. A mutating verb ANYWHERE in the name forces a write, overriding
//     readOnlyHint — so get_or_create / listAndUpdate never auto-run.
//  4. Only a credibly read-shaped name may auto-run. Unknown or ambiguous
//     names (do_thing, foo.bar) fail closed to the approval gate.
//
// Naming style is irrelevant: tokenizeToolName normalizes snake_case,
// kebab-case, camelCase and dot.notation to the same tokens.
func classifyToolRisk(t McpTool) toolRisk {
	readOnlyHint := t.Annotations != nil && t.Annotations.ReadOnlyHint
	destructiveHint := t.Annotations != nil && t.Annotations.DestructiveHint

	// (1) Destructive evidence always wins, hint or name.
	if destructiveHint || inferDestructiveByName(t.Name) {
		return toolRisk{ReadOnly: false, Destructive: true}
	}
	// (2) No read-only claim → write.
	if !readOnlyHint {
		return toolRisk{}
	}
	tokens := tokenizeToolName(t.Name)
	// (3) A mutating token contradicts the read-only claim → write.
	if hasMutatingToken(tokens) {
		return toolRisk{}
	}
	// (4) Fail closed unless the name is credibly read-shaped.
	if !readShapedName(tokens) {
		return toolRisk{}
	}
	return toolRisk{ReadOnly: true}
}

// mutatingVerbs are name tokens that denote a state change (non-destructive
// ones; the irreversible verbs live in destructiveVerbs and are checked too).
// Provider-agnostic and data-driven: any of these tokens anywhere in a tool
// name means "write", even when the server claims readOnlyHint.
var mutatingVerbs = map[string]bool{
	"create": true, "add": true, "insert": true, "upsert": true, "update": true,
	"edit": true, "modify": true, "patch": true, "set": true, "put": true,
	"post": true, "write": true, "save": true, "store": true, "replace": true,
	"push": true, "commit": true, "merge": true, "rebase": true, "revert": true,
	"reset": true, "rename": true, "move": true, "upload": true, "send": true,
	"publish": true, "deploy": true, "release": true, "apply": true,
	"execute": true, "exec": true, "run": true, "invoke": true, "trigger": true,
	"start": true, "stop": true, "restart": true, "cancel": true, "submit": true,
	"approve": true, "reject": true, "assign": true, "unassign": true,
	"lock": true, "unlock": true, "archive": true, "restore": true,
	"install": true, "enable": true, "disable": true, "subscribe": true,
	"unsubscribe": true, "invite": true, "grant": true, "transfer": true,
	"fork": true, "clone": true, "import": true, "register": true,
	"unregister": true, "schedule": true, "toggle": true, "close": true,
	"reopen": true, "comment": true, "react": true, "sync": true, "mutate": true,
}

// hasMutatingToken reports whether any token denotes a mutation (mutating or
// destructive verb). Env-extended destructive keywords count here too.
func hasMutatingToken(tokens []string) bool {
	destructive := destructiveVerbSet()
	for _, tk := range tokens {
		if mutatingVerbs[tk] || destructive[tk] {
			return true
		}
	}
	return false
}

// readVerbs are name tokens that make a tool credibly read-shaped: they only
// observe state. A name must contain at least one of them (and no mutating
// token) to be eligible for auto-run — namespaced styles like repos.list or
// files.get are covered because the token may appear anywhere.
var readVerbs = map[string]bool{
	"get": true, "list": true, "read": true, "fetch": true, "search": true,
	"find": true, "lookup": true, "query": true, "view": true, "browse": true,
	"show": true, "describe": true, "detail": true, "details": true,
	"info": true, "inspect": true, "count": true, "stat": true, "stats": true,
	"status": true, "diff": true, "log": true, "logs": true, "history": true,
	"retrieve": true, "preview": true, "check": true, "validate": true,
	"resolve": true, "summarize": true, "explain": true, "compare": true,
	"ping": true, "whoami": true, "me": true, "tree": true, "blame": true,
}

// readShapedName reports whether a tool name credibly describes a read. Names
// with no recognized read verb are AMBIGUOUS and fail closed to approval.
func readShapedName(tokens []string) bool {
	for _, tk := range tokens {
		if readVerbs[tk] {
			return true
		}
	}
	return false
}

// destructiveVerbs are tool-name tokens that denote an irreversible/high-risk
// mutation. Used both to raise a write to DESTRUCTIVE and (via
// hasMutatingToken) to deny auto-run, so governance does not depend on the
// server annotating honestly.
var destructiveVerbs = map[string]bool{
	"delete": true, "destroy": true, "drop": true, "remove": true,
	"purge": true, "truncate": true, "wipe": true, "erase": true,
	"overwrite": true, "revoke": true, "terminate": true, "uninstall": true,
	"prune": true, "rmdir": true, "rm": true,
}

// destructiveVerbSet returns the destructive-verb set, extended with any
// deployment-specific keywords from AI_DESTRUCTIVE_TOOL_KEYWORDS (comma-sep),
// so operators can cover custom MCP tools without a code change.
func destructiveVerbSet() map[string]bool {
	extra := strings.TrimSpace(os.Getenv("AI_DESTRUCTIVE_TOOL_KEYWORDS"))
	if extra == "" {
		return destructiveVerbs
	}
	set := make(map[string]bool, len(destructiveVerbs)+4)
	for k := range destructiveVerbs {
		set[k] = true
	}
	for _, kw := range strings.Split(extra, ",") {
		if kw = strings.ToLower(strings.TrimSpace(kw)); kw != "" {
			set[kw] = true
		}
	}
	return set
}

// inferDestructiveByName reports whether a tool's NAME implies an irreversible
// mutation. It is evaluated for EVERY tool (never gated on the server's
// readOnlyHint), so a server that marks delete_file read-only is still treated
// as destructive. Provider-agnostic: it tokenizes the name (camelCase +
// snake/kebab/dot/space separators) and matches whole-token destructive verbs
// (so "get_deleted_items" — token "deleted", not "delete" — is NOT flagged),
// plus a force-push/overwrite combination (force + push/write/reset).
func inferDestructiveByName(tool string) bool {
	tokens := tokenizeToolName(tool)
	set := destructiveVerbSet()
	forced := false
	for _, tk := range tokens {
		if set[tk] {
			return true
		}
		if tk == "force" {
			forced = true
		}
	}
	if forced {
		for _, tk := range tokens {
			if tk == "push" || tk == "write" || tk == "reset" {
				return true
			}
		}
	}
	return false
}

// tokenizeToolName lowercases and splits a tool name into word tokens on
// non-alphanumeric separators AND camelCase boundaries, so deleteFile,
// delete_file, delete-file and delete.file all yield ["delete","file"].
func tokenizeToolName(name string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	runes := []rune(name)
	for i, r := range runes {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r):
			if i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])) {
				flush()
			}
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}

// mcpToolDescription prefixes the tool's own description with its source server
// so the model knows where the capability comes from.
func mcpToolDescription(serverName string, t McpTool, destructive bool) string {
	desc := strings.TrimSpace(t.Description)
	if desc == "" {
		desc = "External tool provided via MCP."
	}
	out := fmt.Sprintf("[%s] %s", serverName, desc)
	// Surface the resolved risk (server hint OR inferred) so the model reasons
	// about it (e.g. prefers a branch+PR over a destructive default-branch write).
	if destructive {
		out += " (destructive: irreversible change — prefer a reversible, reviewable path such as a new branch and pull request)"
	}
	return out
}

// jsonSchema is the subset of JSON Schema we read from an MCP tool's
// inputSchema to derive params + types.
type jsonSchema struct {
	Type       string                `json:"type"`
	Properties map[string]schemaProp `json:"properties"`
	Required   []string              `json:"required"`
}

type schemaProp struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

// schemaToParams converts an MCP tool inputSchema into ToolParam definitions and
// a name→jsonType map (used for argument coercion at call time). Unknown/empty
// schemas yield no params (the tool is callable with no arguments).
func schemaToParams(raw json.RawMessage) ([]ai.ToolParam, map[string]string) {
	types := map[string]string{}
	if len(raw) == 0 {
		return nil, types
	}
	var sch jsonSchema
	if err := json.Unmarshal(raw, &sch); err != nil {
		return nil, types
	}
	required := map[string]bool{}
	for _, r := range sch.Required {
		required[r] = true
	}
	var params []ai.ToolParam
	for name, p := range sch.Properties {
		jsonType := strings.ToLower(strings.TrimSpace(p.Type))
		if jsonType == "" {
			jsonType = "string"
		}
		types[name] = jsonType
		params = append(params, ai.ToolParam{
			Name:        name,
			Type:        paramTypeLabel(jsonType),
			Required:    required[name],
			Description: strings.TrimSpace(p.Description),
		})
	}
	return params, types
}

// paramTypeLabel maps a JSON Schema type to the registry's coarse type label.
func paramTypeLabel(jsonType string) string {
	switch jsonType {
	case "boolean":
		return "boolean"
	case "integer", "number":
		return "number"
	default:
		return "string"
	}
}

// coerceArg converts an agent-emitted string value to the JSON type the tool's
// schema expects, falling back to the raw string when coercion fails (the
// server then validates).
//
// Nested (array/object) arguments are the important case: model params cross
// the tool boundary as strings (map[string]string) and the provider schema
// models every field as a string, so a structured argument — e.g. github
// push_files' `files: [{path, content}]` — arrives here as a JSON STRING. Left
// unparsed it would be sent to the server as a quoted string and rejected
// ("provide a valid array of objects…"). We parse it back into real JSON so the
// server receives a proper array/object; on invalid JSON we fall back to the
// raw string and let the server validate (the model then corrects on retry).
func coerceArg(val, jsonType string) interface{} {
	switch jsonType {
	case "boolean":
		if b, err := strconv.ParseBool(strings.TrimSpace(val)); err == nil {
			return b
		}
	case "integer":
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			return n
		}
	case "number":
		if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
			return f
		}
	case "array", "object":
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return val
		}
		var parsed interface{}
		if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
			return parsed
		}
	}
	return val
}

// unusableSuffix names how many enabled servers were skipped, or nothing when none were.
//
// A suffix rather than a second log line: the count belongs with the total it qualifies, and a
// separate line can be read on its own and lose that.
func unusableSuffix(skipped int) string {
	if skipped == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d skipped: auth secret unreadable)", skipped)
}
