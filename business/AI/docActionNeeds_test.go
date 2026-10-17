package business

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"

	"github.com/akashc777/OneCamp/business/Access/level"
)

// Docs incident, 14 Oct 2026 (D1): Ask AI's reading actions need only view;
// only what changes the doc needs edit. A member who could read a workspace
// doc got no Ask AI at all.
func TestDocAIReadingActionsNeedOnlyView(t *testing.T) {
	for _, a := range []string{"summarize", "explain", "ask", "translate", " Summarize "} {
		if got := DocActionNeeds(a); got != level.View {
			t.Errorf("%q needs %v, want view: its answer is read in the panel", a, got)
		}
	}
	for _, a := range []string{"write", "expand", "fix_grammar", "shorten", "rewrite", "improve", "fix", "continue", "draft", "tone", "simplify", "edit", "longer", "shorter", "draft_from_tasks"} {
		if got := DocActionNeeds(a); got != level.Edit {
			t.Errorf("%q needs %v, want edit: it changes the doc", a, got)
		}
	}
	if got := DocActionNeeds("something-new"); got != level.Edit {
		t.Errorf("an action nobody placed needs %v, want edit", got)
	}
}

// The web offers by the same list the server refuses by.
func TestDocActionNeedsByNameIsTheList(t *testing.T) {
	byName := DocActionNeedsByName()
	if len(byName) != len(docActionNeeds) {
		t.Fatalf("%d names, want %d", len(byName), len(docActionNeeds))
	}
	for name, l := range docActionNeeds {
		if byName[name] != l.String() {
			t.Errorf("%q is %q, want %q", name, byName[name], l.String())
		}
	}
	byName["summarize"] = "edit"
	if DocActionNeeds("summarize") != level.View {
		t.Error("changing the answer changed the list")
	}
}

// The level each action needs is the level CheckDocForAI asks of the doc: a
// viewer may summarise, explain and ask, and is refused a change.
func TestDocAIActionsNeedTheirLevel(t *testing.T) {
	for _, c := range []struct {
		level  accessBusiness.Level
		action string
		want   error
	}{
		{accessBusiness.LevelView, "summarize", nil},
		{accessBusiness.LevelView, "explain", nil},
		{accessBusiness.LevelView, "ask", nil},
		{accessBusiness.LevelView, "improve", ErrDocAICantEdit},
		{accessBusiness.LevelComment, "continue", ErrDocAICantEdit},
		{accessBusiness.LevelEdit, "improve", nil},
	} {
		fakeRecord(t)
		fakeDecision(t, accessBusiness.Decision{Level: c.level}, nil)
		if _, err := CheckDocForAI(context.Background(), member(), recDoc, DocActionNeeds(c.action)); !errors.Is(err, c.want) && !(c.want == nil && err == nil) {
			t.Errorf("%v asking %s: %v, want %v", c.level, c.action, err, c.want)
		}
	}
}
