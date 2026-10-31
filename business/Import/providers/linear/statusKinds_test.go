package linear

import "testing"

// The plan names each workflow state once, by its name, with its type
// as its class, so "Ready to ship" (started) is proposed as In progress. A
// state with no name is listed by its type, as its issues are mapped.
func TestThePlanGivesEachStateItsType(t *testing.T) {
	values, kinds := planStatuses([]linearIssue{
		{ID: "a", StateName: "Ready to ship", StateType: "started"},
		{ID: "b", StateName: "Todo", StateType: "unstarted"},
		{ID: "c", StateType: "completed"},
	})
	if kinds["ready to ship"] != "started" || kinds["todo"] != "unstarted" {
		t.Errorf("kinds %v, want ready to ship → started and todo → unstarted", kinds)
	}
	want := map[string]bool{"ready to ship": true, "todo": true, "completed": true}
	if len(values) != len(want) {
		t.Errorf("values %v, want each state once: %v", values, want)
	}
	for _, v := range values {
		if !want[v] {
			t.Errorf("values %v list %q", values, v)
		}
	}
}
