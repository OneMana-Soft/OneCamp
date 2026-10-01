package business

import (
	"os"
	"regexp"
	"strings"
	"testing"

	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

func agentActor(tools ...string) ActorIdentity {
	return ActorIdentity{
		Type:              ActorAgent,
		AgentID:           "agent-1",
		AgentName:         "Researcher",
		AgentEnabledTools: tools,
	}
}

// A tool the owner granted is allowed; anything else is refused. This is the whole
// point: the builder's restriction has to mean the same thing here as it does in-app.
func TestAgentToolScopeHonoursTheOwnersChoice(t *testing.T) {
	actor := agentActor("read_doc", "summarize_channel")

	if ok, reason := CheckAgentToolScope(actor, "read_doc"); !ok {
		t.Fatalf("a granted tool was refused: %s", reason)
	}
	ok, reason := CheckAgentToolScope(actor, "send_message")
	if ok {
		t.Fatal("send_message was allowed for an agent whose owner did not grant it")
	}
	if !strings.Contains(reason, "send_message") {
		t.Errorf("the refusal should name the tool asked for, got %q", reason)
	}
	// Must not enumerate the rest of the toolset — that hands a caller a map of the
	// identity's capabilities.
	for _, granted := range actor.AgentEnabledTools {
		if strings.Contains(reason, granted) {
			t.Errorf("the refusal leaks the agent's other tools (%q): %s", granted, reason)
		}
	}
}

// AN EMPTY LIST MEANS NO TOOLS, matching the in-app runner, where an agent with no
// enabled tools is a conversation-only agent. Reading it as "unrestricted" would invert
// the owner's intent in the case where it is most explicit, and would also be the unsafe
// reading of a malformed column.
func TestAgentWithNoToolsCannotAct(t *testing.T) {
	for name, actor := range map[string]ActorIdentity{
		"nil list":   agentActor(),
		"empty list": {Type: ActorAgent, AgentID: "a", AgentEnabledTools: []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			if ok, _ := CheckAgentToolScope(actor, "read_doc"); ok {
				t.Fatal("an agent with no enabled tools was allowed to act; an empty list " +
					"must mean no tools, never all tools")
			}
		})
	}
}

// An unbound integration credential has no agent toolset, so the check must pass it
// through — otherwise adding this rung would break every existing token.
func TestAPIClientIsUnaffectedByAgentToolScope(t *testing.T) {
	actor := ActorIdentity{Type: ActorAPIClient, TokenID: "tok-1"}
	if ok, _ := CheckAgentToolScope(actor, "send_message"); !ok {
		t.Fatal("an unbound credential was refused by the agent toolset check")
	}
}

// A blank tool name is a caller bug and must refuse rather than match an empty entry.
func TestAgentToolScopeRefusesABlankToolName(t *testing.T) {
	if ok, _ := CheckAgentToolScope(agentActor("read_doc"), "  "); ok {
		t.Fatal("a blank tool name was allowed")
	}
}

// There was an AgentScopedTools([]string) narrowing here, and a test asserting it agreed with
// CheckAgentToolScope. Both were removed: nothing called the function. The serving controller
// filters with CheckAgentToolScope inline over its own element type, so a []string helper never
// fitted it. TestAgentToolsetIsEnforcedOnEveryPath below is what actually holds the guarantee,
// and it checks the paths that run.

// THE RATCHET. The agent's declared toolset must be enforced everywhere a tool can be
// reached or discovered. Missing it anywhere means an owner's restriction holds on some
// paths and not others, which is indistinguishable from not having it.
func TestAgentToolsetIsEnforcedOnEveryPath(t *testing.T) {
	for _, target := range []struct {
		file string
		why  string
	}{
		{"authorize.go", "governed tool calls would ignore the agent's declared toolset"},
		{"ungoverned.go", "ungoverned tool calls would ignore the agent's declared toolset"},
		// protocol.go used to be listed here for its own ListTools catalogue. That catalogue was
		// removed — nothing served it, and it listed only governed tools, so it would have hidden
		// everything not yet migrated. There is now exactly one catalogue, on the endpoint below,
		// which is a better place for this ratchet to point: it is the one clients read.
		{"../../controllers/MCP/mcpServerController.go",
			"tools/list on the serving endpoint would advertise tools the agent may not use"},
	} {
		raw, err := os.ReadFile(target.file)
		if err != nil {
			t.Fatalf("read %s: %v", target.file, err)
		}
		if !strings.Contains(string(raw), "CheckAgentToolScope(") {
			t.Errorf("%s does not apply CheckAgentToolScope: %s", target.file, target.why)
		}
	}
}

