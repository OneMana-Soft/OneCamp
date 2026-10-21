package business

import (
	"strings"
	"testing"
)

// TestWorkGraphToolsStartAtTheKindTheyName: related_items and how_connected
// are checked at the item they start from, as the kind the call names it:
// a doc by the doc's rule, a task, project, channel and team by theirs, and
// only the kinds with no rule of their own here by the work graph's
// (ResourceWorkItem). A graph uid, a kind the work graph doesn't have and a
// missing id are refusals, never a fallback to a looser kind.
func TestWorkGraphToolsStartAtTheKindTheyName(t *testing.T) {
	const id = "10000000-0000-4000-8000-00000000000a"
	cases := []struct {
		tool   string
		params map[string]string
		kind   ResourceKind
		id     string
	}{
		{"related_items", map[string]string{"kind": "doc", "uuid": id}, ResourceDoc, id},
		{"related_items", map[string]string{"kind": "Task", "uuid": strings.ToUpper(id)}, ResourceTask, id},
		{"related_items", map[string]string{"kind": "project", "uuid": id}, ResourceProject, id},
		{"related_items", map[string]string{"kind": "channel", "uuid": id}, ResourceChannel, id},
		{"related_items", map[string]string{"kind": "team", "uuid": id}, ResourceTeam, id},
		{"related_items", map[string]string{"kind": "person", "uuid": id}, ResourceWorkItem, "person:" + id},
		{"related_items", map[string]string{"kind": "board", "uuid": id}, ResourceWorkItem, "board:" + id},
		{"related_items", map[string]string{"kind": "message", "uuid": id}, ResourceWorkItem, "message:" + id},
		{"how_connected", map[string]string{"from_kind": "doc", "from_uuid": id, "to_kind": "person", "to_uuid": id}, ResourceDoc, id},
		{"how_connected", map[string]string{"from_kind": "message", "from_uuid": id, "to_kind": "doc", "to_uuid": id}, ResourceWorkItem, "message:" + id},
	}
	for _, c := range cases {
		ref, known, err := ResourceForToolCall(c.tool, c.params)
		if !known || err != nil {
			t.Errorf("%s %v: known %v, %v", c.tool, c.params, known, err)
			continue
		}
		if ref.Kind != c.kind || ref.ID != c.id || ref.Access != AccessRead {
			t.Errorf("%s %v: %+v, want %s %s read", c.tool, c.params, ref, c.kind, c.id)
		}
	}
	for _, bad := range []map[string]string{
		{"kind": "doc", "uuid": "0x1f"},
		{"kind": "doc", "uuid": `{ q(func: has(doc_uuid)) { uid } }`},
		{"kind": "folder", "uuid": id},
		{"kind": "", "uuid": id},
		{"kind": "doc", "uuid": ""},
		{"uuid": id},
	} {
		if ref, _, err := ResourceForToolCall("related_items", bad); err == nil {
			t.Errorf("%v resolved to %+v; it must be refused", bad, ref)
		}
	}
}

// TestOnlyTheWorkGraphToolsUseWorkItem: ResourceWorkItem reads by the work
// graph's rule; a tool that reads one kind of thing answers to that kind's
// rule instead, so nothing else may borrow it, and it never writes.
func TestOnlyTheWorkGraphToolsUseWorkItem(t *testing.T) {
	allowed := map[string]bool{"related_items": true, "how_connected": true}
	for _, table := range [][]binding{bridgedTools, inAppTools} {
		for _, b := range table {
			if b.Kind != ResourceWorkItem && b.KindArg == "" {
				continue
			}
			if !allowed[b.Tool] {
				t.Errorf("%q uses the work graph's kind or a kind argument; only the work graph's tools may", b.Tool)
			}
			if !b.Behaviour.ReadOnly {
				t.Errorf("%q writes through ResourceWorkItem, which is read only", b.Tool)
			}
		}
	}
	if _, err := resourceResolver(binding{Tool: "read_doc", Kind: ResourceDoc, IDArg: "doc_uuid", KindArg: "kind"}); err == nil {
		t.Error("a kind argument on a tool that isn't ResourceWorkItem must not register")
	}
	if _, err := resourceResolver(binding{Tool: "related_items", Kind: ResourceWorkItem, IDArg: "uuid"}); err == nil {
		t.Error("ResourceWorkItem without a kind argument must not register")
	}
	if _, err := resourceResolver(binding{Tool: "related_items", Kind: ResourceWorkItem, IDArg: "uuid", KindArg: "type"}); err == nil {
		t.Error("a kind argument the tool doesn't declare must not register")
	}
}
