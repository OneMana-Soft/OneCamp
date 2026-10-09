package business

// An agent asked by someone other than its sponsor: what it may do for them.
//
// Every tool an agent calls executes as its sponsor (agentRunner: userUUID =
// agent.CreatedBy), and each executor re-checks the SPONSOR's permissions. That
// made the sponsor's whole reach available to anyone who could address the
// agent: a DM to a dm_able agent, a mention, a task handed to it. Asked "find
// the plan" by a teammate, it searched the sponsor's private channels and DMs
// and answered the teammate; asked to check mail, it read the sponsor's Gmail.
//
// The rule now: a run someone else asked for reaches only what BOTH of them
// can. The sponsor bound is the one the executors already enforce. The asker
// bound is enforced in three places, each the narrowest seam for its kind of
// read:
//
//   - A call that names an object (read a doc, summarize a channel, post in
//     one, change a task) is refused here, before it runs, unless the asker
//     could act on that object themselves. The question is the MCP surface's
//     own (business/MCPServer.PrincipalCanReach), against the same per-tool
//     declarations, so an agent inside the workspace and an agent connecting
//     from outside are held to one rule.
//   - A search is narrowed in the index itself: the asker's permission filter
//     is ANDed with the sponsor's (services/AI runRequester.go), so a hit must
//     be visible to both.
//   - A list ("my projects", "my tasks", "tables") is narrowed as it is built
//     (business/AI requesterReach.go).
//
// The sponsor's PERSONAL reach — their connected accounts, their calendar,
// their direct messages — is not shared with anyone, so those tools refuse
// outright unless the sponsor is the one asking. A run nobody asked for (a
// schedule, an event) acts for its sponsor alone, unchanged.
//
// Fails closed throughout: a run whose asker cannot be identified, a call whose
// target cannot be read from its arguments, a tool with no declaration, or a
// lookup that fails, is a refusal with a reason the model can relay — never a
// fall back to the sponsor's reach.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	mcpServer "github.com/akashc777/OneCamp/business/MCPServer"
	pollBusiness "github.com/akashc777/OneCamp/business/Poll"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// personalTools act as the sponsor in a way nobody else may borrow: through an
// account they connected, on their own calendar, or in their own direct
// messages. The value is how the refusal names what is theirs.
var personalTools = map[string]string{
	"gmail_search":          "connected accounts",
	"gmail_send":            "connected accounts",
	"calendar_list_events":  "connected accounts",
	"calendar_create_event": "connected accounts",
	"github_list_prs":       "connected accounts",
	"github_list_issues":    "connected accounts",
	"github_comment":        "connected accounts",
	// set_reminder writes an event to the sponsor's calendar, synced to Google.
	"set_reminder": "calendar",
	// Both name the OTHER person, and the conversation they mean is the sponsor's
	// own DM with them: a DM has no id of its own, so there is no way for the
	// asker to name a DM that is theirs instead.
	"summarize_dm": "direct messages",
	"send_dm":      "direct messages",
}

// askedBy records who asked for the run the context is about to start: the
// person a DM, a mention or a task assignment came from. An empty requesterUUID
// means a person asked and could not be identified, and the run then refuses
// everything that needs one rather than act with the sponsor's reach.
//
// Only the asker is recorded here. Whose agent it is gets bound once, by the
// runner (bindRunRequester), so no entry point can pair an asker with the wrong
// sponsor; until then the asker is treated as someone other than the sponsor,
// which can only narrow what is read on the way in.
func askedBy(ctx context.Context, requesterUUID string) context.Context {
	return ai.WithRunRequester(ctx, requesterUUID, "")
}

// bindRunRequester ties whoever asked for this run to THIS agent's sponsor,
// the one place that pairing is made. A run started from inside another
// agent's run (a delegated mention) is therefore judged against its own
// sponsor, never the previous agent's.
func bindRunRequester(ctx context.Context, agent *model.AiAgent) context.Context {
	requester, asked := ai.RunAskedBy(ctx)
	if !asked {
		return ctx
	}
	return ai.WithRunRequester(ctx, requester, agent.CreatedBy.String())
}

// askerOf is who the run ctx carries is for, as the person something it sets
// up is recorded for (a routine, a remembered instruction): the asker when that
// is someone other than the sponsor, the sponsor otherwise. known is false when
// someone else asked and could not be identified.
func askerOf(ctx context.Context, agent *model.AiAgent) (who uuid.UUID, known bool) {
	requester, _, forOther := ai.RunRequester(ctx)
	if !forOther {
		return agent.CreatedBy, true
	}
	id, err := uuid.Parse(strings.TrimSpace(requester))
	return id, err == nil && id != uuid.Nil
}