// The binding must be settable at creation. api_tokens.agent_id existed, was read by
// Validate and enforced by ResolveActor, and was never written by anything — so the
// entire agent-identity mechanism was dormant: no kill switch to trip, no per-agent
// budget to meter, and every call audited as an anonymous api_client.
func TestTheAgentBindingCanActuallyBeWritten(t *testing.T) {
	raw, err := os.ReadFile("../../models/postgres/ApiToken/apiTokenModel.go")
	if err != nil {
		t.Fatalf("read apiTokenModel.go: %v", err)
	}
	src := string(raw)

	insert := strings.Index(src, "INSERT INTO api_tokens")
	if insert < 0 {
		t.Fatal("could not find the api_tokens insert; this ratchet has gone stale")
	}
	stmt := src[insert:min(insert+400, len(src))]
	if !strings.Contains(stmt, "agent_id") {
		t.Error("the api_tokens insert does not write agent_id, so no credential can ever " +
			"be bound to an agent identity: the kill switch, the per-agent token cap and " +
			"agent attribution in the audit trail are all unreachable")
	}
}

func scopedActor(channels, projects []string) ActorIdentity {
	return ActorIdentity{
		Type:      ActorAgent,
		AgentID:   "agent-1",
		AgentName: "Support Bot",
		AgentScope: agentModel.AgentScope{
			ChannelIDs: channels,
			ProjectIDs: projects,
		},
	}
}

// A confined agent may act in the channels it was given and nowhere else.
func TestAgentResourceScopeConfinesChannels(t *testing.T) {
	actor := scopedActor([]string{"chan-a", "chan-b"}, nil)

	if ok, reason := CheckAgentResourceScope(actor, ResourceRef{Kind: ResourceChannel, ID: "chan-a"}); !ok {
		t.Fatalf("a scoped channel was refused: %s", reason)
	}
	ok, reason := CheckAgentResourceScope(actor, ResourceRef{Kind: ResourceChannel, ID: "chan-z"})
	if ok {
		t.Fatal("an agent acted in a channel its owner did not scope it to")
	}
	// Must not enumerate where the agent IS scoped — that is a map of where an identity
	// operates.
	for _, granted := range actor.AgentScope.ChannelIDs {
		if strings.Contains(reason, granted) {
			t.Errorf("the refusal leaks the agent's other channels (%q): %s", granted, reason)
		}
	}
}

func TestAgentResourceScopeConfinesProjects(t *testing.T) {
	actor := scopedActor(nil, []string{"proj-a"})

	if ok, _ := CheckAgentResourceScope(actor, ResourceRef{Kind: ResourceProject, ID: "proj-a"}); !ok {
		t.Fatal("a scoped project was refused")
	}
	if ok, _ := CheckAgentResourceScope(actor, ResourceRef{Kind: ResourceProject, ID: "proj-z"}); ok {
		t.Fatal("an agent acted in a project its owner did not scope it to")
	}
}

// EMPTY MEANS UNRESTRICTED — the opposite of the toolset rule, and the difference is not
// an inconsistency. An agent with no tools was given nothing to do; an agent with no
// channel scope was simply never confined, which is the default for every agent that
// predates the setting. Getting this backwards would confine every existing agent to
// nowhere.
func TestEmptyAgentScopeIsUnrestricted(t *testing.T) {
	actor := scopedActor(nil, nil)

	for _, ref := range []ResourceRef{
		{Kind: ResourceChannel, ID: "any-channel"},
		{Kind: ResourceProject, ID: "any-project"},
	} {
		if ok, _ := CheckAgentResourceScope(actor, ref); !ok {
			t.Fatalf("an unconfined agent was refused %s; empty scope must mean "+
				"'wherever its owner can act', or every agent predating the setting "+
				"would be confined to nowhere", ref.Kind)
		}
	}
}

