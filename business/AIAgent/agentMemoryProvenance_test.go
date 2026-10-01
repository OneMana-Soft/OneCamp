package business

import (
	"context"
	"testing"
)

// The attack this guard exists for: text that arrives in a tool result, not from
// a person, asking to be remembered forever.
func TestInjectedContentCannotWriteMemory(t *testing.T) {
	// A run started by an ordinary request. Nobody asked for anything to be kept.
	ctx := WithAgentHumanText(context.Background(), func() []string {
		return []string{"summarise what happened in this channel today"}
	})
	if humanAskedToRemember(ctx) {
		t.Fatal("a plain request was read as permission to write standing memory; " +
			"an instruction inside a tool result could then become permanent")
	}
}

// The legitimate case has to keep working, or the tool is useless.
func TestAPersonAskingStillWorks(t *testing.T) {
	for _, phrasing := range []string{
		"remember that we deploy on Thursdays",
		"Remember: the staging URL is different",
		"from now on, post the summary in this channel",
		"always tag me when a build fails",
		"note that Priya owns billing",
		"going forward, skip the weekend runs",
		"keep in mind we are on IST",
		"never post before 9am",
	} {
		ctx := WithAgentHumanText(context.Background(), func() []string { return []string{phrasing} })
		if !humanAskedToRemember(ctx) {
			t.Errorf("a person saying %q was refused", phrasing)
		}
	}
}

// Steering is a person talking mid-run, so it counts.
func TestSteeringCountsAsAsking(t *testing.T) {
	ctx := WithAgentHumanText(context.Background(), func() []string {
		return []string{"check the deploy", "actually, remember to always check staging first"}
	})
	if !humanAskedToRemember(ctx) {
		t.Error("a mid-run human instruction was not treated as human provenance")
	}
}

// FAILS CLOSED. A caller that has not been wired loses the ability to remember
// and says so, rather than silently accepting writes from anywhere.
func TestUnwiredCallerCannotWriteMemory(t *testing.T) {
	if humanAskedToRemember(context.Background()) {
		t.Fatal("with no human text attached the guard opened; it must fail closed")
	}
}

// Case must not matter: people type how they type.
func TestCueMatchingIsCaseInsensitive(t *testing.T) {
	ctx := WithAgentHumanText(context.Background(), func() []string { return []string{"REMEMBER THIS"} })
	if !humanAskedToRemember(ctx) {
		t.Error("uppercase phrasing was refused")
	}
}
