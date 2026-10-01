package business

// Admission control: is this surface open, and does it expose this tool?
//
// A DIFFERENT QUESTION FROM EVERY OTHER CHECK IN THIS PACKAGE. The rest of the ladder
// asks what a PERSON may do — their memberships, their grants, their token's scopes. This
// asks what the WORKSPACE has agreed to expose at all, which is the admin's decision
// rather than the caller's.
//
// The two compose in one direction only. Admission can narrow what is reachable and can
// never widen it: a tool an admin has enabled is still bounded by the token's scopes
// intersected with its owner's live permission on the specific object. Turning everything
// on returns the surface to exactly the authority it already had, which is what makes this
// safe to add to something already in use.
//
// WHY GROUPS AND NOT TOOLS. The list has to stay answerable by the person deciding. "May
// agents read our documents" is a question an admin can weigh; "should read_doc be on but
// summarize_channel off" is one they answer by enabling everything, which is the same as
// having no control. Groups are the scope prefixes that already exist — tasks, docs,
// messages, tables, search — so a new tool joins an approved group automatically and
// cannot invent a group nobody has agreed to.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	aiSettingsModel "github.com/akashc777/OneCamp/models/postgres/AI"
)

// wildcardGroup means every group, present and future.
//
// A deliberate escape hatch for the operator who wants the whole surface and does not want
// to revisit the setting when a tool is added. The alternative — making them re-approve on
// every upgrade — produces approval fatigue, and an admin who has said "all of it" has
// made a real decision worth honouring.
const wildcardGroup = "*"

// ToolGroup is the admin-facing group a tool belongs to, derived from its required scope.
//
// DERIVED, NEVER DECLARED. Scopes are already "<group>:<verb>" (tasks:read,
// messages:write), so the group is the prefix. A second field on ToolSpec saying which
// group a tool is in would be a second thing to keep true, and the failure mode is a tool
// filed under a group an admin approved for something else.
//
// A scope with no colon is its own group. That is not a case today, and treating it as a
// group of one keeps the function total rather than returning an error nobody can act on.
func ToolGroup(spec *ToolSpec) string {
	if spec == nil {
		return ""
	}
	return ToolGroupForScope(spec.RequiredScope)
}

// ToolGroupForScope is the group rule itself, on a bare scope string.
//
// Exists because admission has to be decidable for a tool that has NO ToolSpec. Half
// the public catalogue is not governed yet — no authority rule has been written for
// it — and those tools were escaping admission entirely: an admin could disable the
// MCP surface and they stayed callable, because the only admission entry point
// required a spec they do not have.
//
// A tool's group must not depend on whether someone has got round to governing it.
// The scope is the one thing every public tool has, governed or not, so the rule
// operates on that and ToolGroup becomes a thin wrapper over it.
func ToolGroupForScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if i := strings.Index(scope, ":"); i > 0 {
		return scope[:i]
	}
	return scope
}

// AllToolGroups is every group the registry currently contains, sorted.
//
// For the admin UI, so the choices offered are exactly the groups that exist rather than a
// hardcoded list that drifts from the registry. An admin cannot be shown a group with no
// tools in it, and cannot fail to be shown one that has appeared.
// It is the union of the governed registry and the FULL public tool catalogue, and the
// union is the load-bearing part. Governance is a per-tool rule someone writes over
// time; admission is a workspace decision that has to cover everything reachable
// TODAY. Deriving the choices from the governed registry alone left whole groups
// unofferable — calendar and data_sources have no governed tool — so an admin could
// not enable them, and once admission gates those tools they would have been
// permanently unreachable with no setting able to say otherwise.
func AllToolGroups() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 16)
	add := func(g string) {
		if g == "" || seen[g] {
			return
		}
		seen[g] = true
		out = append(out, g)
	}

	registryMu.RLock()
	for _, spec := range registry {
		add(ToolGroup(spec))
	}
	registryMu.RUnlock()

	// Every publicly exposable tool, governed or not. This is the same map the
	// non-governed call path reads its scope from, so the groups an admin is offered
	// and the groups actually enforced come from one source.
	for _, scope := range apiTokenBusiness.ToolScope {
		add(ToolGroupForScope(scope))
	}

	sort.Strings(out)
	return out
}

// ParseToolGroups reads the stored comma-separated allowlist.
//
// Lenient about formatting and strict about meaning: whitespace and empty entries are
// discarded, names are lowercased, but an empty result stays empty rather than becoming a
// wildcard. That asymmetry is the important part — a malformed or truncated setting must
// close the surface, not open it.
func ParseToolGroups(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		g := strings.ToLower(strings.TrimSpace(p))
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	return out
}

// AdmissionSettings is the admin's decision, passed in rather than read here.
//
// Kept as a plain struct with no I/O so the rule is a pure function: testable
// exhaustively, and unable to fail separately from the settings load that feeds it. The
// caller already loads AI settings for other reasons on this path.
type AdmissionSettings struct {
	// Enabled is whether the MCP surface is exposed at all.
	Enabled bool
	// ToolGroups is the raw stored allowlist, as a comma-separated string.
	ToolGroups string
}

// AdmissionDecision is the outcome, with a reason populated either way so the audit record
// and the client-facing refusal come from one place.
type AdmissionDecision struct {
	Allow  bool
	Reason string
}