// KINDS OTHER THAN CHANNEL AND PROJECT ARE NOT CONFINED, matching the in-app rule
// exactly. Being stricter only on this surface would be the same divergence in the
// opposite direction — a control that behaves differently depending on how the agent was
// invoked.
func TestAgentScopeDoesNotConfineOtherKinds(t *testing.T) {
	actor := scopedActor([]string{"chan-a"}, []string{"proj-a"})

	for _, kind := range []ResourceKind{
		ResourceDoc, ResourceTask, ResourceTable, ResourceChat,
		ResourceDirectMessage, ResourceWorkspace,
	} {
		if ok, reason := CheckAgentResourceScope(actor, ResourceRef{Kind: kind, ID: "x"}); !ok {
			t.Errorf("%s was confined by agent scope (%s), which the in-app rule does not "+
				"do — MCP must not be stricter than the app for the same agent", kind, reason)
		}
	}
}

// A stray space in an admin-entered scope list must not silently exclude something they
// believe they granted.
func TestAgentScopeToleratesWhitespaceInTheStoredList(t *testing.T) {
	actor := scopedActor([]string{" chan-a "}, nil)
	if ok, _ := CheckAgentResourceScope(actor, ResourceRef{Kind: ResourceChannel, ID: "chan-a"}); !ok {
		t.Fatal("a scope entry with surrounding whitespace failed to match")
	}
}

// An unbound integration credential has no confinement, so adding this rung must not
// affect any existing token.
func TestAPIClientIsUnaffectedByAgentResourceScope(t *testing.T) {
	client := ActorIdentity{Type: ActorAPIClient}
	if ok, _ := CheckAgentResourceScope(client, ResourceRef{Kind: ResourceChannel, ID: "chan-z"}); !ok {
		t.Fatal("an unbound credential was confined by an agent scope it does not have")
	}
	if ok, _ := CheckAgentArgScope(client, map[string]any{"channel_uuid": "chan-z"}); !ok {
		t.Fatal("an unbound credential was confined via arguments")
	}
}

// The argument-based form must reach the SAME verdict as the resource-based one, or the
// governed and ungoverned paths would confine an agent differently.
func TestAgentArgScopeAgreesWithTheResourceForm(t *testing.T) {
	actor := scopedActor([]string{"chan-a"}, []string{"proj-a"})

	cases := []struct {
		name  string
		args  map[string]any
		allow bool
	}{
		{"in-scope channel", map[string]any{"channel_uuid": "chan-a"}, true},
		{"out-of-scope channel", map[string]any{"channel_uuid": "chan-z"}, false},
		{"in-scope project", map[string]any{"project_uuid": "proj-a"}, true},
		{"out-of-scope project", map[string]any{"project_uuid": "proj-z"}, false},
		{"neither named", map[string]any{"text": "hello"}, true},
		{"blank channel", map[string]any{"channel_uuid": "  "}, true},
		// Both are checked independently: satisfying one does not excuse the other.
		{"good channel, bad project", map[string]any{"channel_uuid": "chan-a", "project_uuid": "proj-z"}, false},
		{"bad channel, good project", map[string]any{"channel_uuid": "chan-z", "project_uuid": "proj-a"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := CheckAgentArgScope(actor, tc.args)
			if got != tc.allow {
				t.Fatalf("allow=%v want %v (%s)", got, tc.allow, reason)
			}
		})
	}
}

// THE RATCHET. The confinement must be applied on BOTH call paths, and on both it must
// run before the budget is charged — a call about to be refused must not spend a
// legitimate credential's budget.
//
// Deliberately NOT folded into the shared-order test: the governed path can only check
// this after it has resolved the resource, so the rung genuinely sits at a different
// position on the two ladders. Asserting a shared position would have forced one of them
// into the wrong place.
func TestAgentConfinementIsAppliedOnBothPathsBeforeCharging(t *testing.T) {
	for _, target := range []struct {
		file   string
		needle string
		budget string
		why    string
	}{
		{"authorize.go", "CheckAgentResourceScope(", "CheckBudget(",
			"a governed call could act on a channel or project the agent's owner confined it out of"},
		{"ungoverned.go", "CheckAgentArgScope(", "CheckCallBudget(",
			"an ungoverned call could act outside the agent's confinement, so the setting " +
				"would cover 22 tools and not the other 8"},
	} {
		raw, err := os.ReadFile(target.file)
		if err != nil {
			t.Fatalf("read %s: %v", target.file, err)
		}
		src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

		at := strings.Index(src, target.needle)
		if at < 0 {
			t.Errorf("%s does not apply %s: %s", target.file, target.needle, target.why)
			continue
		}
		if b := strings.Index(src, target.budget); b >= 0 && b < at {
			t.Errorf("%s charges the budget before checking the agent's confinement, so a "+
				"call that was going to be refused still spends the credential's budget",
				target.file)
		}
	}
}
