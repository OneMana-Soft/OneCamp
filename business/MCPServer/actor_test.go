package business

// The actor identity, and the property that makes it worth having: deactivating an
// agent stops it, on the next call, without anyone hunting for credentials.
//
// The lookup path needs a database, so it is covered in integration. What is asserted
// here is the contract that decides whether the kill switch is a kill switch — the
// choice between REFUSING a deactivated agent and quietly downgrading it to an
// unbound credential. Downgrading would mean deactivating an agent removed its
// attribution while leaving its access intact, which is the most dangerous possible
// reading of the flag.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestUnboundCredentialResolvesToAnApiClient(t *testing.T) {
	// Every token predating migration 138 is unbound, and must keep working exactly
	// as before. "api_client" is also the honest label for a script.
	actor, ok, reason := ResolveActor(context.Background(), "tok-1", nil, "person-1")
	if !ok {
		t.Fatalf("an unbound credential was refused: %s", reason)
	}
	if actor.Type != ActorAPIClient {
		t.Errorf("type = %q, want %q", actor.Type, ActorAPIClient)
	}
	if actor.AgentID != "" || actor.AgentName != "" {
		t.Error("an unbound credential must not claim an agent identity")
	}
	if actor.TokenID != "tok-1" || actor.PrincipalUserID != "person-1" {
		t.Errorf("credential and principal must be carried through: %+v", actor)
	}
}

// THE kill-switch property. With no database configured the agent lookup fails, which
// is the same code path as "cannot verify the agent" — and it must refuse rather than
// fall back to an unbound credential.
func TestAnUnverifiableAgentIsRefusedNotDowngraded(t *testing.T) {
	id := uuid.New()
	actor, ok, reason := ResolveActor(context.Background(), "tok-1", &id, "person-1")

	if ok {
		t.Fatal("a credential bound to an agent that could not be verified was allowed")
	}
	if actor.Type == ActorAgent {
		t.Error("an unverified agent must not be reported as an active agent identity")
	}
	if reason == "" {
		t.Error("a refusal must carry a reason")
	}
	if !strings.Contains(reason, "agent") {
		t.Errorf("the reason should say the agent is the problem, got %q", reason)
	}
}

// The label is read by humans — in an audit summary and on an approval card — so it
// must never render as an empty string or a hex id.
func TestActorLabelIsAlwaysHumanReadable(t *testing.T) {
	cases := []struct {
		name  string
		actor ActorIdentity
		want  string
	}{
		{"named agent", ActorIdentity{Type: ActorAgent, AgentName: "Triage"}, "Triage"},
		{"agent with no name", ActorIdentity{Type: ActorAgent, AgentID: "abc"}, "an agent"},
		{"agent with blank name", ActorIdentity{Type: ActorAgent, AgentName: "   "}, "an agent"},
		{"api client", ActorIdentity{Type: ActorAPIClient}, "an API client"},
		{"zero value", ActorIdentity{}, "an API client"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.actor.Label()
			if got != c.want {
				t.Errorf("Label() = %q, want %q", got, c.want)
			}
			if strings.TrimSpace(got) == "" {
				t.Error("a label must never be blank; it is rendered to a person")
			}
		})
	}
}

// Actor type is a closed, recorded vocabulary. An auditor querying "everything an
// agent did" matches one value, so the values must not drift or overlap.
func TestActorTypeVocabulary(t *testing.T) {
	if ActorAgent == ActorAPIClient {
		t.Fatal("actor types must be distinguishable")
	}
	for _, ty := range []ActorType{ActorAgent, ActorAPIClient} {
		if strings.TrimSpace(string(ty)) == "" {
			t.Error("an actor type must not be empty; it is written into audit metadata")
		}
		if string(ty) != strings.ToLower(string(ty)) {
			t.Errorf("actor type %q should be lower case for stable querying", ty)
		}
	}
}

// A refusal must still name the agent that was refused. "Which agent was stopped" is
// as much a part of the evidence as which one succeeded.
func TestRefusalsCarryTheActorOnceItIsKnown(t *testing.T) {
	actor := ActorIdentity{Type: ActorAgent, AgentID: "a-1", AgentName: "Triage", TokenID: "tok-1"}
	d := denyAs(actor, "not a member", "tok-1", "person-1")

	if d.Allow {
		t.Fatal("denyAs produced an allow")
	}
	if d.Actor.AgentName != "Triage" {
		t.Errorf("the refused agent was not recorded: %+v", d.Actor)
	}
	if d.Reason == "" {
		t.Error("a refusal must carry a reason")
	}
}
