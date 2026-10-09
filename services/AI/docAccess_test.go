package ai

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

// What a write tells the index about a doc's privacy. An entry that exists is
// updated with only the fields a write sends, so a doc's entry and a doc
// comment's say whether the doc is private, and every other write says
// nothing, keeping what is stored. A write that didn't know used to say
// "public", which is how saving a private doc, or editing a comment on one,
// made it searchable by everyone.
func TestDocPrivacyIsWrittenOnlyByWhatKnowsIt(t *testing.T) {
	resetReindexBuffer()
	beginReindexBuffering() // the writes are captured here instead of indexed
	defer resetReindexBuffer()

	EmbedDocContent("Plans", "<p>reorg</p>", "doc-private", "owner", "Owner", true, "owner", []string{"reader"}, []string{"owner"}, nil)
	EmbedDocContent("Handbook", "<p>hello</p>", "doc-public", "owner", "Owner", false, "owner", nil, []string{"owner"}, nil)
	EmbedDocCommentContent("noted", "comment-1", "reader", "Reader", "doc-private", true, "owner", []string{"reader"}, []string{"owner"}, nil)

	deadline := time.Now().Add(5 * time.Second)
	var writes []reindexWrite
	for time.Now().Before(deadline) {
		reindexBufferMu.Lock()
		writes = append([]reindexWrite(nil), reindexBuffer...)
		reindexBufferMu.Unlock()
		if len(writes) == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(writes) != 3 {
		t.Fatalf("captured %d writes, want 3", len(writes))
	}
	stored := map[string]map[string]any{}
	for _, w := range writes {
		raw, _ := json.Marshal(w.doc)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		stored[w.doc.ContentUUID] = m
	}
	for id, want := range map[string]bool{"doc-private": true, "doc-public": false, "comment-1": true} {
		if got, ok := stored[id]["doc_private"]; !ok || got != want {
			t.Errorf("%s: doc_private = %v (sent: %v), want %v", id, got, ok, want)
		}
	}

	// A comment edited (the shape business/Comment.UpdateComment writes) and a
	// post say nothing about a doc's privacy.
	for name, doc := range map[string]EmbeddingDoc{
		"an edited comment": {ContentText: "noted!", ContentType: "comment", ContentUUID: "comment-1"},
		"a post":            {ContentText: "hi", ContentType: "post", ContentUUID: "post-1", ChannelUUID: "ch-1"},
	} {
		raw, _ := json.Marshal(doc)
		if strings.Contains(string(raw), "doc_private") {
			t.Errorf("%s writes doc_private: %s", name, raw)
		}
	}
}

// The update that writes docs' privacy and sharing onto their entries.
func TestDocAccessUpdateBody(t *testing.T) {
	if body, err := docAccessUpdateBody(nil); body != nil || err != nil {
		t.Fatalf("nothing to write: %s %v", body, err)
	}
	if body, err := docAccessUpdateBody([]DocAccess{{DocUUID: " "}}); body != nil || err != nil {
		t.Fatalf("a doc with no id: %s %v", body, err)
	}

	body, err := docAccessUpdateBody([]DocAccess{
		{DocUUID: "d1", Private: true, CreatedBy: "owner", Reading: []string{"r1"}},
		{DocUUID: "d2", Private: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Script struct {
			Source string `json:"source"`
			Lang   string `json:"lang"`
			Params struct {
				Docs map[string]struct {
					IsPrivate  bool     `json:"is_private"`
					CreatedBy  string   `json:"created_by"`
					Reading    []string `json:"reading"`
					Editing    []string `json:"editing"`
					Commenting []string `json:"commenting"`
				} `json:"docs"`
			} `json:"params"`
		} `json:"script"`
		Query json.RawMessage `json:"query"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if got.Script.Lang != "painless" || got.Script.Source != docAccessScript {
		t.Errorf("script: %+v", got.Script)
	}
	d1, d2 := got.Script.Params.Docs["d1"], got.Script.Params.Docs["d2"]
	if !d1.IsPrivate || d1.CreatedBy != "owner" || len(d1.Reading) != 1 || d1.Reading[0] != "r1" {
		t.Errorf("d1's access: %+v", d1)
	}
	// Nobody is an empty list, not null: a grant taken away is written away.
	if d2.IsPrivate || d2.Reading == nil || d2.Editing == nil || d2.Commenting == nil {
		t.Errorf("d2's access: %+v", d2)
	}
	if len(got.Script.Params.Docs) != 2 {
		t.Errorf("params name %d docs, want 2", len(got.Script.Params.Docs))
	}

	// The query reaches each doc's own entry, and its comments' whichever way
	// the index mapped doc_uuid.
	var q struct {
		Bool struct {
			Should []map[string]json.RawMessage `json:"should"`
			Min    int                          `json:"minimum_should_match"`
		} `json:"bool"`
	}
	if err := json.Unmarshal(got.Query, &q); err != nil || q.Bool.Min != 1 || len(q.Bool.Should) != 3 {
		t.Fatalf("query: %s (%v)", got.Query, err)
	}
	var fields []string
	for _, clause := range q.Bool.Should {
		for kind, raw := range clause {
			if kind == "terms" {
				var m map[string][]string
				_ = json.Unmarshal(raw, &m)
				for f, ids := range m {
					sort.Strings(ids)
					if strings.Join(ids, ",") != "d1,d2" {
						t.Errorf("%s names %v", f, ids)
					}
					fields = append(fields, f)
				}
			}
		}
	}
	sort.Strings(fields)
	if strings.Join(fields, ",") != "doc_uuid,doc_uuid.keyword" {
		t.Errorf("comment clauses on %v", fields)
	}
	if !strings.Contains(string(got.Query), `"content_uuid":["d1","d2"]`) || !strings.Contains(string(got.Query), `"content_type":"doc"`) {
		t.Errorf("the docs' own entries aren't asked for: %s", got.Query)
	}
}
