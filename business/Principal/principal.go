package business

// Principal eligibility — the one place that decides whether an IDENTITY may
// authorize work at all, before anyone asks what it may reach.
//
// THE DISTINCTION THIS PACKAGE EXISTS TO MAKE. Authorization in OneCamp is asked
// as two separate questions, and conflating them is how an offboarded employee
// keeps access:
//
//	1. Is this identity one that may authorize anything?   <- here
//	2. May it reach THIS object?                           <- membership lookups
//
// Question 2 is answered by walking the permission graph: channel membership,
// project membership, doc grants. Those lookups are correct and well tested. But
// they all begin by resolving the person, and resolving a person only proves the
// NODE exists — not that the human behind it is still employed, still able to log
// in, or a human at all. Every membership edge a deactivated user had on their last
// day is still in the graph the day after, because deactivation writes a timestamp
// and stops. So question 2 answers "yes, still a member" for someone who no longer
// works here, and without question 1 that is the final answer.
//
// WHY IT IS A SEPARATE PACKAGE. Two surfaces need this identical judgement:
//
//   - business/MCPServer   — the human whose api_token an external agent presents.
//   - business/AIAgent     — the human at the root of an in-workspace delegation
//     chain (originCanAddressSurface).
//
// Neither imports the other, and they must not diverge: an identity that cannot
// authorize an MCP call must not be able to authorize the same work by mentioning
// an agent in a channel instead. A shared function makes them agree by
// construction rather than by both remembering.
//
// WHY IT TAKES A STRUCT AND DOES NO I/O. Assess accepts a user that the caller has
// already fetched. Both call sites resolve the principal anyway to get their graph
// uid, and the flags this gate needs are already selected by that same query — so
// the check costs zero extra round-trips, cannot fail independently of the lookup
// it guards, and is a pure function that tests exhaustively without a database.

import (
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Eligibility is the verdict on one identity. Reason is populated on allow as well
// as deny, so an audit record and a client-facing refusal are produced from the
// same sentence and cannot disagree about why.
type Eligibility struct {
	Allowed bool
	Reason  string
}

// Assess reports whether u may authorize work.
//
// Deny-by-default: an identity is eligible only by falling through every check
// below. The order is deliberate — cheapest and most fundamental first, so the
// reason attached to a refusal is the most specific true statement about it.
func Assess(u *dgraphStruct.DgraphUser) Eligibility {
	// Unresolvable. Callers already treat a failed lookup as a refusal, but a nil
	// or uid-less struct reaching here means something upstream returned success
	// with nothing in it, and that must not read as eligible.
	if u == nil || strings.TrimSpace(u.Uid) == "" {
		return Eligibility{Reason: "the originating person could not be resolved"}
	}

	// DEACTIVATED — the case this package was written for.
	//
	// DeactivateUser sets deleted_at in Postgres and Dgraph. It does NOT remove
	// channel or project memberships, and it does NOT revoke api_tokens. Nothing
	// downstream noticed: the user lookup does not filter deleted_at, the token
	// row's foreign key cascades only on a HARD delete, and the membership queries
	// are asked about a node that still has all its edges.
	//
	// So without this check, offboarding an employee leaves every agent their token
	// authorises fully operational, and the "access is re-evaluated live on every
	// call" property is true for channel removal and false for the single event an
	// operator most expects it to cover.
	if helpers.IsSoftDeleted(u.DeletedAt) {
		return Eligibility{Reason: "the originating person's account is deactivated"}
	}

	// BOT. A bot is not accountable for anything: there is no person behind it to
	// have intended the work or to answer for it afterwards. Agent lineage already
	// carries the authorising human from the root of the chain precisely so a hop
	// is never credited to the bot that relayed it (see originLineage in
	// business/AIAgent/agentTriggers.go, whose comment names the failure: "every
	// chain would look like it was authorised by a bot").
	//
	// That invariant is currently maintained by careful assignment. This makes it
	// enforced, so a future path that starts a chain without lineage is refused
	// rather than quietly rooted at a bot with nobody behind it.
	if u.IsBot {
		return Eligibility{Reason: "a bot identity cannot authorize work; the accountable person is missing"}
	}

	// EXTERNAL / GHOST. is_external marks attribution-only identities — the OneCamp
	// AI bot, unmapped GitHub commit authors, imported Slack users with no mapped
	// account. They exist so history can name an author, and they CANNOT LOG IN.
	// An identity with no means of authenticating cannot have intended a call, so
	// it cannot be the authority for one.
	//
	// Guests are a different mechanism and are already excluded by construction,
	// not by this check: guest grants are never written to the users table, and
	// api_tokens.created_by is a foreign key INTO that table, so a guest has no
	// representation that could appear here at all.
	if u.IsExternal {
		return Eligibility{Reason: "an external attribution identity cannot authorize work; it has no way to sign in"}
	}

	return Eligibility{Allowed: true, Reason: "active workspace member"}
}

// CanReceiveDirectMessage reports whether an identity may be sent a direct message.
//
// A DIFFERENT QUESTION FROM Assess, and the difference is one flag. Assess asks who may
// AUTHORIZE work; this asks who may RECEIVE a message. They agree that an unresolvable
// or deactivated identity is out, and they disagree about bots — deliberately.
//
// A bot cannot authorize work, because there is no person behind it to have intended it
// or answer for it afterwards. But a bot is a perfectly good RECIPIENT: OneCamp's shared
// automation bot is is_external by class and is intentionally messageable, because a DM
// to it is answered by the AI coworker. Refusing bots here would break that feature,
// which is why this is its own function rather than a flag on Assess. Two questions that
// differ on a real case should not share one answer.
//
// THE RULE IS THE SHIPPED ONE, not a new one. controllers/Chat.CreateChat has enforced
// exactly this since DMs existed: the recipient must resolve, must not be soft-deleted,
// and is refused when is_external unless it is also is_bot. This function is that rule
// extracted so more than one caller can hold it.
//
// WHY IT HAD TO BE EXTRACTED. The rule was written out in the HTTP controller and
// PARTIALLY repeated in the AI executor, which checked resolution and soft-deletion and
// omitted the external test. So an agent could DM an attribution-only ghost identity
// that a person using the app was refused — a real divergence, invisible because both
// paths looked like they were checking the recipient. One function, three callers: the
// controller, the executor, and the MCP authorizer.
func CanReceiveDirectMessage(u *dgraphStruct.DgraphUser) Eligibility {
	if u == nil || strings.TrimSpace(u.Uid) == "" {
		return Eligibility{Reason: "the recipient could not be resolved"}
	}

	// Deactivated. Delivering to an account nobody can sign into produces a message
	// with no reader and a conversation that looks live.
	if helpers.IsSoftDeleted(u.DeletedAt) {
		return Eligibility{Reason: "the recipient's account is deactivated"}
	}

	// EXTERNAL, EXCEPT BOTS — the exception is the whole reason this differs from
	// Assess. is_external marks attribution-only identities that cannot sign in
	// (unmapped commit authors, imported users with no account), so a message to one
	// can never be read. The automation bot carries the same flag by class and IS
	// meant to be messageable.
	if u.IsExternal && !u.IsBot {
		return Eligibility{Reason: "an external attribution identity cannot receive messages; it has no way to sign in"}
	}

	return Eligibility{Allowed: true, Reason: "recipient can receive messages"}
}