// requesterGate decides calls for one turn of a run, resolving the people it
// names once rather than per call.
type requesterGate struct {
	requester string
	// sponsorName is how a refusal names the sponsor: the name the app shows
	// for them, or a description when it cannot be resolved.
	sponsorName string
}

// newRequesterGate returns the gate for a run acting for someone other than its
// sponsor, or nil for a run that acts for its sponsor alone (nothing to check).
func newRequesterGate(ctx context.Context) *requesterGate {
	requester, sponsor, ok := ai.RunRequester(ctx)
	if !ok {
		return nil
	}
	return &requesterGate{requester: requester, sponsorName: personName(ctx, sponsor, "the person who set me up")}
}

// refusal says why the run may not make call a for the person it acts for, or
// "" when it may.
func (g *requesterGate) refusal(ctx context.Context, a ai.ProposedAction) string {
	if g == nil {
		return ""
	}
	if strings.TrimSpace(g.requester) == "" {
		return fmt.Sprintf("I couldn't tell who asked for this, so I won't do it with %s's access", g.sponsorName)
	}
	if what, personal := personalTools[a.ToolName]; personal {
		return fmt.Sprintf("only %s can ask me to use their %s", g.sponsorName, what)
	}
	switch a.ToolName {
	case codePRToolName:
		return g.codePRRefusal(ctx)
	case "read_poll":
		return g.pollRefusal(ctx, a.Params["poll_uuid"])
	case "run_analysis":
		return g.analysisRefusal(ctx, a.Params["inputs"])
	}
	if !isStaticTool(a.ToolName) {
		// A tool from an admin-registered MCP server. It acts with that server's
		// own credential, not the sponsor's or anyone's personal access, so the
		// person asking needs only to be someone who may ask for work at all.
		return g.reach(ctx, mcpServer.ResourceRef{Kind: mcpServer.ResourceWorkspace, Access: mcpServer.AccessRead})
	}
	ref, known, err := mcpServer.ResourceForToolCall(a.ToolName, a.Params)
	if !known {
		return fmt.Sprintf("%s can only be run for %s; it has no rule for anyone else", a.ToolName, g.sponsorName)
	}
	if err != nil {
		return fmt.Sprintf("%v, so I won't run it with %s's access", err, g.sponsorName)
	}
	return g.reach(ctx, ref)
}

// reach asks the one authority question for the asker: may they act on ref
// themselves?
func (g *requesterGate) reach(ctx context.Context, ref mcpServer.ResourceRef) string {
	d := mcpServer.PrincipalCanReach(ctx, g.requester, ref)
	if d.Allow {
		return ""
	}
	return g.reachRefusal(ctx, ref, d.Reason)
}

// channelIsPublic reports whether everyone signed in may read a channel; a seam.
var channelIsPublic = channelBusiness.IsPublic

// reachRefusal words the authorizer's refusal for the model to relay. A public
// channel the asker hasn't joined is refused like any channel they aren't in,
// since it is membership that decides what an agent reads for someone; but
// "outside what they can reach" is untrue of a channel anyone may open, so it
// says what would change the answer instead.
func (g *requesterGate) reachRefusal(ctx context.Context, ref mcpServer.ResourceRef, reason string) string {
	if ref.Kind == mcpServer.ResourceChannel && reason == mcpServer.ReasonNotChannelMember {
		if public, err := channelIsPublic(ctx, g.requester, ref.ID); err == nil && public {
			return fmt.Sprintf("the person who asked hasn't joined this channel. It's open to everyone, so they can join "+
				"it and ask me again; I only read channels both they and %s are in", g.sponsorName)
		}
	}
	return fmt.Sprintf("this is outside what the person who asked can reach themselves (%s). I'm acting for them, "+
		"so I only reach what both they and %s can", askerReason(reason), g.sponsorName)
}

// askerReason words an authority refusal about the person who asked. The
// authorizer's reasons name "the originating person" (the MCP surface's term
// for the human behind a call), which reads oddly in a sentence the model
// relays to that same person.
var askerReason = strings.NewReplacer(
	"the originating person's", "their",
	"the originating person is", "they're",
	"the originating person has", "they have",
	"the originating person", "they",
).Replace

// codePRRefusal: code_pr pushes with a person's own GitHub account, and for a
// run someone else asked for that account is theirs. Without one there is
// nothing to push with, and borrowing the sponsor's would be the exact thing
// this file exists to stop.
func (g *requesterGate) codePRRefusal(ctx context.Context) string {
	id, err := uuid.Parse(g.requester)
	if err != nil {
		return "I couldn't tell who asked for this change, so I won't open a pull request for it"
	}
	if !connectorBusiness.IsConnected(ctx, id, connectorBusiness.ProviderGitHub) {
		return fmt.Sprintf("a pull request I open for someone is pushed with their own GitHub account, and the person who asked hasn't connected one; "+
			"they can connect GitHub under Settings → Connectors and ask again (I won't use %s's)", g.sponsorName)
	}
	return ""
}

