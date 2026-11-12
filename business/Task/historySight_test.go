package business

import (
	"testing"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A history line names no task the reader may not see (main's C1, V-678): a
// dependency, link or move line loses a hidden task's id and keeps a seen
// one's, and its words (a label); once a task is hidden, a sub-task line keeps
// only a name a seen task carries (a hidden one's, now or before a rename,
// goes), and with none hidden keeps its name; other lines are left alone.
func TestAHistoryLineNamesNoTaskKeptFromTheReader(t *testing.T) {
	const seen, hidden = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	const twin = "33333333-3333-4333-8333-333333333333"
	named := []*dgraphStruct.DgraphTask{
		{Uuid: seen, Name: "Seen"},
		{Uuid: hidden, Name: "Secret"},
		{Uuid: twin, Name: "Seen"}, // hidden, but a seen task has its name
	}
	verdicts := map[string]workAccess.Verdict{seen: {See: true}, hidden: {}, twin: {}}
	line := func(kind, prev, next string) *dgraphStruct.DgraphTaskActivity {
		return &dgraphStruct.DgraphTaskActivity{Type: kind, PrevState: prev, NextState: next}
	}
	lines := []*dgraphStruct.DgraphTaskActivity{
		line(dgraphStruct.ACTIVITY_TYPE_ADD_DEPENDENCY, "", hidden),
		line(dgraphStruct.ACTIVITY_TYPE_ADD_DEPENDENCY, "", seen),
		line(dgraphStruct.ACTIVITY_TYPE_REMOVE_DEPENDENCY, hidden, ""),
		line(dgraphStruct.ACTIVITY_TYPE_REMOVE_DEPENDENCY, "duplicates", hidden),
		line(dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK, "", "Secret"),
		line(dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK, "", "Seen"),
		line(dgraphStruct.ACTIVITY_TYPE_STATUS, "todo", "Secret"),
		line(dgraphStruct.ACTIVITY_TYPE_ADD_LINK, "is duplicated by", hidden),
		line(dgraphStruct.ACTIVITY_TYPE_REMOVE_LINK, "relates to", seen),
		line(dgraphStruct.ACTIVITY_TYPE_PARENT, seen, hidden),
		line(dgraphStruct.ACTIVITY_TYPE_REMOVE_SUB_TASK, "", hidden),
		nil,
	}
	blankUnseenLines(lines, named, verdicts)
	want := [][2]string{{"", ""}, {"", seen}, {"", ""}, {"duplicates", ""}, {"", ""}, {"", "Seen"}, {"todo", "Secret"},
		{"is duplicated by", ""}, {"relates to", seen}, {seen, ""}, {"", ""}}
	for i, w := range want {
		if got := [2]string{lines[i].PrevState, lines[i].NextState}; got != w {
			t.Errorf("line %d (%s): states %q, want %q", i, lines[i].Type, got, w)
		}
	}

	// A sub-task kept from the reader and renamed since it went under the
	// task (V-678, CR-B36 F1): its line names it "Old", which no task carries
	// now. Once the read finds a task kept from the reader, a sub-task line
	// keeps its name only when a task the reader sees carries it.
	renamed := []*dgraphStruct.DgraphTask{{Uuid: seen, Name: "Seen"}, {Uuid: hidden, Name: "New"}}
	sub := []*dgraphStruct.DgraphTaskActivity{
		line(dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK, "", "Old"),
		line(dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK, "", "Seen"),
	}
	blankUnseenLines(sub, renamed, verdicts)
	if sub[0].NextState != "" || sub[1].NextState != "Seen" {
		t.Errorf("with a renamed sub-task kept from the reader: lines name %q and %q, want \"\" and \"Seen\"", sub[0].NextState, sub[1].NextState)
	}
	// Nothing kept from the reader: a name no task carries now (a seen
	// sub-task renamed since) stays.
	open := []*dgraphStruct.DgraphTaskActivity{line(dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK, "", "Old")}
	blankUnseenLines(open, []*dgraphStruct.DgraphTask{{Uuid: seen, Name: "New"}}, verdicts)
	if open[0].NextState != "Old" {
		t.Errorf("with every task seen: the line names %q, want \"Old\"", open[0].NextState)
	}
}
