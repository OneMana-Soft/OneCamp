package business

import (
	"strings"
	"testing"
)

// TestWorkGraphToolsStartAtTheKindTheyName: related_items and how_connected
// are checked at the item they start from, as the kind the call names it:
// a doc by the doc's rule, and a folder, a file or a link (docs F1: doc
// nodes, which an answer names by what they are) by the doc's rule too; a
// task, project, channel and team by theirs, and only the kinds with no rule
// of their own here by the work graph's (ResourceWorkItem). A graph uid, a
// kind the work graph doesn't have and a missing id are refusals, never a
// fallback to a looser kind.
func TestWorkGraphToolsStartAtTheKindTheyName(t *testing.T) {
	const id = "10000000-0000-4000-8000-00000000000a"
	cases := []struct {
		tool   string
		params map[string]string
		kind   ResourceKind
		id     string
	}{
		{"related_items", map[string]string{"kind": "doc", "uuid": id}, ResourceDoc, id},
		{"related_items", map[string]string{"kind": "folder", "uuid": id}, ResourceDoc, id},
		{"related_items", map[string]string{"kind": "file", "uuid": id}, ResourceDoc, id},
		{"related_items", map[string]string{"kind": "Link", "uuid": strings.ToUpper(id)}, ResourceDoc, id},
		{"related_items", map[string]string{"kind": "Task", "uuid": strings.ToUpper(id)}, ResourceTask, id},
		{"related_items", map[string]string{"kind": "project", "uuid": id}, ResourceProject, id},
		{"related_items", map[string]string{"kind": "channel", "uuid": id}, ResourceChannel, id},
		{"related_items", map[string]string{"kind": "team", "uuid": id}, ResourceTeam, id},
		{"related_items", map[string]string{"kind": "person", "uuid": id}, ResourceWorkItem, "person:" + id},
		{"related_items", map[string]string{"kind": "board", "uuid": id}, ResourceWorkItem, "board:" + id},
		{"related_items", map[string]string{"kind": "message", "uuid": id}, ResourceWorkItem, "message:" + id},
		{"how_connected", map[string]string{"from_kind": "doc", "from_uuid": id, "to_kind": "person", "to_uuid": id}, ResourceDoc, id},
		{"how_connected", map[string]string{"from_kind": "message", "from_uuid": id, "to_kind": "doc", "to_uuid": id}, ResourceWorkItem, "message:" + id},
		{"how_connected", map[string]string{"from_kind": "file", "from_uuid": id, "to_kind": "folder", "to_uuid": id}, ResourceDoc, id},
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
		{"kind": "nonsense", "uuid": id},
		{"kind": "page", "uuid": id},
		{"kind": "", "uuid": id},
		{"kind": "doc", "uuid": ""},
		{"uuid": id},
	} {
		if ref, _, err := ResourceForToolCall("related_items", bad); err == nil {
			t.Errorf("%v resolved to %+v; it must be refused", bad, ref)
		}
	}
	// A folder, a file or a link is the doc it is: the same object, read the
	// same way, so the doc's rule decides it and one the caller can't open is
	// refused exactly as that doc would be, never by a looser rule.
	asDoc, _, err := ResourceForToolCall("related_items", map[string]string{"kind": "doc", "uuid": id})
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"folder", "file", "link"} {
		ref, _, err := ResourceForToolCall("related_items", map[string]string{"kind": word, "uuid": id})
		if err != nil || ref != asDoc {
			t.Errorf("a %s is checked as %+v (%v), the doc as %+v", word, ref, err, asDoc)
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
