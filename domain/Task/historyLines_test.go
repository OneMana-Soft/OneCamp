package Domain

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// historyOf is the lines a write gives each node (by uid), as kind, prev, next.
func historyOf(t *testing.T, set []map[string]any, by string, at time.Time) map[string][][3]string {
	t.Helper()
	out := map[string][][3]string{}
	for _, n := range set {
		uid, _ := n["uid"].(string)
		lines, _ := n["task_activities"].([]map[string]any)
		for _, l := range lines {
			if l["dgraph.type"] != "Activity" || l["activity_uuid"] == "" || l["activity_time"] != at ||
				l["activity_by"].(map[string]any)["uid"] != by {
				t.Errorf("a line on %s isn't a whole line by %s at %v: %v", uid, by, at, l)
			}
			prev, _ := l["activity_prev_state"].(string)
			next, _ := l["activity_next_state"].(string)
			out[uid] = append(out[uid], [3]string{l["activity_type"].(string), prev, next})
		}
	}
	return out
}

// A move writes its history in its own mutation: the task names where it sat
// and where it goes, the task it left names it by id (it is no longer that
// task's sub-task, so only its id can be judged), and the one it went under
// names it by name, as making a sub-task does.
func TestAMoveWritesItsHistoryOnEveryTaskItTouches(t *testing.T) {
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	child := Place{UUID: "c-uuid", Uid: "0x1", Parent: "old-uuid", ParentUid: "0x2"}
	got := historyOf(t, moveHistory("0x9", child, "Ship it", &Place{UUID: "new-uuid", Uid: "0x3"}, at), "0x9", at)
	want := map[string][][3]string{
		"0x1": {{dgraphStruct.ACTIVITY_TYPE_PARENT, "old-uuid", "new-uuid"}},
		"0x2": {{dgraphStruct.ACTIVITY_TYPE_REMOVE_SUB_TASK, "", "c-uuid"}},
		"0x3": {{dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK, "", "Ship it"}},
	}
	for uid, w := range want {
		if len(got[uid]) != 1 || got[uid][0] != w[0] {
			t.Errorf("a move under another parent writes %v on %s, want %v", got[uid], uid, w)
		}
	}
	top := historyOf(t, moveHistory("0x9", child, "Ship it", nil, at), "0x9", at)
	if len(top) != 2 || len(top["0x1"]) != 1 || top["0x1"][0] != [3]string{dgraphStruct.ACTIVITY_TYPE_PARENT, "old-uuid", ""} {
		t.Errorf("a move to the top writes %v", top)
	}
	in := historyOf(t, moveHistory("0x9", Place{UUID: "c-uuid", Uid: "0x1"}, "Ship it", &Place{UUID: "new-uuid", Uid: "0x3"}, at), "0x9", at)
	if len(in) != 2 || in["0x1"][0] != [3]string{dgraphStruct.ACTIVITY_TYPE_PARENT, "", "new-uuid"} {
		t.Errorf("a move from the top writes %v", in)
	}
}

// A link change writes a line on both its tasks, in its own mutation: each
// names the other by id and reads the link from its side.
func TestALinkWritesItsHistoryOnBothTasks(t *testing.T) {
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	from, to := Place{UUID: "f-uuid", Uid: "0x1"}, Place{UUID: "t-uuid", Uid: "0x2"}
	for _, c := range []struct {
		kind     string
		remove   bool
		line     string
		fromSide string
		toSide   string
	}{
		{LinkDuplicates, false, dgraphStruct.ACTIVITY_TYPE_ADD_LINK, "duplicates", "is duplicated by"},
		{LinkClones, true, dgraphStruct.ACTIVITY_TYPE_REMOVE_LINK, "clones", "is cloned by"},
		{LinkRelates, false, dgraphStruct.ACTIVITY_TYPE_ADD_LINK, "relates to", "relates to"},
	} {
		got := historyOf(t, linkHistory("0x9", c.kind, c.remove, from, to, at), "0x9", at)
		want := map[string][3]string{"0x1": {c.line, c.fromSide, "t-uuid"}, "0x2": {c.line, c.toSide, "f-uuid"}}
		if len(got) != 2 {
			t.Errorf("%s (remove %v) writes on %d tasks, want 2: %v", c.kind, c.remove, len(got), got)
		}
		for uid, w := range want {
			if len(got[uid]) != 1 || got[uid][0] != w {
				t.Errorf("%s (remove %v) writes %v on %s, want %v", c.kind, c.remove, got[uid], uid, w)
			}
		}
	}
}
