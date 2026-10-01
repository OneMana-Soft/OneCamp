package business

// The write path's properties, all of which are the difference between "an agent
// can write" and "an agent can write safely".

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func writeSpec(name string, b ToolBehaviour) *ToolSpec {
	s := validSpec(name)
	s.Behaviour = b
	if b.needsIdempotencyKey() {
		s.IdempotencyKey = func(args map[string]any) (string, error) {
			v, _ := args["body"].(string)
			return v, nil
		}
	}
	return s
}

func callWith(body string) ToolCallContext {
	return ToolCallContext{
		PrincipalUserID: "person-1",
		Resource:        ResourceRef{Kind: ResourceChannel, ID: "chan-1"},
		Args:            map[string]any{"body": body},
	}
}

// THE property. Same arguments must always produce the same key, or a retry
// deduplicates nothing and the guarantee is decorative.
func TestIdempotencyKeyIsStableAcrossRetries(t *testing.T) {
	spec := writeSpec("post_message", ToolBehaviour{})
	first, err := DeriveIdempotencyKey(spec, callWith("hello"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := DeriveIdempotencyKey(spec, callWith("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the same call produced two keys, so a retry would be a duplicate write:\n %s\n %s", first, second)
	}
	if first == "" {
		t.Fatal("a mutating tool produced no key")
	}
}

func TestIdempotencyKeyChangesWithEveryScopingDimension(t *testing.T) {
	spec := writeSpec("post_message", ToolBehaviour{})
	base, _ := DeriveIdempotencyKey(spec, callWith("hello"))

	// Different arguments — a genuinely different write.
	if other, _ := DeriveIdempotencyKey(spec, callWith("goodbye")); other == base {
		t.Error("different arguments must not share a key, or one write would suppress the other")
	}

	// Different resource — the same text posted to another channel is not a duplicate.
	otherResource := callWith("hello")
	otherResource.Resource.ID = "chan-2"
	if other, _ := DeriveIdempotencyKey(spec, otherResource); other == base {
		t.Error("the same text to a different channel must not be treated as a duplicate")
	}

	// Different principal. Without this, one person's call could silently suppress
	// another's identical one — a correctness bug that looks like a permission bug.
	otherPerson := callWith("hello")
	otherPerson.PrincipalUserID = "person-2"
	if other, _ := DeriveIdempotencyKey(spec, otherPerson); other == base {
		t.Error("two different people posting the same text must not collide")
	}

	// Different tool deriving the same raw key must not collide.
	otherTool := writeSpec("create_task", ToolBehaviour{})
	if other, _ := DeriveIdempotencyKey(otherTool, callWith("hello")); other == base {
		t.Error("two tools deriving the same raw key must not collide")
	}
}

// Length-prefixing the hashed parts: "a"+"|b" must not hash like "a|"+"b".
func TestIdempotencyScopeIsNotSeparatorConfusable(t *testing.T) {
	a := idempotencyScope("tool", ResourceRef{Kind: ResourceChannel, ID: "x"}, "p", "y")
	b := idempotencyScope("tool", ResourceRef{Kind: ResourceChannel, ID: "xp"}, "", "y")
	if a == b {
		t.Fatal("scope parts are confusable across boundaries; a crafted resource id " +
			"could impersonate another call's key")
	}
}

func TestReadOnlyAndIdempotentToolsNeedNoKey(t *testing.T) {
	for _, b := range []ToolBehaviour{{ReadOnly: true}, {Idempotent: true}} {
		spec := writeSpec("t", b)
		key, err := DeriveIdempotencyKey(spec, callWith("x"))
		if err != nil {
			t.Fatalf("behaviour %+v: %v", b, err)
		}
		if key != "" {
			t.Errorf("behaviour %+v produced a key it does not need", b)
		}
	}
}

// An empty derived key would make every call to that tool a duplicate of the first —
// silent and total data loss for that tool, so it must be an error.
func TestAnEmptyDerivedKeyIsRefused(t *testing.T) {
	spec := writeSpec("post_message", ToolBehaviour{})
	spec.IdempotencyKey = func(map[string]any) (string, error) { return "   ", nil }

	if _, err := DeriveIdempotencyKey(spec, callWith("x")); err == nil {
		t.Fatal("an empty key was accepted; every subsequent call would be treated " +
			"as a duplicate of the first")
	}
}

func TestRequiresApprovalOnlyForDestructiveTools(t *testing.T) {
	cases := map[bool]ToolBehaviour{
		false: {ReadOnly: true},
		true:  {Destructive: true},
	}
	for want, b := range cases {
		spec := writeSpec("t", b)
		if got := RequiresApproval(spec); got != want {
			t.Errorf("behaviour %+v: RequiresApproval = %v, want %v", b, got, want)
		}
	}
	// Additive writes proceed. Gating everything trains people to click Approve
	// without reading, which is worse than not asking.
	if RequiresApproval(writeSpec("t", ToolBehaviour{})) {
		t.Error("an additive write must not require approval")
	}
	if RequiresApproval(nil) {
		t.Error("a nil spec must not require approval; it must not be actionable at all")
	}
}

func TestPlanWriteOutcomes(t *testing.T) {
	ctx := context.Background()
	none := func(context.Context, string) (ExistingWrite, error) {
		return ExistingWrite{State: ExistingNone}, nil
	}

	t.Run("read-only proceeds with no key", func(t *testing.T) {
		p := PlanWrite(ctx, writeSpec("get", ToolBehaviour{ReadOnly: true}), callWith("x"), "cli", none)
		if p.Outcome != WriteProceed || p.IdempotencyKey != "" {
			t.Fatalf("got %+v", p)
		}
	})

	t.Run("additive write proceeds with a key", func(t *testing.T) {
		p := PlanWrite(ctx, writeSpec("post", ToolBehaviour{}), callWith("x"), "cli", none)
		if p.Outcome != WriteProceed || p.IdempotencyKey == "" {
			t.Fatalf("got %+v", p)
		}
	})

	t.Run("destructive write needs approval", func(t *testing.T) {
		p := PlanWrite(ctx, writeSpec("del", ToolBehaviour{Destructive: true}), callWith("x"), "cli", none)
		if p.Outcome != WriteNeedsApproval {
			t.Fatalf("a destructive write proceeded without approval: %+v", p)
		}
	})

	t.Run("an applied call is not executed again", func(t *testing.T) {
		applied := func(context.Context, string) (ExistingWrite, error) {
			return ExistingWrite{State: ExistingApplied, PendingActionID: "pa-1"}, nil
		}
		p := PlanWrite(ctx, writeSpec("post", ToolBehaviour{}), callWith("x"), "cli", applied)
		if p.Outcome != WriteAlreadyApplied {
			t.Fatalf("a retry would have written twice: %+v", p)
		}
	})

	t.Run("a denied call stays denied", func(t *testing.T) {
		rejected := func(context.Context, string) (ExistingWrite, error) {
			return ExistingWrite{State: ExistingRejected}, nil
		}
		p := PlanWrite(ctx, writeSpec("del", ToolBehaviour{Destructive: true}), callWith("x"), "cli", rejected)
		if p.Outcome != WriteRefused {
			t.Fatalf("a human denied this and it was not refused: %+v", p)
		}
	})

	t.Run("an expired approval may be asked again", func(t *testing.T) {
		expired := func(context.Context, string) (ExistingWrite, error) {
			return ExistingWrite{State: ExistingExpired}, nil
		}
		p := PlanWrite(ctx, writeSpec("del", ToolBehaviour{Destructive: true}), callWith("x"), "cli", expired)
		if p.Outcome != WriteNeedsApproval {
			t.Fatalf("an expired window must be re-askable, not refused forever: %+v", p)
		}
	})

	// The important failure mode. If we cannot tell whether this was already applied,
	// applying it twice is worse than not applying it.
	t.Run("an unreadable store refuses rather than risking a duplicate", func(t *testing.T) {
		broken := func(context.Context, string) (ExistingWrite, error) {
			return ExistingWrite{}, errors.New("store unavailable")
		}
		p := PlanWrite(ctx, writeSpec("post", ToolBehaviour{}), callWith("x"), "cli", broken)
		if p.Outcome != WriteRefused {
			t.Fatalf("proceeded without knowing whether this was a duplicate: %+v", p)
		}
		if !strings.Contains(p.Reason, "duplicate") {
			t.Errorf("reason should explain the risk, got %q", p.Reason)
		}
	})

	t.Run("a missing key derivation refuses", func(t *testing.T) {
		spec := writeSpec("post", ToolBehaviour{})
		spec.IdempotencyKey = nil // registry refuses this; belt and braces here
		p := PlanWrite(ctx, spec, callWith("x"), "cli", none)
		if p.Outcome != WriteRefused {
			t.Fatalf("a mutating tool with no key derivation proceeded: %+v", p)
		}
	})

	t.Run("every plan carries a reason", func(t *testing.T) {
		for _, spec := range []*ToolSpec{
			writeSpec("a", ToolBehaviour{ReadOnly: true}),
			writeSpec("b", ToolBehaviour{}),
			writeSpec("c", ToolBehaviour{Destructive: true}),
		} {
			if p := PlanWrite(ctx, spec, callWith("x"), "cli", none); p.Reason == "" {
				t.Errorf("tool %s produced a plan with no reason", spec.Name)
			}
		}
	})
}

// The approval card is read by a human deciding whether to allow a change. It has to
// say what will change and to what — "run tool delete_task" cannot be approved
// responsibly.
func TestApprovalDescriptionIsWrittenForTheReviewer(t *testing.T) {
	spec := writeSpec("delete_task", ToolBehaviour{Destructive: true})
	got := ApprovalDescription(spec, callWith("x"), "Claude Desktop")

	for _, want := range []string{"Claude Desktop", "delete_task", "chan-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("description omits %q, which the reviewer needs: %q", want, got)
		}
	}
	if !strings.Contains(got, "not undone") {
		t.Errorf("a destructive action should say so plainly: %q", got)
	}

	// An unnamed client still produces a readable sentence rather than a gap.
	anon := ApprovalDescription(spec, callWith("x"), "  ")
	if strings.Contains(anon, `""`) || strings.HasPrefix(anon, " ") {
		t.Errorf("an unnamed client produced a malformed sentence: %q", anon)
	}
	if ApprovalDescription(nil, callWith("x"), "cli") != "" {
		t.Error("a nil spec must produce no description")
	}
}
