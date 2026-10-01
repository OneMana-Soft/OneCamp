package business

// The whole path, once: what the model emits -> what a person sees -> what the
// run is told when it resumes.
//
// Every piece is unit-tested on its own. This asserts they are connected, which
// is the thing unit tests cannot see and the reason a feature ships broken.

import (
	"strings"
	"testing"
)

func TestElicitationEndToEnd(t *testing.T) {
	// 1. What a model emits. Params are flattened to strings by
	//    ToolCallToAction, so the array arrives re-marshalled.
	params := map[string]string{
		"reason":  "Which environment should I deploy to?",
		"options": `["staging","production"]`,
	}

	// 2. The runner builds the question.
	elic := newElicitation(params["reason"], params["options"])
	if sensitiveElicitation(elic.Question) {
		t.Fatal("a legitimate question was refused as sensitive")
	}
	if len(elic.Options) != 2 {
		t.Fatalf("options did not survive the flatten: %v", elic.Options)
	}

	// 3. The durable worker stores the rendered form (agentTaskWorker.go).
	stored := "blocked: " + elic.Render()

	// 4. The work feed splits it for the FE (agentActiveWork.go).
	note, options := blockerNote(stored)
	if note != "Which environment should I deploy to?" {
		t.Errorf("the card shows %q", note)
	}
	if len(options) != 2 || options[0] != "staging" {
		t.Fatalf("the card offers %v", options)
	}

	// 5. A person replies. The run matches it against the SAME list.
	back := Elicitation{Question: note, Options: options}
	action, choice := back.MatchAnswer("production")
	if action != ElicitAccept || choice != "production" {
		t.Fatalf("reply matched to (%s, %q)", action, choice)
	}

	// 6. The agent resumes knowing the decision, not re-reading the sentence.
	if n := back.ResumeNote(action, choice, "production"); !strings.Contains(n, "production") {
		t.Errorf("resume note does not carry the decision: %q", n)
	}

	// And a refusal survives the same trip as a refusal.
	action, _ = back.MatchAnswer("no")
	if action != ElicitDecline {
		t.Fatalf("a refusal came back as %s", action)
	}
	if n := back.ResumeNote(action, "", "no"); !strings.Contains(strings.ToLower(n), "do not ask this again") {
		t.Errorf("a refusal does not stop the re-ask: %q", n)
	}
}

// A credential request must never reach a person, on any path.
func TestCredentialRequestNeverBecomesAQuestion(t *testing.T) {
	elic := newElicitation("Please paste your API key so I can deploy", "")
	if !sensitiveElicitation(elic.Question) {
		t.Fatal("a credential request would have been put to a person")
	}
}