// pollRefusal: a poll lives in a post, so the poll layer's own visibility rule
// (the same one a person opening it meets) decides, asked as the asker.
func (g *requesterGate) pollRefusal(ctx context.Context, pollID string) string {
	info, err := aiBusiness.BuildUserInfoByUserUUID(ctx, g.requester)
	if err != nil || info == nil {
		return fmt.Sprintf("I couldn't confirm the person who asked can see that poll, so I won't read it with %s's access", g.sponsorName)
	}
	if _, err := pollBusiness.Get(ctx, info, strings.TrimSpace(pollID)); err != nil {
		return fmt.Sprintf("the person who asked can't see that poll, and I only reach what both they and %s can", g.sponsorName)
	}
	return ""
}

// analysisRefusal: run_analysis reads the tables named in its inputs into the
// sandbox, so the asker must be able to view every one of them. Code with no
// inputs reads no workspace data.
func (g *requesterGate) analysisRefusal(ctx context.Context, raw string) string {
	var inputs []struct {
		TableUUID string `json:"table_uuid"`
	}
	if s := strings.TrimSpace(raw); s != "" && s != "[]" {
		if err := json.Unmarshal([]byte(s), &inputs); err != nil {
			return fmt.Sprintf("I couldn't tell which tables this would read, so I won't run it with %s's access", g.sponsorName)
		}
	}
	for _, in := range inputs {
		if why := g.reach(ctx, mcpServer.ResourceRef{Kind: mcpServer.ResourceTable, ID: in.TableUUID, Access: mcpServer.AccessRead}); why != "" {
			return why
		}
	}
	return g.reach(ctx, mcpServer.ResourceRef{Kind: mcpServer.ResourceWorkspace, Access: mcpServer.AccessRead})
}

// progressRefusal is why a run for someone else does not write the agent's
// working notes: every later run reads them as its plan, the ones nobody is
// watching included, and those act with the sponsor's whole reach.
func progressRefusal(g *requesterGate) string {
	return fmt.Sprintf("not saved: I keep working notes only on work for %s, because my later runs follow them", g.sponsorName)
}

// refusals decides every call of one turn the runner would otherwise execute,
// keyed by action signature. Calls the runner handles itself (pausing,
// progress, memory, routines) and calls not on the agent's allow-list are left
// out: neither reaches an executor. nil when the run acts for its sponsor alone.
func (g *requesterGate) refusals(ctx context.Context, actions []ai.ProposedAction, allow map[string]bool) map[string]string {
	if g == nil {
		return nil
	}
	out := map[string]string{}
	for _, a := range actions {
		sig := ai.ActionSignature(a)
		if _, done := out[sig]; done || isControlTool(a.ToolName) || !allow[a.ToolName] {
			continue
		}
		out[sig] = g.refusal(ctx, a)
	}
	return out
}

// isStaticTool reports whether a tool is one of OneCamp's own (compile-time
// registry), as opposed to one an admin-registered MCP server contributed.
func isStaticTool(name string) bool {
	for _, t := range ai.ToolRegistry {
		if t.Name == name {
			return true
		}
	}
	return false
}

// personName is how a person is named to the model: the name the app shows for
// them, or fallback when it cannot be resolved.
func personName(ctx context.Context, userUUID, fallback string) string {
	if _, err := uuid.Parse(strings.TrimSpace(userUUID)); err != nil {
		return fallback
	}
	u, err := lookupPersonName(ctx, strings.TrimSpace(userUUID))
	if err != nil || u == nil {
		return fallback
	}
	for _, name := range []string{u.UserFullName, u.UserName} {
		if name = strings.Join(strings.Fields(name), " "); name != "" {
			return name
		}
	}
	return fallback
}

// lookupPersonName is a seam, so naming is tested without a graph.
var lookupPersonName = userBusiness.GetDgraphUserInfoByUUID

// actingForPrompt tells the model whom it is working for, so a refusal reads
// to it as a boundary to explain rather than a fault to retry around.
func actingForPrompt(ctx context.Context, g *requesterGate) string {
	if g == nil {
		return ""
	}
	asker := personName(ctx, g.requester, "someone")
	return fmt.Sprintf("\n\nYou are working for %s, who is not the person who set you up (%s). You can only see and change what "+
		"both of them can, and %s's own accounts, calendar and direct messages are not available to you here. When a tool "+
		"result says it was skipped for that reason, tell %s plainly that you can't reach it for them; never guess at what it would have shown.",
		asker, g.sponsorName, g.sponsorName, asker)
}
