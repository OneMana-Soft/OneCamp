package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A call's recap is kept on its node whatever else it made: its words, and its
// action items without any task (a task is noted on its own), at most
// MaxActionItems of them. A recap that says nothing writes nothing.
func TestACallKeepsItsRecapAndItsActionItems(t *testing.T) {
	r := CallRecap{Text: "  **Summary**\n- We ship Friday  ", Items: []ActionItem{
		{Text: " Send the deck ", Owner: "Maya", Due: "2026-11-14", Task: uuid.NewString()},
		{Text: "   "},
		{Text: "Book the room"},
	}}
	got, err := r.fields()
	if err != nil {
		t.Fatal(err)
	}
	if got["recording_recap"] != "**Summary**\n- We ship Friday" {
		t.Errorf("recap = %q, want its words trimmed", got["recording_recap"])
	}
	var items []ActionItem
	if err := json.Unmarshal([]byte(fmt.Sprint(got["recording_action_items"])), &items); err != nil {
		t.Fatalf("items %v: %v", got["recording_action_items"], err)
	}
	want := []ActionItem{{Text: "Send the deck", Owner: "Maya", Due: "2026-11-14"}, {Text: "Book the room"}}
	if fmt.Sprint(items) != fmt.Sprint(want) {
		t.Errorf("items = %+v, want %+v (no blank item, no task)", items, want)
	}

	many := CallRecap{Text: "x"}
	for i := 0; i < MaxActionItems+5; i++ {
		many.Items = append(many.Items, ActionItem{Text: fmt.Sprintf("item %d", i)})
	}
	got, _ = many.fields()
	if n := strings.Count(fmt.Sprint(got["recording_action_items"]), `"text"`); n != MaxActionItems {
		t.Errorf("kept %d items, want at most %d", n, MaxActionItems)
	}

	if got, _ := (CallRecap{Text: "  "}).fields(); len(got) != 0 {
		t.Errorf("a recap that says nothing wrote %v", got)
	}
}

// An event's "Recordings & notes" read each call's recap and action items,
// each item with the task made from it: the least when two were noted, none
// for a note that names no item or no task.
func TestAnEventsCallCarriesItsRecapAndEachItemsTask(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	if b.String() < a.String() {
		a, b = b, a
	}
	tasksLive(t, a, b, c)
	items := `[{"text":"Send the deck","owner":"Maya","due":"2026-11-14"},{"text":"Book the room"},{"text":"Draft the FAQ"}]`
	raw, _ := json.Marshal(map[string]any{"artefacts": []map[string]any{{
		"recording_egress_id": "call:s1", "lines": 9, "recording_transcript_only": true,
		"recording_recap": "**Summary**\n- We ship Friday", "recording_action_items": items,
		"recording_action_tasks": []string{"1:" + b.String(), "1:" + a.String(), "7:" + c.String(), "x", "2:not-a-task"},
	}, {"recording_egress_id": "EG_2", "lines": 1}}})
	got, err := parseEventCallArtefacts(context.Background(), raw)
	if err != nil || len(got) != 2 {
		t.Fatalf("parseEventCallArtefacts = %+v, %v", got, err)
	}
	call := got[0]
	if call.Recap != "**Summary**\n- We ship Friday" || len(call.ActionItems) != 3 {
		t.Fatalf("call = %+v, want its recap and three items", call)
	}
	if it := call.ActionItems[0]; it.Text != "Send the deck" || it.Owner != "Maya" || it.Due != "2026-11-14" || it.Task != "" {
		t.Errorf("item 0 = %+v", it)
	}
	if call.ActionItems[1].Task != a.String() {
		t.Errorf("item 1's task = %q, want the least noted, %s", call.ActionItems[1].Task, a)
	}
	if call.ActionItems[2].Task != "" {
		t.Errorf("item 2's task = %q, want none for a note that names no task", call.ActionItems[2].Task)
	}
	if got[1].Recap != "" || got[1].ActionItems != nil {
		t.Errorf("a call with no recap = %+v", got[1])
	}
	if v := actionTaskValue(4, a); v != "4:"+a.String() {
		t.Errorf("actionTaskValue = %q", v)
	}
}

