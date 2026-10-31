package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A change that isn't a body (a file's new version, docs F1) is recorded as
// one write: when and who together, on a live doc and an editor who is in the
// graph, both or neither, by the doc's uuid and the editor's (never a uid a
// client sent); the doc's uuid written again, as every doc update writes it.
func TestATouchIsWhenAndWhoInOneWrite(t *testing.T) {
	doc, editor := "5F0C4A2E-8A63-4F3E-9D2B-1C7A9E0B3D41", "6a1d5b3f-9b74-4a4f-8e3c-2d8b0f1c4e52"
	at := time.Date(2026, 10, 21, 17, 30, 0, 0, time.FixedZone("IST", 5*3600+1800))
	q, vars, m, err := touchRequest(doc, editor, at)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, `doc as var(func: eq(doc_uuid, $id)) @filter(`+liveDocFilter+`) { u as doc_uuid }`) || !strings.Contains(q, `editor as var(func: eq(user_uuid, $editor), first: 1)`) {
		t.Errorf("the read: %s", q)
	}
	if vars["$id"] != strings.ToLower(doc) || vars["$editor"] != editor {
		t.Errorf("by uuid, as uuids are written: %v", vars)
	}
	for _, line := range []string{
		"uid(doc) <doc_uuid> val(u) .",
		`uid(doc) <doc_updated_at> "2026-10-21T12:00:00Z" .`,
		"uid(doc) <doc_updated_by> uid(editor) .",
	} {
		if !strings.Contains(m.SetNQuads, line) {
			t.Errorf("the write lacks %q: %s", line, m.SetNQuads)
		}
	}
	if m.Cond != "@if(eq(len(doc), 1) AND eq(len(editor), 1))" || m.DelNQuads != "" || m.Set != nil {
		t.Errorf("one write, both or neither: %+v", m)
	}
	// No time is now.
	if _, _, m, _ := touchRequest(doc, editor, time.Time{}); !strings.Contains(m.SetNQuads, `<doc_updated_at> "`+time.Now().UTC().Format("2006-01-02")) {
		t.Errorf("a touch with no time: %s", m.SetNQuads)
	}
	if _, _, _, err := touchRequest("not-a-uuid", editor, at); !errors.Is(err, ErrNoDocToUpdate) {
		t.Errorf("no doc: %v", err)
	}
	if _, _, _, err := touchRequest(doc, "0x1", at); !errors.Is(err, ErrNoEditor) {
		t.Errorf("an editor named by a uid: %v", err)
	}
}

// A colour is not a change (docs F1, DOCS-LEAD-2's ruling): colouring a
// node writes neither when it changed nor who changed it.
func TestAColourIsNotAChange(t *testing.T) {
	_, muts, err := colorRequest([]string{"5f0c4a2e-8a63-4f3e-9d2b-1c7a9e0b3d41"}, "sky")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range muts {
		for _, p := range []string{"doc_updated_at", "doc_updated_by"} {
			if strings.Contains(m.SetNQuads, p) || strings.Contains(m.DelNQuads, p) {
				t.Errorf("a colour writes %s: %+v", p, m)
			}
		}
	}
}