// CheckAdmission decides whether the workspace exposes this tool.
//
// Deny-by-default at every step: not enabled, no groups named, or a group not on the list
// are all refusals. A workspace that has said nothing exposes nothing.
//
// The reasons distinguish the three cases on purpose, because they need three different
// actions from whoever reads them. "MCP is off" is one toggle; "no groups enabled" means
// the admin turned it on and stopped; "this group is not enabled" means a deliberate
// narrowing the caller should not expect to be widened. Collapsing them into "not
// permitted" would send an operator hunting.
func CheckAdmission(settings AdmissionSettings, spec *ToolSpec) AdmissionDecision {
	if spec == nil {
		// Ordered before the Enabled check only because "there is no tool here" is a
		// caller bug rather than a policy outcome, and reporting the policy reason for
		// it would send someone to the admin panel to fix a nil pointer.
		if !settings.Enabled {
			return AdmissionDecision{Reason: "the MCP surface is not enabled for this workspace"}
		}
		return AdmissionDecision{Reason: "no tool to admit"}
	}
	return CheckAdmissionForScope(settings, spec.RequiredScope)
}

// CheckAdmissionForScope is the admission rule on a bare scope, for the public tools
// that have no ToolSpec yet. Same rule, same deny-by-default, same three
// distinguishable reasons — see CheckAdmission and ToolGroupForScope for why this
// has to be reachable without a spec.
func CheckAdmissionForScope(settings AdmissionSettings, scope string) AdmissionDecision {
	if !settings.Enabled {
		return AdmissionDecision{Reason: "the MCP surface is not enabled for this workspace"}
	}

	groups := ParseToolGroups(settings.ToolGroups)
	if len(groups) == 0 {
		return AdmissionDecision{Reason: "the MCP surface is enabled but exposes no tool groups yet"}
	}

	want := ToolGroupForScope(scope)
	if want == "" {
		// A tool with no scope cannot be placed in any group an admin has approved.
		// Refusing is the only safe reading: admitting it would mean the one tool
		// nobody can describe is the one tool nobody can switch off.
		return AdmissionDecision{Reason: "this tool declares no scope, so no tool group can admit it"}
	}
	for _, g := range groups {
		// The wildcard is checked inside the loop rather than before it, so it is one
		// entry among others and cannot be reached by an empty list.
		if g == wildcardGroup || g == want {
			return AdmissionDecision{Allow: true, Reason: "tool group " + want + " is enabled"}
		}
	}
	return AdmissionDecision{Reason: "the " + want + " tool group is not enabled for this workspace"}
}

// There was an AdmittedTools(settings, specs) slice-filter here. It existed only for
// protocol.go's ListTools catalogue, which has been removed, and it was a loop around
// CheckAdmission. The catalogue that ships filters with CheckAdmissionForScope inline over
// ai.ToolRegistry, so the rule still applies to the listing — see listToolsForScopes.

// SetAdmission writes the admin's admission-control decision.
//
// LIVES HERE, NOT IN business/AI, and the reason is structural rather than tidiness:
// business/MCPServer already imports business/AI for the approval-card creator, so putting
// this beside the other AI settings writers would close an import cycle. It also belongs
// here on its own merits — this is MCP policy, and the group names it validates are
// derived from this package's registry.
//
// VALIDATES THE GROUP NAMES, because an unrecognised name is silently equivalent to
// exposing nothing. An admin would toggle the surface on, name a group, save successfully,
// and find agents still see no tools — a worse outcome than an error, because nothing
// about it suggests where to look.
//
// Checked against the live registry rather than a hardcoded list, so the valid names are
// exactly the groups the running binary has tools for. The error names them, since
// "invalid group" without the options leaves an admin guessing at strings.
//
// No AI-service reload, unlike the delegation setter: these settings are read fresh on
// each authorization rather than cached, so a save takes effect on the next call.
func SetAdmission(ctx context.Context, enabled bool, toolGroups string) error {
	groups := ParseToolGroups(toolGroups)

	// Refusing "on with nothing named" rather than saving it. It is a valid state in the
	// database and never a state anyone wants: it reads as enabled while behaving as off.
	if enabled && len(groups) == 0 {
		return fmt.Errorf("name at least one tool group, or %q for all: enabling the MCP "+
			"surface without a group exposes nothing", wildcardGroup)
	}

	known := AllToolGroups()
	valid := map[string]bool{wildcardGroup: true}
	for _, g := range known {
		valid[g] = true
	}
	for _, g := range groups {
		if !valid[g] {
			return fmt.Errorf("unknown tool group %q; valid groups are: %s, or %q for all",
				g, strings.Join(known, ", "), wildcardGroup)
		}
	}

	return aiSettingsModel.SetMCPServer(ctx, enabled, toolGroups)
}

// Admission reads the current decision, for the admin UI.
//
// Returns the available groups alongside the stored ones so the client renders the choices
// that actually exist rather than a list of its own — the same reason the audit log serves
// its categories. A group added by a new tool appears in the UI with nothing to remember.
func Admission(ctx context.Context) (AdmissionSettings, []string, error) {
	settings, err := aiSettingsModel.GetSettings(ctx)
	if err != nil {
		return AdmissionSettings{}, nil, err
	}
	if settings == nil {
		return AdmissionSettings{}, nil, ErrUnauthorized
	}
	return AdmissionSettings{
		Enabled:    settings.MCPEnabled,
		ToolGroups: settings.MCPToolGroups,
	}, AllToolGroups(), nil
}
