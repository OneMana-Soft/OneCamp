package business

import (
	"context"
	"errors"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// On the public demo everyone shares one visitor account, and the visitor's
// assistant (and the demo's agent, which acts as a member of the seeded cast)
// could still find the demo's real members by name: "send cannabisd a DM",
// "assign this to Akash". The pickers already list only the demo's own people
// (helpers.DemoListsPerson); an AI action is held to the same rule, so nothing
// a visitor asks for reaches a real account.
//
// The rule applies when the account an action is taken as is one of the
// demo's own people (the visitor, the cast, a bot): the people who run the
// demo, using their own assistant as themselves, reach whom they like, and
// on any other server nothing changes.

// errDemoPerson is what an action that would reach a real person on the demo
// is refused with, in words the model can relay.
var errDemoPerson = errors.New("on the demo, the assistant can only reach the demo's own people " +
	"and its agents; install OneCamp to work with your own team")

// actingEmail is the address of the account an AI action is taken as. A seam,
// so the rule is tested without a graph.
var actingEmail = func(ctx context.Context, actingUUID string) string {
	u, err := getDgraphUserForExecutor(ctx, actingUUID)
	if err != nil || u == nil {
		return ""
	}
	return u.EmailID
}

// demoActorRestricted reports whether actions taken as actingUUID may reach
// only the demo's own people: on the demo, for one of its own people, and
// when who it is is unknown (fails closed).
func demoActorRestricted(ctx context.Context, actingUUID string) bool {
	if !helpers.DemoMode() {
		return false
	}
	email := actingEmail(ctx, actingUUID)
	return email == "" || helpers.IsDemoPerson(email)
}

// demoReachable is people without those an AI action taken as actingUUID may
// not reach on the demo. Everywhere else, people as it is.
func demoReachable(ctx context.Context, actingUUID string, people []*dgraphStruct.DgraphUser) []*dgraphStruct.DgraphUser {
	if !demoActorRestricted(ctx, actingUUID) {
		return people
	}
	kept := make([]*dgraphStruct.DgraphUser, 0, len(people))
	for _, p := range people {
		if p != nil && (p.IsBot || helpers.IsDemoPerson(p.EmailID)) {
			kept = append(kept, p)
		}
	}
	return kept
}

// demoRefuses is errDemoPerson when an AI action taken as actingUUID may not
// reach any of targets on the demo, and nil otherwise.
func demoRefuses(ctx context.Context, actingUUID string, targets ...*dgraphStruct.DgraphUser) error {
	n := 0
	for _, t := range targets {
		if t != nil {
			n++
		}
	}
	if n == 0 || !demoActorRestricted(ctx, actingUUID) {
		return nil
	}
	if len(demoReachable(ctx, actingUUID, targets)) != n {
		return errDemoPerson
	}
	return nil
}

// Nor does anything a demo person starts reach OUTSIDE the demo. An MCP
// server's tools act with the credential of whoever added the server (on the
// demo, an admin's GitHub token), and a remote agent is sent the conversation.
// A visitor's DM to an admin's agent, or the visitor's own agent with the
// admin's tools switched on, used both. So on the demo a run may use outside
// tools or a remote agent only when it is known to act for, and to have been
// asked for by, people who aren't the demo's own.

// errDemoOutside is what reaching outside is refused with, in words the model
// can relay and a person can act on.
var errDemoOutside = errors.New(helpers.DemoOutsideMsg)

// DemoRefusesOutside is errDemoOutside when, on the demo, a run acting as
// actingUUID may not reach anything outside the workspace: an MCP server's
// tools, a remote agent. That is when the account it acts as, or the person
// who asked for it, is one of the demo's own people or can't be identified,
// or when nobody asked and an event or another agent started it (anyone may
// have written what it reacts to). nil everywhere else, and always off the
// demo.
func DemoRefusesOutside(ctx context.Context, actingUUID string) error {
	if !helpers.DemoMode() {
		return nil
	}
	if demoActorRestricted(ctx, actingUUID) {
		return errDemoOutside
	}
	if requester, asked := ai.RunAskedBy(ctx); asked {
		if requester == "" || demoActorRestricted(ctx, requester) {
			return errDemoOutside
		}
		return nil
	}
	if by, ok := auditBusiness.InitiatorFromCtx(ctx); ok &&
		(by == auditBusiness.InitiatorEvent || by == auditBusiness.InitiatorHandoff) {
		return errDemoOutside
	}
	return nil
}

// The rule guards every outside tool where it runs, whoever calls it.
func init() { ai.RegisterOutsideReachGuard(DemoRefusesOutside) }
