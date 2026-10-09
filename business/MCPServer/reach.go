package business

// PrincipalCanReach — the one place that decides whether the human behind an MCP
// call may act on a specific object.
//
// THE PROPERTY THIS EXISTS FOR. An MCP call arrives from an external process with
// no session and nobody at a keyboard. Its credential is an api_token, and that
// row carries created_by: the human who minted it. So the effective authority of
// any call is
//
//	token.scopes  ∩  that human's LIVE permission on THIS object
//
// an intersection, never a union. Two consequences follow, and they are the reason
// this is worth building rather than buying:
//
//   - Attenuation is live. Remove someone from a channel and their agent loses that
//     channel on the NEXT call. No token rotation, no cache to invalidate, nothing
//     for an operator to remember.
//   - A token can never exceed its human. Scopes only narrow. An admin widening the
//     scopes on a limited member's token grants nothing, because the intersection
//     still bounds it. The confused-deputy path is closed by construction rather
//     than by policy.
//
// This is also what a gateway product structurally cannot do. A gateway in front of
// SaaS sees the token's scopes and stops, because the system behind it does not
// expose per-object membership for a third party to consult on every call. OneCamp
// owns the permission graph, so it can ask.
//
// DELIBERATELY THE SAME LOOKUPS as originCanAddressSurface in
// business/AIAgent/agentDelegation.go. A second implementation of "may this person
// touch this thing" is how two permission models drift until one of them is wrong;
// the in-workspace delegation surface and the MCP surface must agree by
// construction. If the rule changes, it changes for both.
//
// Deny-by-default throughout: anything that cannot be positively verified is a
// refusal.

