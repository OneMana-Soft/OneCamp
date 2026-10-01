package business

// Ratchets on PrincipalCanReach.
//
// Every check in reach.go needs a live Dgraph to exercise for real, and there is an
// integration test that does exactly that. These are the complementary guards: they
// assert STRUCTURAL properties of the function that no unit test can observe from
// outside and that an integration test would only catch for the specific resource
// kind it happens to cover.
//
// They are deliberately narrow. Each one names the failure it prevents, so a
// breakage reads as a design violation rather than as a broken assertion.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// reachSource returns reach.go with comments stripped, so an assertion cannot be
// satisfied by prose that merely mentions the right identifier.
func reachSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("reach.go")
	if err != nil {
		t.Fatalf("read reach.go: %v", err)
	}
	return regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")
}

// kindsDeclaredInSpec finds every ResourceKind constant, so this test discovers new
// kinds by itself. A ratchet that has to be updated by hand to cover a new case is
// not a ratchet.
func kindsDeclaredInSpec(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("spec.go")
	if err != nil {
		t.Fatalf("read spec.go: %v", err)
	}
	matches := regexp.MustCompile(`(?m)^\s*(Resource\w+)\s+ResourceKind\s*=`).FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatal("found no ResourceKind constants; this test has gone stale")
	}
	kinds := make([]string, 0, len(matches))
	for _, m := range matches {
		kinds = append(kinds, m[1])
	}
	return kinds
}

// A ResourceKind with no branch falls to `default`, which denies — so the failure
// mode is a refusal, not a leak. That is the safe direction, but it is still a bug:
// a tool declaring that kind is unusable, and the reason a caller gets back is
// "unsupported resource kind" rather than anything actionable.
//
// The point of this test is that adding a kind to spec.go without teaching reach.go
// what authority means for it is caught at build time, by the author, instead of
// being discovered by whoever ships the first tool that uses it.
func TestEveryResourceKindHasAnAuthorityRule(t *testing.T) {
	src := reachSource(t)
	for _, kind := range kindsDeclaredInSpec(t) {
		if !strings.Contains(src, "case "+kind) {
			t.Errorf("ResourceKind %s has no branch in PrincipalCanReach, so every tool "+
				"declaring it is refused with \"unsupported resource kind\". Add a branch "+
				"stating what authority over this kind means, even if the answer is a denial.", kind)
		}
	}
}

// Membership does not expire when the thing you are a member of is deleted.
//
// Deleting a channel, a doc, a task or a project writes a timestamp; it does not
// walk the graph removing member edges or access grants. So `IsMember > 0` and
// `docGrantsAccess` both keep answering yes about objects that no longer exist,
// and a tool would happily read a deleted doc for someone who still appears on its
// reader list.
//
// Asserted per branch rather than by counting, so the error names the resource kind
// that stopped checking.
func TestEveryResourceBranchChecksLiveness(t *testing.T) {
	src := reachSource(t)

	// Exemptions, each with the reason it is one. The requirement this test encodes
	// is "liveness must be ACCOUNTED FOR", not "this exact function must appear" —
	// a branch that delegates to a lookup which already excludes deleted rows has
	// accounted for it, and adding a redundant check there would be code written to
	// satisfy a test rather than a requirement.
	//
	// Stated per kind rather than inferred, so an exemption is a decision someone
	// wrote down. A new kind is never exempt by default.
	exempt := map[string]string{
		"ResourceWorkspace": "identifies no single object, so there is nothing whose " +
			"deletion could be missed; its authority is the principal being a live person, " +
			"which the eligibility gate has already established",
		"ResourceTable": "delegates to dataTableBusiness.CanView, whose GetTableByID " +
			"filters `deleted_at IS NULL` — a soft-deleted table is indistinguishable " +
			"from one that never existed, so the refusal happens at the lookup",
		"ResourceChat": "a grouping is not soft-deleted as a unit — DgraphDm carries no " +
			"deleted_at, because individual chats are deleted rather than the conversation. " +
			"There is no timestamp on this object to check; a grouping with no participants " +
			"simply fails the participation test",
		"ResourceSelfOwned": "identifies no object — it creates one that does not exist " +
			"yet and will belong to the caller — so there is nothing whose deletion could " +
			"be missed. Same reasoning as ResourceWorkspace, and both are covered by " +
			"ResourceKind.IdentifiesNoObject",
		"ResourceDataSource": "delegates to dataSourceBusiness.CanQuery, whose GetByID " +
			"filters `deleted_at IS NULL` — a soft-deleted source is indistinguishable from " +
			"one that never existed, so the refusal happens at the lookup. The same shape as " +
			"ResourceTable, and collapsing the two cases is deliberate: telling them apart " +
			"would let a caller probe which warehouses a workspace has connected",
		"ResourceDirectMessage": "the only object here is the RECIPIENT, and their liveness " +
			"is checked — inside principalBusiness.CanReceiveDirectMessage, which calls " +
			"helpers.IsSoftDeleted on them. The conversation itself has no deleted_at: a DM's " +
			"grouping is derived from its two participants rather than created and deleted. " +
			"Repeating the check inline would be a second copy of the rule this branch " +
			"deliberately shares with the HTTP controller and the in-app executor",
	}

	for _, kind := range kindsDeclaredInSpec(t) {
		if reason, ok := exempt[kind]; ok {
			t.Logf("%s is exempt from an inline liveness check: %s", kind, reason)
			continue
		}
		start := strings.Index(src, "case "+kind)
		if start < 0 {
			continue // already reported by TestEveryResourceKindHasAnAuthorityRule
		}
		// The branch runs to the next case, or to the default arm.
		rest := src[start+len("case "+kind):]
		end := len(rest)
		for _, boundary := range []string{"\n\tcase ", "\n\tdefault:"} {
			if i := strings.Index(rest, boundary); i >= 0 && i < end {
				end = i
			}
		}
		branch := rest[:end]

		if !strings.Contains(branch, "IsSoftDeleted(") {
			t.Errorf("the %s branch of PrincipalCanReach never calls helpers.IsSoftDeleted, "+
				"so a deleted %s still grants reach to anyone whose membership or grant "+
				"survived the deletion", kind, strings.TrimPrefix(kind, "Resource"))
		}
	}
}