// Only an event's read asks for a recap: a board's calls (CallArtefacts) list
// what each call kept, never its recap.
func TestOnlyAnEventsReadAsksForTheRecap(t *testing.T) {
	for _, f := range []string{"recording_recap", "recording_action_items", "recording_action_tasks"} {
		asks := regexp.MustCompile(`\b` + f + `\b`).MatchString
		if !asks(eventArtefactFields) {
			t.Errorf("an event's read doesn't ask for %s", f)
		}
		if asks(artefactFields) {
			t.Errorf("every list of calls asks for %s", f)
		}
		if q, _ := callArtefactsQuery([]CallWindow{{Key: "call:s1", Home: "ch"}}); asks(q) {
			t.Errorf("a board's calls ask for %s", f)
		}
	}
}

// A recap's headings and the lines that say there is nothing, as its maker
// (business/AI) and its readers (business/Calendar) both tell them.
func TestARecapsHeadingsAndItsNothingLines(t *testing.T) {
	for l, want := range map[string]bool{"## Decisions": true, "**📝 Summary**": true, "****": false, "**Ship** it": false, "- We ship Friday": false} {
		if got := IsRecapHeading(l); got != want {
			t.Errorf("IsRecapHeading(%q) = %v, want %v", l, got, want)
		}
	}
	for l, want := range map[string]bool{"None": true, "N/A": true, "n/a.": true, "_None._": true, "No action items": true, "**No action items.**": true, "Nonetheless": false, "None of us": false} {
		if got := IsRecapNone(l); got != want {
			t.Errorf("IsRecapNone(%q) = %v, want %v", l, got, want)
		}
	}
}

// tasksLive stands in for the graph's read of which tasks are live: these
// ones, and records what it was asked.
func tasksLive(t *testing.T, live ...uuid.UUID) *[][]string {
	t.Helper()
	old := liveTasks
	t.Cleanup(func() { liveTasks = old })
	var asked [][]string
	liveTasks = func(_ context.Context, ids []string) (map[string]bool, error) {
		asked = append(asked, ids)
		out := map[string]bool{}
		for _, id := range live {
			out[id.String()] = true
		}
		return out, nil
	}
	return &asked
}

// An action item keeps its task only while that task is live (V-1018): a
// deleted task leaves the item with none, so the event offers Make task
// again, and a task made after it is the item's. The read asks the graph
// once, for every task the calls note, and not at all when none is noted.
func TestAnActionItemsDeletedTaskIsNotItsTask(t *testing.T) {
	gone, made, other := uuid.New(), uuid.New(), uuid.New()
	if made.String() < gone.String() {
		gone, made = made, gone // the deleted one sorts first, so taking the least is not enough
	}
	asked := tasksLive(t, made, other)
	items := `[{"text":"Send the deck"},{"text":"Book the room"},{"text":"Draft the FAQ"}]`
	raw, _ := json.Marshal(map[string]any{"artefacts": []map[string]any{{
		"recording_egress_id": "call:s1", "recording_action_items": items,
		"recording_action_tasks": []string{"0:" + gone.String(), "1:" + gone.String(), "1:" + made.String(), "2:" + other.String()},
	}, {"recording_egress_id": "EG_2", "recording_action_items": items}}})
	got, err := parseEventCallArtefacts(context.Background(), raw)
	if err != nil || len(got) != 2 || len(got[0].ActionItems) != 3 {
		t.Fatalf("parseEventCallArtefacts = %+v, %v", got, err)
	}
	if task := got[0].ActionItems[0].Task; task != "" {
		t.Errorf("item 0's task = %q, want none: its one task was deleted", task)
	}
	if task := got[0].ActionItems[1].Task; task != made.String() {
		t.Errorf("item 1's task = %q, want the live one made after, %s", task, made)
	}
	if task := got[0].ActionItems[2].Task; task != other.String() {
		t.Errorf("item 2's task = %q, want its live task %s", task, other)
	}
	if len(*asked) != 1 || len((*asked)[0]) != 3 {
		t.Errorf("asked the graph %v, want once for the three tasks noted", *asked)
	}
	*asked = nil
	if _, err := parseEventCallArtefacts(context.Background(), []byte(`{"artefacts":[{"recording_egress_id":"EG_3","lines":2}]}`)); err != nil || len(*asked) != 0 {
		t.Errorf("a call with no task noted: %v, asked %v; want no read", err, *asked)
	}
}

