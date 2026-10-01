package codepr

// credential.go — WHICH GitHub identity a coding run pushes with, and why the
// admin's integration is deliberately NOT it.
//
// ── The three GitHub credentials OneCamp holds ─────────────────────────────
//
//  1. The ADMIN INTEGRATION (business/GitHub). One row for the whole workspace:
//     (entity_type=org, entity_id=nil, provider=github). It is an OAuth APP
//     token — client id/secret, scope "repo,read:org" — minted when an admin
//     clicks Connect in the Integrations tab. So it is a PERSON's token: it
//     belongs to that admin's GitHub account. It powers issue/PR import, task
//     sync, webhooks, check runs and repo-access reads.
//
//  2. The per-user CONNECTOR (business/Connector, provider "github_connector").
//     Keyed by the connecting user, scope "repo read:user read:org
//     notifications", consented explicitly for AI use, revocable on its own.
//
//  3. A GitHub MCP server (business/AIMCP). An admin-registered endpoint with its
//     own encrypted bearer secret, giving the model GitHub API tools behind the
//     protected-branch guard and the confirmation gate. It has no checkout and no
//     build, so it cannot author a verified change.
//
// ── Why the admin integration must not push code ───────────────────────────
//
// It was the credential this feature used, and every one of these is a
// consequence of that being a human admin's broad OAuth token:
//
//   - ATTRIBUTION IS FALSE. Every commit and pull request the agent produced was
//     authored by that admin's GitHub identity. A person who never saw the code
//     becomes its author in git history, which defeats review (you cannot tell
//     agent work from theirs), lets them approve their "own" pull request under
//     CODEOWNERS, and corrupts the change-management evidence that a governed
//     coding agent exists to produce. For a product whose whole claim is
//     auditable agent attribution, this is the opposite of the claim.
//
//   - BLAST RADIUS IS THAT ADMIN'S WHOLE GITHUB ACCOUNT. Classic "repo" scope on
//     a personal account reaches every private repository they can see, in every
//     organisation they belong to — including orgs and side projects unrelated to
//     this workspace. The sandbox executes model-authored code with github.com
//     reachable, so that is also the exfiltration surface.
//
//   - CONSENT DOES NOT COVER IT. The admin authorised GitHub to import issues and
//     sync tasks. Nothing in that flow says an autonomous agent will push branches
//     as them. Reusing the credential for a materially different purpose is a
//     consent violation whatever the scopes technically permit.
//
//   - PRIVILEGE ESCALATION. Task.OwnerUserID is documented as "permissions the run
//     uses" and the durable worker populates it, but the GitHub path ignored it:
//     both the access check and the push used this shared token. Any member who
//     could task the agent therefore inherited the admin's entire reach, including
//     repositories they had no access to and could not have opened themselves.
//
//   - IT CANNOT BE NARROWED. One shared row serves sync, webhooks and imports, so
//     its scopes cannot be reduced for coding without breaking those.
//
//   - IT COUPLES UNRELATED CRITICALITY. The admin leaving, rotating, or revoking
//     the grant takes down issue sync and every coding run together.
//
// ── The rule ───────────────────────────────────────────────────────────────
//
//  1. The run has a human principal and that person has connected their own
//     GitHub connector, and their token can reach the target repository ⇒ push
//     with THEIR credential. GitHub enforces the boundary, so escalation is
//     impossible rather than merely checked; the pull request carries the
//     identity of the person who actually asked for the change; and the grant is
//     revocable without touching issue sync.
//
//  2. Otherwise ⇒ BLOCK, and say what to do. There is deliberately no fallback to
//     the admin integration: falling back to a broad human credential is the exact
//     failure being removed, and a silent fallback would mean the safe path is
//     never the one that actually runs.
//
// A repository-scoped GitHub App installation token is the right primary
// credential for unattended runs — short-lived, limited to the repositories an
// admin installed it on, and attributed to a bot rather than a person. That
// machinery does not exist yet (there is no app id / private key / JWT path in
// business/GitHub; github_links.installation_id is stored but never used for
// auth), so it is not referenced here rather than stubbed. Until it lands, an
// unattended run without a connected principal is blocked instead of quietly
// borrowing an admin's identity.
//
// Everything here is pure or injected, so the precedence is unit-tested rather
// than inferred from production behaviour.

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// CredentialKind names which GitHub identity a run acted with. Recorded on the
// run and disclosed in the pull request, because "who was this done as" is not
// answerable after the fact otherwise.
type CredentialKind string

const (
	// CredentialUserConnector is the requesting person's own GitHub connector:
	// least privilege, honestly attributed, independently revocable.
	CredentialUserConnector CredentialKind = "user_connector"
	// CredentialNone means no usable credential was found and the run must block.
	CredentialNone CredentialKind = ""
)

// Credential is the resolved identity a run will clone and push with. Token is
// never logged and never recorded.
type Credential struct {
	Token  string
	Kind   CredentialKind
	UserID uuid.UUID // the person, when Kind is CredentialUserConnector
	// BlockedReason explains an unusable credential in terms the requester can
	// act on. "You have no GitHub connection", "you can see that repository but
	// cannot push to it" and "that repository is archived" need three different
	// answers — collapsing them into one generic refusal leaves the person with no
	// idea which thing to fix.
	BlockedReason string
	// DefaultBranch is the repository's default branch as reported during the
	// access check, so a task that named no base branch does not have to guess.
	DefaultBranch string
}

