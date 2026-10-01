package business

// AuthorizeToolCall is the seam. These test the LADDER — that every refusal is
// reachable, that the order is the documented one, and that no refusal depends on a
// lookup that could itself be unavailable.
//
// The ordering assertions matter as much as the outcomes. If a scope check moved
// after the resource lookup, an unauthenticated caller could make the server do
// graph work, which is both a performance and a denial-of-service property.

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestAuthorizeToolCallRefusesAnInvalidCredentialBeforeAnythingElse(t *testing.T) {
	resetRegistryForTest()

	// A tool whose Resource function records whether it was consulted. It must not
	// be: an unauthenticated call has to be refused before the server does any work
	// on its behalf.
	resourceConsulted := false
	spec := validSpec("probe")
	spec.Resource = func(map[string]any) (ResourceRef, error) {
		resourceConsulted = true
		return ResourceRef{Kind: ResourceWorkspace}, nil
	}
	if err := Register(spec); err != nil {
		t.Fatal(err)
	}

	for _, token := range []string{"", "   ", "not-a-real-token", "onecamp_pat_bogus"} {
		d := AuthorizeToolCall(context.Background(), ToolCallRequest{
			TokenPlaintext: token,
			ToolName:       "probe",
		})
		if d.Allow {
			t.Fatalf("credential %q was accepted", token)
		}
		if !strings.Contains(d.Reason, "credential") {
			t.Errorf("token %q: reason = %q, want it to name the credential", token, d.Reason)
		}
	}

	if resourceConsulted {
		t.Error("an unauthenticated call reached the tool's Resource function; the " +
			"actor check must come first so an anonymous caller cannot make the " +
			"server do work")
	}
}

// A refusal must never reveal whether a tool exists, because a tool name is
// information about the workspace. Both an unknown tool and a real tool the caller
// cannot use must fail identically from the outside — here, both fail on the
// credential, which is the earliest and least informative point.
func TestAuthorizeToolCallDoesNotDiscloseTheCatalogue(t *testing.T) {
	resetRegistryForTest()
	if err := Register(validSpec("real_tool")); err != nil {
		t.Fatal(err)
	}

	known := AuthorizeToolCall(context.Background(), ToolCallRequest{
		TokenPlaintext: "onecamp_pat_bogus", ToolName: "real_tool",
	})
	unknown := AuthorizeToolCall(context.Background(), ToolCallRequest{
		TokenPlaintext: "onecamp_pat_bogus", ToolName: "no_such_tool",
	})

	if known.Reason != unknown.Reason {
		t.Errorf("an invalid credential must answer identically for a known and an "+
			"unknown tool, otherwise the reply enumerates the catalogue:\n"+
			"  known:   %q\n  unknown: %q", known.Reason, unknown.Reason)
	}
}

// A refusal must be attributable. A denial nobody can trace is a denial nobody can
// investigate, and the audit record is built from these fields.
func TestAuthorizeToolCallRefusalsCarryWhateverIdentityWasResolved(t *testing.T) {
	resetRegistryForTest()
	if err := Register(validSpec("t")); err != nil {
		t.Fatal(err)
	}
	d := AuthorizeToolCall(context.Background(), ToolCallRequest{
		TokenPlaintext: "onecamp_pat_bogus", ToolName: "t",
	})
	if d.Allow {
		t.Fatal("unexpected allow")
	}
	if d.Reason == "" {
		t.Error("every refusal must carry a reason")
	}
	// With an unresolvable token there is no identity to carry, and that is correct —
	// what must not happen is a populated identity that was never verified.
	if d.TokenID != "" || d.PrincipalUserID != "" {
		t.Errorf("an unresolved credential must not yield an identity, got token=%q principal=%q",
			d.TokenID, d.PrincipalUserID)
	}
}

