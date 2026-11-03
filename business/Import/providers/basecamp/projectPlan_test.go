package basecamp

import (
	"context"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// Head 3 part 2 (linear-15's times from Basecamp): a done to-do, read through
// a project's to-dos as the import reads them, comes with when it was
// completed (Basecamp's completion), so a project's pace counts it then and
// not at its last edit (business/Import statusSince). An open to-do comes
// with none, and so does a card done by its column, which Basecamp gives no
// completion: the import counts it from its last change. Basecamp's projects
// keep no lead, and its project reads give no dates, so a project has no
// plan.
func TestADoneBasecampTodoComesWithWhenItWasCompleted(t *testing.T) {
	fakeBasecamp(t)
	p, job := testProvider(t)
	out, errs := p.IterTasksOfProject(context.Background(), job, nil, "1")
	by := map[string]importProvider.SourceTask{}
	for _, st := range drain(t, out, errs) {
		by[st.SourceID] = st
	}
	if want := time.Date(2026, 1, 2, 16, 30, 0, 0, time.UTC); by["112"].StatusSince == nil || !by["112"].StatusSince.Equal(want) {
		t.Errorf("a done to-do's time: %v, want %v", by["112"].StatusSince, want)
	}
	for _, id := range []string{"111", "231"} {
		if s := by[id].StatusSince; s != nil {
			t.Errorf("%s: %v, want none", id, s)
		}
	}
	projects, errs := p.IterProjects(context.Background(), job, nil)
	for _, sp := range drain(t, projects, errs) {
		if sp.Plan != nil {
			t.Errorf("%s: plan %+v, want none", sp.SourceID, sp.Plan)
		}
	}
}
