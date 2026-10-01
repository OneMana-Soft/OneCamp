package business

import (
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func liveUser() *dgraphStruct.DgraphUser {
	return &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "11111111-1111-1111-1111-111111111111"}
}

func TestAssessAllowsAnOrdinaryActiveMember(t *testing.T) {
	got := Assess(liveUser())
	if !got.Allowed {
		t.Fatalf("an ordinary active member must be eligible, got refusal: %s", got.Reason)
	}
	if strings.TrimSpace(got.Reason) == "" {
		t.Fatal("Reason must be populated on allow too, so audit and refusal text share one source")
	}
}

// Each refusal below is a distinct way an identity can be present in the graph and
// still have no business authorising anything.
func TestAssessRefusesIneligibleIdentities(t *testing.T) {
	deleted := time.Now()

	cases := []struct {
		name       string
		user       *dgraphStruct.DgraphUser
		wantReason string
		why        string
	}{
		{
			name:       "nil",
			user:       nil,
			wantReason: "could not be resolved",
			why:        "a nil user must never read as eligible",
		},
		{
			name:       "resolved but no graph uid",
			user:       &dgraphStruct.DgraphUser{Uuid: "abc"},
			wantReason: "could not be resolved",
			why:        "a struct with no uid means the lookup returned success with nothing in it",
		},
		{
			name:       "deactivated member",
			user:       &dgraphStruct.DgraphUser{Uid: "0x1", DeletedAt: &deleted},
			wantReason: "deactivated",
			why:        "DeactivateUser leaves every membership edge in place, so only this check offboards them",
		},
		{
			name:       "bot",
			user:       &dgraphStruct.DgraphUser{Uid: "0x1", IsBot: true},
			wantReason: "bot identity",
			why:        "a bot has no accountable person behind it",
		},
		{
			name:       "external ghost identity",
			user:       &dgraphStruct.DgraphUser{Uid: "0x1", IsExternal: true},
			wantReason: "external attribution identity",
			why:        "attribution-only identities cannot sign in, so cannot have intended a call",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Assess(c.user)
			if got.Allowed {
				t.Fatalf("expected refusal (%s), got allow with reason %q", c.why, got.Reason)
			}
			if !strings.Contains(got.Reason, c.wantReason) {
				t.Fatalf("reason = %q, want it to mention %q so the audit trail says which gate refused", got.Reason, c.wantReason)
			}
		})
	}
}

// A reactivated user carries a non-nil zero timestamp rather than NULL. Asserting
// this here as well as in helpers keeps the guarantee attached to the gate that
// would lock people out if it broke.
func TestAssessAllowsAReactivatedMember(t *testing.T) {
	reactivated := time.Time{}.UTC() // exactly what ActivateUser writes
	u := liveUser()
	u.DeletedAt = &reactivated

	if got := Assess(u); !got.Allowed {
		t.Fatalf("a reactivated member must be eligible again, got: %s", got.Reason)
	}
}

// Deactivation must dominate. A deactivated admin is still deactivated: no flag
// combination may produce an allow once the account is gone.
func TestAssessDeniesDeactivatedRegardlessOfOtherFlags(t *testing.T) {
	deleted := time.Now()
	for _, u := range []*dgraphStruct.DgraphUser{
		{Uid: "0x1", DeletedAt: &deleted, IsAdmin: true},
		{Uid: "0x1", DeletedAt: &deleted, IsBot: true},
		{Uid: "0x1", DeletedAt: &deleted, IsExternal: true},
	} {
		if got := Assess(u); got.Allowed {
			t.Fatalf("a deactivated account must never be eligible, got allow: %s", got.Reason)
		}
	}
}

// Being an admin is not eligibility. Admin widens what a person may do; it can
// never substitute for the person being able to act at all.
func TestAssessTreatsAdminAsIrrelevantToEligibility(t *testing.T) {
	u := liveUser()
	u.IsAdmin = true
	if got := Assess(u); !got.Allowed {
		t.Fatalf("an active admin is an active member, got: %s", got.Reason)
	}

	botAdmin := &dgraphStruct.DgraphUser{Uid: "0x1", IsBot: true, IsAdmin: true}
	if got := Assess(botAdmin); got.Allowed {
		t.Fatal("admin must not rescue a bot identity; eligibility is not a permission level")
	}
}
