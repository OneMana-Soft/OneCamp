package business

import (
	"testing"
)

// The declarations an agent acting for someone else is judged by. They extend
// the bridge table, so they are held to its rules.

func TestInAppToolDeclarationsAgreeWithTheRegistry(t *testing.T) {
	bridged := map[string]bool{}
	for _, b := range bridgedTools {
		bridged[b.Tool] = true
	}
	seen := map[string]bool{}
	for _, b := range inAppTools {
		if bridged[b.Tool] {
			t.Errorf("%s is declared twice; the bridge table already governs it", b.Tool)
		}
		if seen[b.Tool] {
			t.Errorf("%s is declared twice in inAppTools", b.Tool)
		}
		seen[b.Tool] = true
		def, ok := staticToolDef(b.Tool)
		if !ok {
			t.Errorf("%s is not a static registry tool", b.Tool)
			continue
		}
		if def.ReadOnly != b.Behaviour.ReadOnly {
			t.Errorf("%s is ReadOnly=%v in the registry but %v here; the access asked for would be wrong", b.Tool, def.ReadOnly, b.Behaviour.ReadOnly)
		}
		if _, err := resourceResolver(b); err != nil {
			t.Errorf("%s: %v", b.Tool, err)
		}
	}
}

func TestAToolCallIsJudgedByWhatItNamesAndWhatItDoes(t *testing.T) {
	ref, known, err := ResourceForToolCall("read_doc", map[string]string{"doc_uuid": " d-1 "})
	if !known || err != nil || ref.Kind != ResourceDoc || ref.ID != "d-1" || ref.Access != AccessRead {
		t.Errorf("read_doc: %+v known=%v err=%v", ref, known, err)
	}
	// A write is asked as a write, from the declared behaviour.
	ref, known, err = ResourceForToolCall("append_to_doc", map[string]string{"doc_uuid": "d-1", "content": "x"})
	if !known || err != nil || ref.Kind != ResourceDoc || ref.Access != AccessWrite {
		t.Errorf("append_to_doc: %+v known=%v err=%v", ref, known, err)
	}
	ref, known, err = ResourceForToolCall("send_message", map[string]string{"channel_uuid": "c-1", "text": "hi"})
	if !known || err != nil || ref.Kind != ResourceChannel || ref.Access != AccessWrite {
		t.Errorf("send_message: %+v known=%v err=%v", ref, known, err)
	}
	// A missing target is a refusal, not a widening to the workspace.
	if _, known, err := ResourceForToolCall("summarize_channel", map[string]string{}); !known || err == nil {
		t.Errorf("a call naming no channel must be an error, got known=%v err=%v", known, err)
	}
	if _, known, _ := ResourceForToolCall("no_such_tool", nil); known {
		t.Error("an undeclared tool must be unknown, so the caller refuses it")
	}
}
