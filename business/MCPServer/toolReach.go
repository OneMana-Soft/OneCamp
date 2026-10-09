package business

// What an in-app tool call touches, for deciding whether a person may have an
// agent make it for them.
//
// The governed MCP surface asks one question of every call: may the human
// behind it act on this object? An agent INSIDE the workspace now asks the
// same question. Its tools run as its sponsor, and when someone else asked for
// the run, a call is allowed only if that person could make it too (see
// business/AIAgent agentRequester.go). Same tools, same objects, same answer:
// so the declarations live here beside the bridge table, and the decision is
// PrincipalCanReach, unchanged. A second table of "what does this tool touch"
// in the agent package would be a second answer the first time one was edited.
//
// inAppTools covers the registry tools that are not published on the MCP
// surface — deliberately, for the reasons bridge.go gives — but that agents
// still run for people. Declaring one here publishes nothing; only bridgedTools
// is registered as an MCP tool.

import "fmt"

// inAppTools are the in-app tools with no MCP binding, declared the same way.
var inAppTools = []binding{
	// A call's transcript is read from the channel it happened in; seeing that
	// channel is what permits reading what was said there.
	{Tool: "read_meeting_transcript", Kind: ResourceChannel, IDArg: "channel_uuid",
		Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	// A poll is a post: the channel's posting rule decides, admins-only included.
	{Tool: "create_poll", Kind: ResourceChannel, IDArg: "channel_uuid",
		Behaviour: ToolBehaviour{ReadOnly: false}},
	// Adding to a doc is changing it: its editing list decides, not its visibility.
	{Tool: "append_to_doc", Kind: ResourceDoc, IDArg: "doc_uuid",
		Behaviour: ToolBehaviour{ReadOnly: false}},

	// Workspace-wide reads, the same for every member. The people directory is
	// visible to every member; web search reads no workspace data. The code
	// tools read GitHub through the workspace's integration, which is the token
	// of the admin who connected it (business/CodePR credential.go), for any
	// owner/repo a call names: not the sponsor's reach, so nothing of theirs is
	// lent, but not narrowed to the linked repositories either. Each still needs
	// the person to be a live member, which is what this kind checks. How every
	// workspace-wide tool is narrowed is pinned in business/AIAgent
	// (TestEveryWorkspaceWideToolSaysHowItIsNarrowed).
	{Tool: "find_people", Kind: ResourceWorkspace, Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "web_search", Kind: ResourceWorkspace, Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "code_analyze", Kind: ResourceWorkspace, Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "repo_summary", Kind: ResourceWorkspace, Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "list_recent_changes", Kind: ResourceWorkspace, Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "list_commits", Kind: ResourceWorkspace, Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "read_repo_file", Kind: ResourceWorkspace, Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
	{Tool: "search_repo_code", Kind: ResourceWorkspace, Behaviour: ToolBehaviour{ReadOnly: true, Idempotent: true}},
}

// ResourceForToolCall names the object an in-app tool call acts on and
// whether it reads or changes it, from the declaration that governs the tool
// (the MCP bridge's, or inAppTools). Access is derived from the declared
// behaviour, as AuthorizeToolCall derives it, never taken from the caller.
//
// known is false for a tool with no declaration: a caller deciding on someone
// else's behalf must refuse it rather than guess what it touches. An error is a
// call whose target cannot be read from its arguments, which is also a refusal.
func ResourceForToolCall(tool string, params map[string]string) (ref ResourceRef, known bool, err error) {
	b, ok := declaredBinding(tool)
	if !ok {
		return ResourceRef{}, false, nil
	}
	resolve, err := resourceResolver(b)
	if err != nil {
		return ResourceRef{}, true, err
	}
	args := make(map[string]any, len(params))
	for k, v := range params {
		args[k] = v
	}
	ref, err = resolve(args)
	if err != nil {
		return ResourceRef{}, true, fmt.Errorf("could not tell what %s would act on: %w", tool, err)
	}
	ref.Access = AccessWrite
	if b.Behaviour.ReadOnly {
		ref.Access = AccessRead
	}
	return ref, true, nil
}

// declaredBinding finds a tool's declaration in either table.
func declaredBinding(tool string) (binding, bool) {
	for _, table := range [][]binding{bridgedTools, inAppTools} {
		for _, b := range table {
			if b.Tool == tool {
				return b, true
			}
		}
	}
	return binding{}, false
}
