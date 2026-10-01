package business

import (
	"context"
	"testing"
)

// Everything here is about one question an auditor asks: what ran on somebody's
// authority while they were away. Every kind's answer to it is a fact the log
// is trusted for, so each is pinned rather than inferred from a name.

func TestNobodyWatchingIsExactlyTheThreeUnattendedKinds(t *testing.T) {
	want := map[Initiator]bool{
		InitiatorPerson:   false,
		InitiatorSchedule: true,
		InitiatorEvent:    true,
		InitiatorHandoff:  true,
		// A dry run against a fixture reached nobody, so it is neither a person
		// in the room nor work done unattended on their behalf.
		InitiatorEval: false,
	}
	for kind, unattended := range want {
		if kind.Unattended() != unattended {
			t.Errorf("%s.Unattended() = %v, want %v", kind, kind.Unattended(), unattended)
		}
	}
	// The filter list is derived from the same predicate. If it were written
	// out by hand it could drift, and the screen would show a different answer
	// from the one the row carries.
	got := UnattendedInitiators()
	if len(got) != 3 {
		t.Errorf("UnattendedInitiators() = %v, want the three unattended kinds", got)
	}
	for _, s := range got {
		if !Initiator(s).Unattended() {
			t.Errorf("%q is listed as unattended but the predicate says otherwise", s)
		}
	}
}

func TestAnUnknownKindNeverReachesTheRecord(t *testing.T) {
	// A category a reviewer has no definition for is worse than none.
	ctx := WithInitiator(context.Background(), Initiator("typo"))
	if _, ok := InitiatorFromCtx(ctx); ok {
		t.Error("an invalid kind was carried on the context")
	}
	if Initiator("").Valid() || Initiator("PERSON").Valid() {
		t.Error("empty or differently-cased values must not be valid")
	}
}

func TestTheContextCarriesTheAnswerToEveryRow(t *testing.T) {
	ctx := WithInitiator(context.Background(), InitiatorSchedule)

	meta := stampInitiator(ctx, ActorAgent, nil)
	if meta[MetaInitiator] != string(InitiatorSchedule) {
		t.Errorf("an agent row did not pick up the initiator: %v", meta)
	}
}

func TestACallerThatAlreadySaidIsNotOverruled(t *testing.T) {
	// A dispatcher that knows the delegation hop sets the answer before the
	// runner sees the generic trigger; the runner must not replace it.
	ctx := WithInitiator(context.Background(), InitiatorSchedule)
	meta := stampInitiator(ctx, ActorAgent, map[string]interface{}{MetaInitiator: "handoff"})
	if meta[MetaInitiator] != "handoff" {
		t.Errorf("an explicit initiator was overwritten by the context's: %v", meta)
	}
}

func TestAPersonsOwnRowIsNeverStamped(t *testing.T) {
	// A person's own action has no initiator but themselves. Writing "schedule"
	// onto a human's row would be a confident wrong answer in a compliance
	// record.
	ctx := WithInitiator(context.Background(), InitiatorSchedule)
	meta := stampInitiator(ctx, ActorHuman, map[string]interface{}{"x": 1})
	if _, stamped := meta[MetaInitiator]; stamped {
		t.Error("a human row was stamped with an initiator")
	}
}

func TestTheWorkspacesOwnScheduledWorkSaysSo(t *testing.T) {
	// A receipt the timer took and one a person exported are the same actor kind
	// and different facts. The system row carries the answer so a reviewer can
	// tell which was which from the log alone.
	ctx := WithInitiator(context.Background(), InitiatorSchedule)
	meta := stampInitiator(ctx, ActorSystem, nil)
	if meta[MetaInitiator] != string(InitiatorSchedule) {
		t.Errorf("a system row under scheduled work was not stamped: %v", meta)
	}
}

func TestNoAnswerIsLeftAsNoAnswer(t *testing.T) {
	// When nothing set the context, the row must not default to "person": that
	// is the one value that overclaims, and an absent key is honest.
	meta := stampInitiator(context.Background(), ActorAgent, nil)
	if _, stamped := meta[MetaInitiator]; stamped {
		t.Errorf("a row with no known initiator was given one: %v", meta)
	}
}

func TestTheVocabularyServedMatchesThePredicate(t *testing.T) {
	kinds := InitiatorKinds()
	if len(kinds) != 5 {
		t.Fatalf("expected 5 kinds, got %d", len(kinds))
	}
	for _, k := range kinds {
		if k.Unattended != Initiator(k.Kind).Unattended() {
			t.Errorf("%s served as unattended=%v, predicate says %v", k.Kind, k.Unattended, Initiator(k.Kind).Unattended())
		}
	}
}
