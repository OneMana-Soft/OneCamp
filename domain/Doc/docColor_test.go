package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Colouring is one write for every doc named (docs F1, POST /doc/color): by
// uuid, only on the live ones, the colour set, or removed for none; and each
// doc's uuid written again, as every doc update writes it, so a delete in
// flight conflicts and is run again. A colour that isn't a camp hue, more
// than ColorCap docs, or no doc at all is refused before anything is sent.
func TestColouringIsOneWriteOnTheLiveDocsNamed(t *testing.T) {
	a, b := "5f0c4a2e-8a63-4f3e-9d2b-1c7a9e0b3d41", "6a1d5b3f-9b74-4a4f-8e3c-2d8b0f1c4e52"
	q, muts, err := colorRequest([]string{a, b, a, "not-a-uuid"}, "sky")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, `docs as var(func: eq(doc_uuid, ["`+a+`", "`+b+`"])) @filter(`+liveDocFilter+`) { u as doc_uuid }`) || !strings.Contains(q, "colored(func: uid(docs)) { doc_uuid }") {
		t.Errorf("the read: %s", q)
	}
	if len(muts) != 1 || muts[0].Cond != "@if(gt(len(docs), 0))" || !strings.Contains(muts[0].SetNQuads, `uid(docs) <doc_color> "sky" .`) ||
		!strings.Contains(muts[0].SetNQuads, "uid(docs) <doc_uuid> val(u) .") || muts[0].DelNQuads != "" {
		t.Errorf("a colour's write: %+v", muts)
	}
	_, muts, err = colorRequest([]string{a}, "")
	if err != nil || len(muts) != 1 || !strings.Contains(muts[0].DelNQuads, "uid(docs) <doc_color> * .") ||
		strings.Contains(muts[0].SetNQuads, "doc_color") || !strings.Contains(muts[0].SetNQuads, "uid(docs) <doc_uuid> val(u) .") {
		t.Errorf("no colour's write: %+v %v", muts, err)
	}
	if _, _, err := colorRequest([]string{a}, "red"); !errors.Is(err, ErrBadColor) {
		t.Errorf("a colour that isn't a hue: %v", err)
	}
	if _, _, err := colorRequest([]string{"x"}, "sky"); !errors.Is(err, ErrNoDocToUpdate) {
		t.Errorf("no doc: %v", err)
	}
	many := make([]string, ColorCap+1)
	for i := range many {
		many[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	if _, _, err := colorRequest(many, "sky"); !errors.Is(err, ErrTooManyToColor) {
		t.Errorf("%d docs: %v", len(many), err)
	}
}

// A link is named after its preview only while it is still called by its
// address (docs F1): the write is held to a live doc titled from, so a
// rename someone made meanwhile wins; and it writes the doc's uuid again, as
// every doc update does.
func TestALinkIsRetitledOnlyWhileItIsCalledByItsAddress(t *testing.T) {
	id := "5f0c4a2e-8a63-4f3e-9d2b-1c7a9e0b3d41"
	q, vars, muts, err := retitleRequest(id, "https://docs.google.com/d/1", "Q3 roadmap")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, "live as var(func: eq(doc_uuid, $id)) @filter("+liveDocFilter+") { title as doc_title }") ||
		!strings.Contains(q, "doc as var(func: uid(live)) @filter(eq(val(title), $from))") || strings.Contains(q, "eq(doc_title") ||
		vars["$id"] != id || vars["$from"] != "https://docs.google.com/d/1" {
		t.Errorf("the read: %s %v", q, vars)
	}
	if len(muts) != 1 || muts[0].Cond != "@if(eq(len(doc), 1))" || !strings.Contains(muts[0].SetNQuads, `uid(doc) <doc_title> "Q3 roadmap" .`) ||
		!strings.Contains(muts[0].SetNQuads, `uid(doc) <doc_uuid> "`+id+`" .`) {
		t.Errorf("the write: %+v", muts)
	}
	if _, _, _, err := retitleRequest("x", "a", "b"); !errors.Is(err, ErrNoDocToUpdate) {
		t.Errorf("no doc: %v", err)
	}
}