// The eligibility gate has to run before any membership question, and the ordering
// is the whole point: membership is asked ABOUT a person, so establishing that the
// person may act at all is a precondition, not an equal-ranked check.
//
// If a resource branch ran first, a deactivated employee's agent would be refused
// only for objects they had also been removed from — which is the same as saying
// offboarding does nothing on its own.
func TestEligibilityIsCheckedBeforeAnyResourceBranch(t *testing.T) {
	src := reachSource(t)

	gate := strings.Index(src, "principalBusiness.Assess(")
	if gate < 0 {
		t.Fatal("PrincipalCanReach no longer calls principalBusiness.Assess; a deactivated, " +
			"bot or attribution-only identity can authorize MCP calls again")
	}

	firstBranch := len(src)
	for _, kind := range kindsDeclaredInSpec(t) {
		if i := strings.Index(src, "case "+kind); i >= 0 && i < firstBranch {
			firstBranch = i
		}
	}
	if gate > firstBranch {
		t.Error("principalBusiness.Assess runs after the first resource branch; whether an " +
			"identity may act at all must be settled before asking what it may reach")
	}
}

// Guests are excluded from this surface by a foreign key, not by a check, and that
// is worth pinning: it is the strongest kind of exclusion and the easiest to lose.
//
// A guest grant is deliberately never written to the users table, so a guest has no
// row that api_tokens.created_by could point at. That means no guest can ever be a
// principal here — matching the guest-access rule that a guest gets no workspace
// session and no API access.
//
// If someone later gives guests a users row to simplify some join, this test fails
// and says what breaks: guests silently become eligible principals, and Assess does
// not currently look for them because it has never needed to.
func TestGuestsCannotBecomePrincipalsByConstruction(t *testing.T) {
	tokens, err := os.ReadFile("../../migrations/92_create_api_tokens.up.sql")
	if err != nil {
		t.Fatalf("read api_tokens migration: %v", err)
	}
	// Normalise whitespace so the assertion survives reformatting of the DDL.
	ddl := regexp.MustCompile(`\s+`).ReplaceAllString(string(tokens), " ")
	if !strings.Contains(ddl, `"created_by" uuid NOT NULL REFERENCES users(id)`) {
		t.Error("api_tokens.created_by is no longer a NOT NULL foreign key into users(id). " +
			"That FK is what makes it impossible for a non-member to be an MCP principal; " +
			"without it, guest access must become an explicit check in Principal.Assess")
	}

	grants, err := os.ReadFile("../../migrations/97_create_guest_grants.up.sql")
	if err != nil {
		t.Fatalf("read guest_grants migration: %v", err)
	}
	if !strings.Contains(string(grants), "Guests are NEVER written to the users table") {
		t.Error("the guest_grants schema no longer states that guests stay out of the users " +
			"table. If that invariant changed, guests can now hold api_tokens and this " +
			"surface needs an explicit guest refusal")
	}
}