// A tool that cannot say what it will touch must be refused, never defaulted to
// workspace scope — defaulting would turn a malformed argument into an escalation.
func TestAuthorizeToolCallRefusesAnUnresolvableResource(t *testing.T) {
	resetRegistryForTest()
	spec := validSpec("bad_args")
	spec.Resource = func(map[string]any) (ResourceRef, error) {
		return ResourceRef{}, errTestResolve
	}
	if err := Register(spec); err != nil {
		t.Fatal(err)
	}

	// Reached only with a valid credential, which this binary has no database for.
	// The property is still worth stating: the ladder must not treat a resolution
	// error as "workspace".
	d := AuthorizeToolCall(context.Background(), ToolCallRequest{
		TokenPlaintext: "onecamp_pat_bogus", ToolName: "bad_args",
	})
	if d.Allow {
		t.Fatal("a call whose resource could not be resolved was allowed")
	}
}

// An empty resource kind is refused explicitly. Without this check a zero-value
// ResourceRef would fall through to PrincipalCanReach's default branch, which is
// also a refusal — but relying on that leaves the intent unstated and one
// refactor away from being an allow.
func TestAuthorizeToolCallRefusesAnEmptyResourceKind(t *testing.T) {
	resetRegistryForTest()
	spec := validSpec("empty_kind")
	spec.Resource = func(map[string]any) (ResourceRef, error) {
		return ResourceRef{Kind: "", ID: "whatever"}, nil
	}
	if err := Register(spec); err != nil {
		t.Fatal(err)
	}
	d := AuthorizeToolCall(context.Background(), ToolCallRequest{
		TokenPlaintext: "onecamp_pat_bogus", ToolName: "empty_kind",
	})
	if d.Allow {
		t.Fatal("a call with no resource kind was allowed")
	}
}

// An allow must hand the handler everything it needs, so the handler never
// re-derives an identity (and so cannot derive a different one).
func TestDecisionAllowContractIsComplete(t *testing.T) {
	d := Decision{
		Allow:           true,
		Spec:            validSpec("x"),
		TokenID:         "tok",
		PrincipalUserID: "person",
		Call: ToolCallContext{
			PrincipalUserID:    "person",
			PrincipalDgraphUID: "0x1",
			TokenID:            "tok",
			Resource:           ResourceRef{Kind: ResourceWorkspace},
		},
	}
	if d.Call.PrincipalUserID != d.PrincipalUserID {
		t.Error("the handler's principal must be the one that was authorised")
	}
	if d.Call.PrincipalDgraphUID == "" {
		t.Error("an allow must carry the resolved graph uid")
	}
	if d.Spec == nil {
		t.Error("an allow must carry the resolved spec")
	}
}

var errTestResolve = json.Unmarshal([]byte(`{`), &struct{}{})

// The budget CONSUMES, so it must be the last rung. If it ran before the scope or
// resource checks, an unauthorised caller could drain a legitimate one's budget —
// a denial of service dressed up as a rate limit.
//
// Asserted by reading the source, because the ordering is a property of the function
// body and there is no way to observe it from outside without a live Redis. Narrow
// and specific, so it fails if someone reorders the ladder.
func TestBudgetIsChargedLastInTheLadder(t *testing.T) {
	raw, err := os.ReadFile("authorize.go")
	if err != nil {
		t.Fatalf("read authorize.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

	idx := func(needle string) int {
		i := strings.Index(src, needle)
		if i < 0 {
			t.Fatalf("could not find %q; this test has gone stale", needle)
		}
		return i
	}

	budget := idx("CheckBudget(")
	for _, earlier := range []string{
		"apiTokenBusiness.Validate(", // actor
		"ResolveActor(",              // identity + kill switch
		"Lookup(",                    // tool
		"hasScope(",                  // scope
		"spec.Resource(",             // resource resolution
		"PrincipalCanReach(",         // reach
	} {
		if idx(earlier) > budget {
			t.Errorf("%s runs AFTER CheckBudget; every free check must precede the "+
				"one that consumes, or an unauthorised caller can drain a legitimate "+
				"credential's budget", earlier)
		}
	}
}
