package business

import "testing"

func TestBlockerCallFromText(t *testing.T) {
	a, ok := blockerCallFromText(`{"options":["Provide the doc_uuid","Explain how to find it"],"reason":"I need the UUID of the Launch sync notes document to add the rollback steps."}`)
	if !ok || a.ToolName != blockerToolName || a.Params["reason"] == "" || a.Params["options"] == "" {
		t.Fatalf("the demo's bare pause was not recognised: %+v %v", a, ok)
	}
	// It then meets the same rule as a real call: nobody can answer an internal id.
	if !internalIDElicitation(newElicitation(a.Params["reason"], a.Params["options"]).Question) {
		t.Fatal("a pause asking for a UUID must be caught by the internal-id rule")
	}
	if a, ok := blockerCallFromText("```json\n{\"name\":\"needs_human\",\"arguments\":{\"reason\":\"Which date works?\"}}\n```"); !ok || a.Params["reason"] != "Which date works?" {
		t.Fatalf("a fenced, wrapped call: %+v %v", a, ok)
	}
	for _, text := range []string{
		`Done. I added the rollback steps.`,
		`{"reason":"   "}`,
		`{"reason":"why?","extra":1}`,
		`{"name":"create_task","arguments":{"reason":"x"}}`,
		`{"status":"ok"}`,
		`Here is JSON: {"reason":"x"}`,
	} {
		if _, ok := blockerCallFromText(text); ok {
			t.Fatalf("mistook %q for a pause", text)
		}
	}
}