// A board's calls read their action items' tasks as an event's do, through
// the one live-task read: a live task stays its item's and a deleted one is
// dropped, asked of the graph once for every call, and not at all when no
// task is noted, as the board's own query notes none
// (TestOnlyAnEventsReadAsksForTheRecap).
func TestABoardsCallCarriesOnlyLiveTasks(t *testing.T) {
	gone, kept := uuid.New(), uuid.New()
	asked := tasksLive(t, kept)
	items := `[{"text":"Send the deck"},{"text":"Book the room"}]`
	raw, _ := json.Marshal(map[string]any{
		"s0": []map[string]any{{"recording_egress_id": "CS_one", "recording_action_items": items,
			"recording_action_tasks": []string{"0:" + kept.String(), "1:" + gone.String()}}},
		"s1": []map[string]any{{"recording_egress_id": "CS_two", "recording_action_items": items,
			"recording_action_tasks": []string{"0:" + kept.String()}}},
	})
	calls := []CallWindow{{Key: "CS_one", Home: "ch"}, {Key: "CS_two", Home: "ch"}}
	got, err := parseCallArtefacts(context.Background(), raw, calls)
	if err != nil || len(got["CS_one"]) != 1 || len(got["CS_one"][0].ActionItems) != 2 || len(got["CS_two"]) != 1 {
		t.Fatalf("parseCallArtefacts = %+v, %v", got, err)
	}
	if task := got["CS_one"][0].ActionItems[0].Task; task != kept.String() {
		t.Errorf("the first call's item 0 has task %q, want its live task %s", task, kept)
	}
	if task := got["CS_one"][0].ActionItems[1].Task; task != "" {
		t.Errorf("the first call's item 1 has task %q, want none: its task was deleted", task)
	}
	if task := got["CS_two"][0].ActionItems[0].Task; task != kept.String() {
		t.Errorf("the second call's item 0 has task %q, want its live task %s", task, kept)
	}
	if len(*asked) != 1 || len((*asked)[0]) != 2 {
		t.Errorf("asked the graph %v, want once for the two tasks noted", *asked)
	}
	*asked = nil
	if _, err := parseCallArtefacts(context.Background(), []byte(`{"s0":[{"recording_egress_id":"CS_one","lines":2}]}`), calls[:1]); err != nil || len(*asked) != 0 {
		t.Errorf("a board's call with no task noted: %v, asked %v; want no read", err, *asked)
	}
}

// A read that can't tell which tasks are live fails, rather than show a
// deleted task as an item's.
func TestAnActionItemsTasksUnreadFailsTheRead(t *testing.T) {
	old := liveTasks
	t.Cleanup(func() { liveTasks = old })
	liveTasks = func(context.Context, []string) (map[string]bool, error) { return nil, errors.New("graph down") }
	raw := []byte(`{"artefacts":[{"recording_egress_id":"call:s1","recording_action_items":"[{\"text\":\"a\"}]","recording_action_tasks":["0:` + uuid.NewString() + `"]}]}`)
	if got, err := parseEventCallArtefacts(context.Background(), raw); err == nil {
		t.Errorf("parseEventCallArtefacts = %+v, want the graph's error", got)
	}
}
