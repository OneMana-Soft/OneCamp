package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A change to a doc's look is one upsert on the live doc alone: what it sets,
// written with the doc's uuid (so a delete in flight aborts it), and what it
// takes away, a cover's position with the cover.
func TestALookChangeIsOneUpsertOnTheLiveDoc(t *testing.T) {
	const id = "6f1c2b3a-1111-4222-8333-444455556666"
	s := func(v string) *string { return &v }
	off, wide := 40, true
	req, err := lookRequest(id, LookWrite{Icon: s("📘"), Cover: s(""), CoverOffset: &off, FullWidth: &wide, Font: s("serif")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Query, liveDocFilter) || req.Vars["$id"] != id {
		t.Errorf("the doc isn't found live by its uuid: %s %v", req.Query, req.Vars)
	}
	if len(req.Mutations) != 2 {
		t.Fatalf("mutations: %d", len(req.Mutations))
	}
	var set map[string]any
	if err := json.Unmarshal(req.Mutations[0].SetJson, &set); err != nil {
		t.Fatal(err)
	}
	if set["uid"] != "uid(doc)" || set["doc_uuid"] != id || set["doc_icon"] != "📘" || set["doc_font"] != "serif" || set["doc_full_width"] != true {
		t.Errorf("set: %v", set)
	}
	if _, ok := set["doc_cover_offset"]; ok {
		t.Error("a cover taken away keeps a position")
	}
	del := string(req.Mutations[1].DelNquads)
	if !strings.Contains(del, "uid(doc) <doc_cover_key> * .") || !strings.Contains(del, "uid(doc) <doc_cover_offset> * .") {
		t.Errorf("del: %q", del)
	}
	for _, m := range req.Mutations {
		if m.Cond != "@if(eq(len(doc), 1))" {
			t.Errorf("a mutation runs without its doc: %q", m.Cond)
		}
	}

	// Only what is named changes.
	req, _ = lookRequest(id, LookWrite{CoverOffset: &off})
	set = nil
	_ = json.Unmarshal(req.Mutations[0].SetJson, &set)
	if len(req.Mutations) != 1 || len(set) != 3 || set["doc_cover_offset"] != float64(40) {
		t.Errorf("a new position alone: %v (%d mutations)", set, len(req.Mutations))
	}

	if _, err := lookRequest("not-a-doc", LookWrite{Icon: s("📘")}); !errors.Is(err, ErrNoDocToUpdate) {
		t.Errorf("no uuid: %v", err)
	}
}