// Every resource kind must DECIDE about writes.
//
// The dangerous default is silence. A branch that ignores Access answers a write with
// whatever its read rule says — so a doc anyone can read becomes a doc anyone can
// rewrite, and a project you can see becomes a project you can restructure. Both
// would be a permission escalation produced by omission rather than by a wrong rule,
// which is the kind nobody finds by reading the code.
//
// Deciding is allowed to mean "the same as read", as it does for conversations, which
// have no roles. What is not allowed is not deciding.
func TestEveryResourceBranchDecidesAboutWrites(t *testing.T) {
	src := reachSource(t)

	for _, kind := range kindsDeclaredInSpec(t) {
		start := strings.Index(src, "case "+kind)
		if start < 0 {
			continue // reported by TestEveryResourceKindHasAnAuthorityRule
		}
		rest := src[start+len("case "+kind):]
		end := len(rest)
		for _, boundary := range []string{"\n\tcase ", "\n\tdefault:"} {
			if i := strings.Index(rest, boundary); i >= 0 && i < end {
				end = i
			}
		}
		branch := rest[:end]

		if !strings.Contains(branch, "Access.IsRead()") {
			t.Errorf("the %s branch never consults ref.Access, so a WRITE to a %s is "+
				"authorised by its READ rule. Decide explicitly — even if the answer is "+
				"that they share a rule — because the failure mode of silence here is a "+
				"permission escalation that reads as ordinary code.",
				kind, strings.TrimPrefix(kind, "Resource"))
		}
	}
}

// Access must be DERIVED from the tool's behaviour, never taken from the resource
// reference the tool produced.
//
// If a Resource function could set it, the single field able to understate a call's
// intent would be set by the same author whose tool benefits from understating it. A
// tool that mutates could then present itself as a read and be checked against the
// weaker rule, and nothing else in the ladder would notice — the scope, the budget
// and the audit record would all be the ones a read gets.
func TestAccessIsDerivedFromBehaviourNotFromTheRef(t *testing.T) {
	raw, err := os.ReadFile("authorize.go")
	if err != nil {
		t.Fatalf("read authorize.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

	resolve := strings.Index(src, "spec.Resource(")
	if resolve < 0 {
		t.Fatal("authorize.go no longer resolves the resource; this test has gone stale")
	}
	assign := strings.Index(src, "ref.Access = ")
	if assign < 0 {
		t.Fatal("authorize.go never assigns ref.Access, so whatever a tool's Resource " +
			"function put there is what the permission check uses — including nothing at all")
	}
	if assign < resolve {
		t.Error("ref.Access is assigned before the resource is resolved, so the resolver's " +
			"value would overwrite the derived one")
	}
	if !strings.Contains(src, "spec.Behaviour.ReadOnly") {
		t.Error("ref.Access is not derived from spec.Behaviour.ReadOnly. Deriving it from " +
			"the same field the registry enforces and advertises is what stops a tool " +
			"claiming read access while performing a write")
	}
}

// The zero value must be a write.
//
// Any code path that forgets to set Access, and any typo in a literal, must get the
// STRICTER check. A permissive default would mean a forgotten assignment silently
// downgrades a mutation to a read check.
func TestUnsetAccessIsTreatedAsAWrite(t *testing.T) {
	var unset Access
	if unset.IsRead() {
		t.Fatal("the zero Access value reads as a read request; a forgotten assignment " +
			"must fail closed, not open")
	}
	for _, wrong := range []Access{"", "READ", "Read", " read", "read ", "reads", "write"} {
		if wrong.IsRead() {
			t.Errorf("Access(%q) reads as a read request; only the exact value must count, "+
				"so a near-miss fails closed", string(wrong))
		}
	}
	if !AccessRead.IsRead() {
		t.Fatal("AccessRead must read as a read request")
	}
}
