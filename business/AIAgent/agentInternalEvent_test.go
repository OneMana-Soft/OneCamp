package business

// Pins that only the events an agent may hear can be bound as its event
// trigger (agentEvents in agentEventAccess.go), internal ones never.
//
// The delegation guard (AuthorizeDelegation) is only consulted by
// dispatchMentionAgents. handleAgentEvent's generic event loop runs first and
// checks none of it, so an agent bound to "agent.message" would act on another
// agent's message with no hop budget, no cycle check, no permission check that the
// originating person could address the surface, and no human recorded as the actor.
//
// The admin UI cannot produce that configuration; the API can. These tests are the
// reason it stops mattering which one wrote the row.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestInternalEventTypesAreNotBindableAsTriggers(t *testing.T) {
	// Every internal event must be refused. agent.message is the one that exists
	// today and the one with teeth; the others guard the prefix rule itself.
	for _, ev := range []string{
		EventTypeAgentMessage,
		"agent.message",
		"  agent.message  ", // trigger_config is free text, so it may carry padding
		"AGENT.MESSAGE",     // and arbitrary casing
		"Agent.Message",
		"agent.something.future", // a later internal event is covered on day one
	} {
		if bindableEvent(ev) || boundEvent(ev) {
			t.Errorf("event %q must not be bindable as an agent trigger: the generic "+
				"launch path performs no delegation or permission checks", ev)
		}
	}
}

func TestRealWorkspaceEventsStayBindable(t *testing.T) {
	// The guard must not cost the workspace its actual event vocabulary. These are
	// the values the admin UI offers (services/agentService.ts
	// EVENT_TRIGGER_OPTIONS) plus the ones the dispatcher special-cases.
	for _, ev := range []string{
		"task.created",
		"task.status_changed",
		"task.deleted",
		"post.created",
		"post.comment.created",
		"channel.created",
		"user.joined",
		"table.row.created",
		"table.row.updated",
	} {
		if !bindableEvent(ev) || !boundEvent(ev) {
			t.Errorf("event %q is a real workspace event and must stay bindable", ev)
		}
	}
}

// A GitHub event can't be newly bound, and an agent bound to one before is not
// run on it: the trigger cache skips it, with a line saying so.
func TestGitHubEventsAreWithdrawn(t *testing.T) {
	for _, ev := range []string{"github.pr.opened", "github.pr.review_submitted", "github.check_run.completed", "github.issue.opened"} {
		if bindableEvent(ev) {
			t.Errorf("event %q can still be chosen for an agent", ev)
		}
	}
	raw, err := os.ReadFile("agentTriggers.go")
	if err != nil {
		t.Fatal(err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")
	start := strings.Index(src, "byEvent := make(")
	end := strings.Index(src, "eventCache = byEvent")
	if start < 0 || end < start || !strings.Contains(src[start:end], "withdrawnEvents[ev]") {
		t.Error("the trigger cache still runs agents bound to a withdrawn GitHub event")
	}
}

// TestConversationsAreNotBindable: a direct message, and anything else that
// isn't one of the offered events, never reaches an event agent.
func TestConversationsAreNotBindable(t *testing.T) {
	for _, ev := range []string{"chat.created", "chat.updated", "chat.comment.created", "post.updated", "task.assigned", "", "Task.Created"} {
		if bindableEvent(ev) {
			t.Errorf("event %q must not be bindable", ev)
		}
	}
}

// TestInternalEventGuardIsWiredIntoTheEventCache is the other half.
//
// The tests above only exercise the predicate. They would all still pass if the
// call to it were deleted from the cache builder, which is where it actually does
// the work — and that is the more likely regression, because the predicate looks
// like a harmless helper while the call site looks like a line you can simplify.
//
// The cache builder needs Postgres, so it cannot be exercised directly in a unit
// test. Reading the source is the honest alternative: narrow, specific, and it
// fails if the wiring goes away.
func TestInternalEventGuardIsWiredIntoTheEventCache(t *testing.T) {
	raw, err := os.ReadFile("agentTriggers.go")
	if err != nil {
		t.Fatalf("read agentTriggers.go: %v", err)
	}
	// Strip comments first: this file explains the guard at length, and prose about
	// it must not be mistaken for the guard.
	src := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(raw), " ")
	src = regexp.MustCompile(`//[^\n]*`).ReplaceAllString(src, " ")

	// Isolate the loop that builds the event-trigger cache.
	start := strings.Index(src, "byEvent := make(")
	if start < 0 {
		t.Fatal("could not find the event-trigger cache builder; this test has gone stale")
	}
	end := strings.Index(src[start:], "eventCache = byEvent")
	if end < 0 {
		t.Fatal("could not find where the event cache is published; this test has gone stale")
	}
	builder := src[start : start+end]

	if !strings.Contains(builder, "boundEvent(") {
		t.Error("the event-trigger cache is built without calling boundEvent, " +
			"so an agent can be bound to an internal event such as agent.message and " +
			"will then run through the generic launch path — which performs no hop " +
			"budget, no cycle check, and no AuthorizeDelegation permission check.")
	}
}