// RepoWriteCheck is what an identity may actually do with the target repository.
// A coding run must PUSH a branch and open a pull request, so visibility alone is
// not authorisation — see the failure described on RepoAccess in business/GitHub.
type RepoWriteCheck struct {
	Visible       bool
	CanPush       bool
	Archived      bool
	DefaultBranch string
}

// Usable reports whether the credential can actually be used.
func (c Credential) Usable() bool {
	return strings.TrimSpace(c.Token) != "" && c.Kind != CredentialNone
}

// BlockedMessage is what a human is told when no usable credential exists: the
// specific reason when one was determined, otherwise a safe generic fallback. It
// always names an action, because the honest alternative to a silent credential
// fallback is a clear instruction.
func (c Credential) BlockedMessage() string {
	if r := strings.TrimSpace(c.BlockedReason); r != "" {
		return r
	}
	return reasonNotConnected
}

// CredentialSources are the lookups ChooseCredential needs. Injected so the
// precedence is testable without a database or GitHub.
type CredentialSources struct {
	// UserToken returns the person's own connector token, or "" when they have
	// not connected GitHub.
	UserToken func(ctx context.Context, userID uuid.UUID) (string, error)
	// UserCanPushToRepo checks what the person's token may DO with the target
	// repository. Bound to the SAME token that will push, so the check proves
	// something: a check made with one identity and a push made with another
	// proves nothing. It must establish WRITE access, not visibility — otherwise a
	// read-only collaborator passes the gate and the run burns its entire budget
	// before failing on the push.
	UserCanPushToRepo func(ctx context.Context, token string, repo RepoRef) (RepoWriteCheck, error)
}

// ChooseCredential applies the rule above. It never returns an error: an unusable
// result is a Credential with CredentialNone, which the orchestrator turns into an
// honest block rather than a stack trace.
//
// repo may be zero-valued when the target is not yet known; the reach check is
// then skipped and the decision rests on connection alone, so a caller that
// resolves the repository afterwards must re-verify (VerifyChosen does that).
func ChooseCredential(ctx context.Context, task Task, repo RepoRef, src CredentialSources) Credential {
	principal := credentialPrincipal(task)
	if principal == uuid.Nil {
		return Credential{Kind: CredentialNone, BlockedReason: reasonNoPrincipal}
	}
	if src.UserToken == nil {
		return Credential{Kind: CredentialNone, BlockedReason: reasonNotConnected}
	}
	token, err := src.UserToken(ctx, principal)
	if err != nil || strings.TrimSpace(token) == "" {
		return Credential{Kind: CredentialNone, BlockedReason: reasonNotConnected}
	}
	cred := Credential{Token: token, Kind: CredentialUserConnector, UserID: principal}
	if repo.Valid() && src.UserCanPushToRepo != nil {
		return applyWriteCheck(ctx, cred, repo, src)
	}
	return cred
}

// applyWriteCheck confirms the chosen identity may actually push to repo, and
// turns each distinct failure into its own actionable refusal.
func applyWriteCheck(ctx context.Context, cred Credential, repo RepoRef, src CredentialSources) Credential {
	chk, err := src.UserCanPushToRepo(ctx, cred.Token, repo)
	switch {
	case err != nil:
		// Unconfirmable is NOT authorised. We do not push as someone on the
		// assumption that they probably had access.
		return Credential{Kind: CredentialNone, BlockedReason: "I couldn't confirm your access to " + repo.FullName() + " just now, so I stopped rather than assume it. Try again shortly."}
	case !chk.Visible:
		return Credential{Kind: CredentialNone, BlockedReason: "I can't see " + repo.FullName() + " with your GitHub account. Check the name, or ask for access to it."}
	case chk.Archived:
		return Credential{Kind: CredentialNone, BlockedReason: repo.FullName() + " is archived on GitHub, so nobody can push to it. It would need to be unarchived first."}
	case !chk.CanPush:
		// The case the visibility-only check used to let through, at the cost of a
		// whole run's time and token budget.
		return Credential{Kind: CredentialNone, BlockedReason: "You have read access to " + repo.FullName() + " but not write access, so I can't push a branch or open a pull request there. Ask for write access on the repository and I'll try again."}
	}
	cred.DefaultBranch = chk.DefaultBranch
	return cred
}

// credentialPrincipal is the person a run acts as. OwnerUserID is the run-as
// principal and is authoritative; TriggeredBy is only "who to notify" and is
// deliberately NOT used as an identity — a schedule-triggered run has no
// principal and must not borrow a bystander's credential to get one.
func credentialPrincipal(task Task) uuid.UUID { return task.OwnerUserID }

// The distinct reasons a run has no identity to push with. Separate constants
// because each one has a different fix.
const (
	reasonNoPrincipal = "This run has no person to act as (it was started by a schedule or an event), " +
		"and I won't push code using the workspace's shared admin GitHub connection. " +
		"Ask me directly and I'll use your own GitHub connection, or have an admin set up a repository-scoped " +
		"GitHub App for unattended runs."
	reasonNotConnected = "You haven't connected your GitHub account yet. " +
		"Connect it under Settings → Connectors and ask me again — I'll then work only in repositories you already " +
		"have access to, and the pull request will be attributed to you. " +
		"(I deliberately don't reuse the workspace's admin GitHub connection for writing code: it belongs to one " +
		"admin, reaches every repository they can see, and would credit them as the author of code they didn't write.)"
)