import (
	"context"
	"errors"
	"strings"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	dataSourceBusiness "github.com/akashc777/OneCamp/business/DataSource"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// ReachDecision is the outcome of one authority question. Reason is always
// populated, including on allow, so the audit record and the client-facing refusal
// come from the same place and cannot disagree.
type ReachDecision struct {
	Allow  bool
	Reason string
	// PrincipalDgraphUID is resolved as a side effect of deciding and handed to the
	// handler, so a handler never resolves the principal again (and so cannot
	// resolve a different one).
	PrincipalDgraphUID string
}

// ReasonNotChannelMember is the refusal for a channel the person isn't in. A
// public one included: it is membership that decides, here as in the AI index.
const ReasonNotChannelMember = "the originating person is not a member of this channel"

// PrincipalCanReach decides whether principalUserID may act on ref.
//
// principalUserID is the api_token's created_by — the human at the root. It is
// never the token, and never an admin acting on their behalf.
func PrincipalCanReach(ctx context.Context, principalUserID string, ref ResourceRef) (decision ReachDecision) {
	// FAIL CLOSED, NEVER CRASH. The permission lookups below reach a graph client
	// that is nil until the server has connected, and the underlying driver
	// dereferences it without checking — so an authorization question asked while
	// the graph is unavailable panics rather than returning an error.
	//
	// For an authorization function the only safe failure is a refusal. Recovering
	// to a denial means a datastore outage degrades to "no agent can act", which is
	// correct and boring, instead of taking down the process that was still serving
	// humans fine. A guard that can crash the server it protects is not a guard.
	//
	// This is not defensive noise: the panic is reachable today, and it was found by
	// a test asking a perfectly ordinary question with no database attached.
	defer func() {
		if r := recover(); r != nil {
			decision = ReachDecision{Reason: "permission lookup unavailable; refusing"}
		}
	}()

	principal := strings.TrimSpace(principalUserID)
	if principal == "" {
		return ReachDecision{Reason: "no originating person for this call"}
	}

	// Validate the reference BEFORE any lookup. An unusable ref is a refusal on its
	// own terms, and resolving the user first would spend a query to reach the same
	// answer on the request hot path.
	if ref.Kind != ResourceWorkspace && strings.TrimSpace(ref.ID) == "" {
		return ReachDecision{Reason: "no resource id for a resource-scoped tool"}
	}

	// The principal's graph node is needed by every membership query.
	dgraphUser, err := userDomain.GetDgraphUserInfoByUUID(ctx, principal)
	if err != nil {
		return ReachDecision{Reason: "the originating person could not be resolved"}
	}

	// IS THIS IDENTITY ALLOWED TO AUTHORIZE ANYTHING? Asked before, and separately
	// from, what it may reach.
	//
	// Resolving a person proves their NODE exists, not that the human behind it can
	// still act. Deactivation writes a timestamp and stops: it leaves every channel
	// and project membership edge in place and revokes no tokens, and the lookup
	// above does not filter deleted_at. So the membership checks below would answer
	// "yes, still a member" for someone offboarded months ago.
	//
	// That would make this file's headline promise selectively false. Attenuation
	// really is live for channel removal — and would have been dead for the one
	// event an operator most expects it to cover.
	//
	// Shared with the in-workspace delegation guard rather than restated, so an
	// identity refused here cannot authorize the same work by mentioning an agent
	// in a channel instead.
	if eligible := principalBusiness.Assess(dgraphUser); !eligible.Allowed {
		return ReachDecision{Reason: eligible.Reason}
	}
	uid := dgraphUser.Uid

	switch ref.Kind {
	case ResourceWorkspace:
		// No single object. Authority is "is a real person in this workspace", which
		// resolving them has just established. Tools using this kind MUST scope
		// their own results to the principal — the authorizer cannot do it for them,
		// and that obligation is stated on ResourceWorkspace.
		//
		// A WRITE is refused outright. There is no object to authorise against, so
		// there is nothing for a write to be checked with — a workspace-scoped
		// mutation would be a mutation nobody approved of anything in particular.
		// Creation tools land here naturally (they name no existing object), and this
		// is why they stay ungoverned rather than being quietly allowed: creating
		// needs a rule about the CONTAINER, which is a different reference.
		if !ref.Access.IsRead() {
			return ReachDecision{Reason: "a workspace-scoped tool may not write; a write must name the object it changes"}
		}
		return ReachDecision{Allow: true, Reason: "workspace-scoped tool; results must be scoped by the handler", PrincipalDgraphUID: uid}

	case ResourceChannel:
		channelUUID, perr := uuid.Parse(strings.TrimSpace(ref.ID))
		if perr != nil {
			return ReachDecision{Reason: "channel id is not a uuid"}
		}
		info, cerr := channelBusiness.GetDgraphChannelInfoByUUIDAndMemberInfo(ctx, channelUUID, uid, "")
		if cerr != nil || info == nil {
			return ReachDecision{Reason: "channel could not be read"}
		}
		// Deleting a channel does not clear its membership edges, so the count
		// below still reports a member of a channel nobody can open. Liveness is
		// checked first because "it is gone" is a truer refusal than "you are not
		// in it", and the reason is what an operator reads in the refusal log.
		if helpers.IsSoftDeleted(info.DeletedAt) {
			return ReachDecision{Reason: "this channel has been deleted"}
		}
		if info.IsMember <= 0 {
			return ReachDecision{Reason: ReasonNotChannelMember}
		}
		// WRITE needs more than membership. A channel can be admins-only, and the
		// shipped rule for that is exactly this comparison — see the identical check
		// in business/AI/aiExecutors.go and controllers/Post/postController.go. Reading
		// such a channel is still fine for any member; only posting is restricted.
		if !ref.Access.IsRead() &&
			info.PostPolicy == channelBusiness.ChannelPostPolicyAdminsOnly && info.IsAdmin <= 0 {
			return ReachDecision{Reason: "this channel is admins-only for posting"}
		}
		return ReachDecision{Allow: true, Reason: "channel member", PrincipalDgraphUID: uid}

	case ResourceTask:
		// Project membership is what grants sight of a task, exactly as the work
		// feed and the delegation guard decide it.
		info, terr := taskBusiness.GetDgraphTaskInfo(ctx, strings.TrimSpace(ref.ID), uid)
		if terr != nil || info == nil || info.Project == nil {
			return ReachDecision{Reason: "task or its project could not be read"}
		}
		// Both objects have to still exist. Deleting a task leaves the project's
		// membership edges untouched, and archiving a project leaves its tasks
		// addressable, so neither deletion is visible in the membership count.
		if helpers.IsSoftDeleted(info.DeletedAt) {
			return ReachDecision{Reason: "this task has been deleted"}
		}
		if helpers.IsSoftDeleted(info.Project.DeletedAt) {
			return ReachDecision{Reason: "the project owning this task has been deleted"}
		}
		if info.Project.IsProjectMember <= 0 {
			return ReachDecision{Reason: "the originating person is not a member of the project owning this task"}
		}
		// WRITE requires project ADMIN, the same rule the shipped task controllers
		// apply to a mutation (taskController.go tests IsProjectAdmin before changing
		// status, reassigning, or bulk-editing). Seeing a task on a board does not
		// carry the right to reassign it.
		if !ref.Access.IsRead() && info.Project.IsProjectAdmin <= 0 {
			return ReachDecision{Reason: "changing this task requires being an admin of its project"}
		}
		return ReachDecision{Allow: true, Reason: "project member", PrincipalDgraphUID: uid}

	case ResourceChat:
		// Participation in the grouping, via the same dm_is_member predicate every
		// other chat check in this codebase uses.
		//
		// This is the one branch that turns an IMPLICIT refusal into an explicit one.
		// The in-app summarize path enforces access by passing the caller's accessible
		// grouping ids into the search's permission filter, so a non-participant
		// receives an empty summary. Safe, but unanswerable: "no messages" and "not
		// allowed" are indistinguishable, so nothing can be said to the caller and
		// nothing lands in the audit trail as a refusal. Here it is a decision.
		participates, cerr := chatBusiness.ParticipatesInGrouping(ctx, strings.TrimSpace(ref.ID), uid)
		if cerr != nil {
			return ReachDecision{Reason: "conversation could not be read"}
		}
		if !participates {
			// A nonexistent grouping and someone else's grouping give the same
			// answer. Telling them apart would let a caller enumerate conversations
			// by probing ids.
			return ReachDecision{Reason: "the originating person is not a participant in this conversation"}
		}
		// Read and write share the rule here, and that is a decision rather than an
		// oversight: a conversation has no roles, so being in it is what permits both
		// reading and posting. A private channel has moderators and a doc has an
		// editing list; a group chat has neither.
		//
		// AND A GROUP SEND NEEDS NOTHING FURTHER, which took working through to be sure
		// of. This branch used to say a send must also verify the recipient. That is
		// true of a DM, which names a PERSON — and it now has its own kind for exactly
		// that reason. A group send names a ROOM the caller is already shown to be in,
		// so there is no per-recipient decision to make, the same as posting to a
		// channel. The participant list the send uses is read by a query that already
		// excludes external identities, so an unreachable ghost cannot be introduced by
		// posting.
		if !ref.Access.IsRead() {
			return ReachDecision{Allow: true, Reason: "conversation participant may post", PrincipalDgraphUID: uid}
		}
		return ReachDecision{Allow: true, Reason: "conversation participant", PrincipalDgraphUID: uid}

	case ResourceSelfOwned:
		// NO PER-OBJECT CHECK, because there is no object. The tool creates something
		// that will belong to the person who asked — a document they own, a reminder for
		// themselves — so the authority is that person being a live, eligible member,
		// which the gate at the top of this function has already established.
		//
		// WRITE IS THE ONLY THING THIS KIND IS FOR, and it is allowed rather than
		// refused. That is the opposite of ResourceWorkspace, deliberately: a
		// workspace-scoped tool that writes must name what it changes, and these change
		// nothing — they add something new that did not exist. Both branches are written
		// out so the decision is visible, and a read is allowed for the same reason a
		// write is, though no tool currently uses this kind to read.
		//
		// WHAT STILL APPLIES: admission, the agent kill switch, the agent's declared
		// toolset and confinement, the token scope, the call budget, the deduplication
		// key, and the audit row. This kind is weaker than every other one by exactly one
		// check — the one that cannot exist here — and no more than that.
		if !ref.Access.IsRead() {
			return ReachDecision{Allow: true, Reason: "creating an object owned by the originating person", PrincipalDgraphUID: uid}
		}
		return ReachDecision{Allow: true, Reason: "the originating person's own objects", PrincipalDgraphUID: uid}

	case ResourceTeam:
		// The SAME lookup executeCreateProject uses, checked the same way, so the two
		// cannot drift about who may create work in a team.
		team, terr := teamBusiness.GetBasicDgraphTeamInfoByUUID(ctx, strings.TrimSpace(ref.ID), uid)
		if terr != nil || team == nil || strings.TrimSpace(team.Uuid) == "" {
			return ReachDecision{Reason: "team could not be read"}
		}
		// DELETING A TEAM LEAVES ITS ADMIN EDGES IN PLACE, so the counts below still
		// report an admin of a team nobody can open — and a project created in one would
		// be orphaned on arrival. The lookup did not select this timestamp until this
		// change, which is why nothing checked it: the in-app create path had the same
		// hole and is fixed alongside.
		if helpers.IsSoftDeleted(team.DeletedAt) {
			return ReachDecision{Reason: "this team has been deleted"}
		}
		if team.IsMember <= 0 && team.IsAdmin <= 0 {
			return ReachDecision{Reason: "the originating person is not a member of this team"}
		}
		// WRITE REQUIRES TEAM ADMIN, which is the rule executeCreateProject applies:
		// belonging to a team does not carry the right to create projects in it.
		if !ref.Access.IsRead() && team.IsAdmin <= 0 {
			return ReachDecision{Reason: "creating in this team requires being one of its admins"}
		}
		return ReachDecision{Allow: true, Reason: "team member", PrincipalDgraphUID: uid}

	case ResourceDataSource:
		sourceID, serr := uuid.Parse(strings.TrimSpace(ref.ID))
		if serr != nil {
			return ReachDecision{Reason: "data source id is not a uuid"}
		}
		// WRITE IS REFUSED OUTRIGHT, before any lookup. The connectors are read-only by
		// construction and no tool on this surface offers a write, so a write request
		// against an external source is either a mistake or an attempt — and inventing an
		// authority rule for an operation that cannot happen would be the wrong way to
		// find out which.
		if !ref.Access.IsRead() {
			return ReachDecision{Reason: "external data sources are read-only on this surface"}
		}
		// THE ACTOR IS A POSTGRES ACTOR, for the same reason ResourceTable's is: the rule
		// tests an admin flag whose authoritative copy lives in Postgres, and the graph's
		// copy can be stale.
		actor, aerr := dataSourceActorFor(ctx, principal)
		if aerr != nil {
			return ReachDecision{Reason: "the originating person could not be resolved"}
		}
		// One delegated question, one answer. CanQuery is the same authority every shipped
		// query operation applies, including the disabled check — so a source an admin has
		// turned off is refused here rather than failing inside the tool, and a
		// soft-deleted source is already absent because its lookup filters deleted_at.
		if cerr := dataSourceBusiness.CanQuery(ctx, sourceID, actor); cerr != nil {
			// Forbidden and not-found collapse deliberately: telling a caller apart "this
			// source exists but is not yours" from "no such source" is a probe for which
			// warehouses a workspace has connected.
			return ReachDecision{Reason: "no access to this data source"}
		}
		return ReachDecision{Allow: true, Reason: "data source is queryable by this person", PrincipalDgraphUID: uid}

	case ResourceDirectMessage:
		// THE CALLER'S SIDE IS NOT A QUESTION. A DM's grouping id is derived from its
		// two participants, so naming the other person names the one conversation the
		// caller is in with them. There is no id they could supply that reaches a
		// conversation they are not part of, which is why this branch asks only about
		// the recipient.
		// The same lookup the principal resolve above uses, and the same one both
		// shipped send paths use — it is the only user query that selects is_bot and
		// is_external, which this rule needs.
		recipient, rerr := userDomain.GetDgraphUserInfoByUUID(ctx, strings.TrimSpace(ref.ID))
		if rerr != nil {
			return ReachDecision{Reason: "the recipient could not be read"}
		}
		// The SHIPPED rule, from the one function the HTTP controller and the in-app
		// executor also call: resolvable, not deactivated, and not an attribution-only
		// ghost — with bots deliberately permitted, because the automation bot is
		// is_external by class and a DM to it is answered by the AI coworker.
		//
		// Note this is NOT principalBusiness.Assess. That answers who may AUTHORIZE
		// work and refuses bots; this answers who may RECEIVE a message and does not.
		// The two differ on a real case and so are two functions.
		if e := principalBusiness.CanReceiveDirectMessage(recipient); !e.Allowed {
			return ReachDecision{Reason: e.Reason}
		}
		// READ AND WRITE SHARE THE RULE, stated as a branch rather than left implicit
		// because silence here would mean a write was authorised by a read rule without
		// anyone having decided that.
		//
		// They share it because a two-person conversation has no roles: there is no
		// moderator, no editing list, nothing that distinguishes who may look from who
		// may speak. And the recipient check above is if anything a WRITE rule already —
		// whether someone can be written to is precisely what it asks — so applying it
		// to reads is the stricter direction, not the looser one.
		if !ref.Access.IsRead() {
			return ReachDecision{Allow: true, Reason: "direct message with a reachable person may be written to", PrincipalDgraphUID: uid}
		}
		return ReachDecision{Allow: true, Reason: "direct message with a reachable person", PrincipalDgraphUID: uid}

	case ResourceTable:
		tableID, terr := uuid.Parse(strings.TrimSpace(ref.ID))
		if terr != nil {
			return ReachDecision{Reason: "table id is not a uuid"}
		}
		// THE ACTOR FOR THIS KIND IS A POSTGRES ACTOR, and that is not incidental.
		// A table's rule tests IsAdmin, and the authoritative admin flag lives in
		// Postgres — VerifyAuth and VerifyApiToken both overwrite the graph's copy
		// with it, which means the graph's copy can be stale. Using dgraphUser.IsAdmin
		// here would be a permission decision made from a value nothing keeps current.
		actor, aerr := tableActorFor(ctx, principal)
		if aerr != nil {
			return ReachDecision{Reason: "the originating person could not be resolved"}
		}
		// One delegated question, one answer. CanView is the same check the app and
		// the in-app read_table tool apply, and it treats a soft-deleted table as
		// absent — so an archived table is refused without a separate liveness test.
		// dataTableBusiness already separates these two questions as canView and
		// canManage, so the access dimension maps onto a distinction that package
		// makes itself rather than one invented here.
		check := dataTableBusiness.CanView
		if !ref.Access.IsRead() {
			check = dataTableBusiness.CanManage
		}
		if cerr := check(ctx, tableID, actor); cerr != nil {
			// Forbidden and not-found are collapsed deliberately. Telling a caller
			// apart "this table exists but is not yours" from "no such table" is a
			// probe for which tables exist.
			return ReachDecision{Reason: "no access to this table"}
		}
		return ReachDecision{Allow: true, Reason: "table access granted", PrincipalDgraphUID: uid}

	case ResourceProject:
		// The SAME lookup executeReadProject uses, checked the same way. A second
		// query answering "is this person in this project" is a second answer the
		// first time one of them is edited.
		proj, perr := projectBusiness.GetBasicDgraphProjectInfo(ctx, strings.TrimSpace(ref.ID), uid)
		if perr != nil || proj == nil || strings.TrimSpace(proj.Uuid) == "" {
			return ReachDecision{Reason: "project could not be read"}
		}
		// Archiving a project does not clear its member edges, so the count below
		// still reports a member of a project nobody can open.
		if helpers.IsSoftDeleted(proj.DeletedAt) {
			return ReachDecision{Reason: "this project has been deleted"}
		}
		if proj.IsProjectMember <= 0 {
			return ReachDecision{Reason: "the originating person is not a member of this project"}
		}
		// WRITE requires project ADMIN, which is the rule the shipped controllers
		// apply to project and task mutations (see controllers/Task/taskController.go,
		// where creating, reassigning and bulk-changing tasks all test
		// IsProjectAdmin). Membership grants sight of the work; changing it does not
		// follow from being able to see it.
		if !ref.Access.IsRead() && proj.IsProjectAdmin <= 0 {
			return ReachDecision{Reason: "changing work in this project requires being a project admin"}
		}
		return ReachDecision{Allow: true, Reason: "project member", PrincipalDgraphUID: uid}

	case ResourceDoc:
		doc, derr := docDomain.GetDocPermissions(ctx, strings.TrimSpace(ref.ID))
		if derr != nil || doc == nil {
			return ReachDecision{Reason: "doc could not be read"}
		}
		// A deleted doc keeps its reader/editor/commenter grants, so the grant
		// check below would still pass for a doc that has been removed.
		if helpers.IsSoftDeleted(doc.DeletedAt) {
			return ReachDecision{Reason: "this doc has been deleted"}
		}
		// A non-private doc is visible workspace-wide; a private one requires an
		// explicit grant. IsPrivate is a pointer, and a nil value is indeterminate —
		// treated as private, because guessing "public" on a doc whose privacy is
		// unknown is the one wrong direction to guess.
		// WRITE is decided first and on its own terms, because it does NOT follow from
		// visibility. A non-private doc is readable workspace-wide, and that says
		// nothing about who may edit it — so falling through to the privacy check
		// below would let anyone rewrite any public document.
		if !ref.Access.IsRead() {
			if !docGrantsEdit(doc, principal) {
				return ReachDecision{Reason: "the originating person may not edit this doc"}
			}
			return ReachDecision{Allow: true, Reason: "doc editor", PrincipalDgraphUID: uid}
		}
		if doc.IsPrivate == nil || *doc.IsPrivate {
			if !docGrantsAccess(doc, principal) {
				return ReachDecision{Reason: "the originating person has no grant on this private doc"}
			}
			return ReachDecision{Allow: true, Reason: "explicit grant on private doc", PrincipalDgraphUID: uid}
		}
		return ReachDecision{Allow: true, Reason: "workspace-visible doc", PrincipalDgraphUID: uid}

	default:
		// A kind nobody has written an authority rule for. Refusing here is what
		// makes ResourceKind's closedness meaningful.
		return ReachDecision{Reason: "unsupported resource kind"}
	}
}

// docGrantsEdit reports whether principalUUID may CHANGE a doc.
//
// Only the editing list and the creator. Deliberately NOT the reading or commenting
// lists — being able to read something, or to comment on it, is exactly the case
// where "may see" must not imply "may change" — and deliberately not affected by the
// doc being non-private, because a workspace-visible doc is readable by everyone and
// that is not a licence for everyone to rewrite it.
//
// A commenter is a real edge case worth naming: they can add to the conversation
// around a doc without being able to alter the doc, so they belong in
// docGrantsAccess and not here.
func docGrantsEdit(doc *dgraphStruct.DgraphDoc, principalUUID string) bool {
	if doc == nil {
		return false
	}
	principalUUID = strings.TrimSpace(principalUUID)
	if principalUUID == "" {
		return false
	}
	if doc.CreatedBy != nil && strings.EqualFold(strings.TrimSpace(doc.CreatedBy.Uuid), principalUUID) {
		return true
	}
	for _, u := range doc.EditingUser {
		if u == nil {
			continue
		}
		if candidate := strings.TrimSpace(u.Uuid); candidate != "" && strings.EqualFold(candidate, principalUUID) {
			return true
		}
	}
	return false
}

// docGrantsAccess reports whether principalUUID may see a doc.
//
// THE CREATOR COUNTS, and leaving them out was a bug. Reader, editor and commenter
// all imply "may see it", which is the question a read tool asks — but so does
// having created the thing. Nothing guarantees a doc's author also appears in its
// own reading list, so checking only the three grant lists refuses an author access
// to their own private document.
//
// Found by comparing this against the permission gate in executeReadDoc, which the
// in-app path has always applied:
//
//	hasAccess := !isPrivate || isCreator || HasReadAccess > 0 || HasEditAccess > 0 || HasCommentAccess > 0
//
// Two rules for one question is how they end up disagreeing, and here they did. The
// in-app one was right.
//
// A write tool must additionally check the editing list; "may see" is not "may
// change", and this function answers only the first.
func docGrantsAccess(doc *dgraphStruct.DgraphDoc, principalUUID string) bool {
	if doc == nil {
		return false
	}
	principalUUID = strings.TrimSpace(principalUUID)
	// TWO UNKNOWNS ARE NOT A MATCH. Every comparison below is string equality, and
	// an empty principal would equal an empty uuid on a partially populated doc —
	// so an unresolved caller would inherit access to any doc whose creator or grant
	// entry came back blank. PrincipalCanReach already refuses an empty principal
	// before reaching here, but an authorization primitive should not be safe only
	// because of what currently calls it.
	if principalUUID == "" {
		return false
	}
	if doc.CreatedBy != nil && strings.EqualFold(strings.TrimSpace(doc.CreatedBy.Uuid), principalUUID) {
		return true
	}
	for _, group := range [][]*dgraphStruct.DgraphUser{doc.ReadingUser, doc.EditingUser, doc.CommentingUser} {
		for _, u := range group {
			if u == nil {
				continue
			}
			if candidate := strings.TrimSpace(u.Uuid); candidate != "" && strings.EqualFold(candidate, principalUUID) {
				return true
			}
		}
	}
	return false
}

// tableActorFor builds the Postgres-side actor a table's visibility rule needs.
//
// SEPARATE FROM THE GRAPH LOOKUP ON PURPOSE. Most resource kinds here answer a
// membership question, which lives in the graph. Tables answer an ownership-and-
// visibility question that tests an admin flag, and the authoritative admin flag is
// in Postgres — both auth middlewares overwrite the graph's copy with it, which is
// the tell that the graph's copy is derived rather than current.
//
// Using GetActiveUserWithAdminFlagByUserUUID rather than a plain fetch adds a second
// benefit for free: it filters deleted_at, so a deactivated person cannot pass this
// even if the eligibility gate above were ever bypassed. Two independent refusals
// for the same condition is the right amount for an authorization path.
// dataSourceActorFor resolves the Postgres actor a data-source rule needs.
//
// A separate function from tableActorFor only because the two packages declare their own
// Actor types. The resolution is identical and deliberately shares tableActorFor's
// lookup, so the admin flag both rules test comes from one place.
func dataSourceActorFor(ctx context.Context, principalUserID string) (dataSourceBusiness.Actor, error) {
	a, err := tableActorFor(ctx, principalUserID)
	if err != nil {
		return dataSourceBusiness.Actor{}, err
	}
	return dataSourceBusiness.Actor{UserID: a.UserID, IsAdmin: a.IsAdmin}, nil
}

func tableActorFor(ctx context.Context, principalUserID string) (dataTableBusiness.Actor, error) {
	userUUID, err := uuid.Parse(strings.TrimSpace(principalUserID))
	if err != nil {
		return dataTableBusiness.Actor{}, err
	}
	row, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, userUUID)
	if err != nil {
		return dataTableBusiness.Actor{}, err
	}
	if row == nil || row.Id == (uuid.UUID{}) {
		return dataTableBusiness.Actor{}, errPrincipalNotFound
	}
	return dataTableBusiness.Actor{UserID: row.Id, IsAdmin: row.IsAdmin}, nil
}

// errPrincipalNotFound is returned when a principal resolves to no active row.
var errPrincipalNotFound = errors.New("mcp: the originating person has no active account")
