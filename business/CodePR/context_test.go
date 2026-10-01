package codepr

import (
	"strings"
	"testing"
)

func TestAssembleContext_Empty(t *testing.T) {
	// OFF-safe: no fragments → empty bundle (code-only prompt unchanged).
	if b, used := AssembleContext(nil, 10000); b != "" || used != nil {
		t.Fatalf("no fragments must yield empty bundle, got %q / %v", b, used)
	}
	// All-empty bodies also yield nothing.
	f := []ContextFragment{{Kind: KindMemory, Body: "   "}}
	if b, _ := AssembleContext(f, 10000); b != "" {
		t.Fatalf("whitespace-only fragments must be ignored, got %q", b)
	}
	// Non-positive budget yields nothing.
	if b, _ := AssembleContext([]ContextFragment{{Kind: KindMemory, Body: "x"}}, 0); b != "" {
		t.Fatalf("zero budget must yield empty, got %q", b)
	}
}

func TestAssembleContext_OrdersByWeightThenKind(t *testing.T) {
	frags := []ContextFragment{
		{Kind: KindMemory, Title: "mem", Body: "memory item", Weight: 1},
		{Kind: KindOriginConversation, Title: "thread", Body: "the bug discussion", Weight: 5},
		{Kind: KindLinkedTask, Title: "task", Body: "the task text", Weight: 5},
	}
	bundle, used := AssembleContext(frags, 10000)
	// Both weight-5 items precede the weight-1 memory; on the weight tie the
	// origin conversation precedes the linked task (kind priority).
	iConv := strings.Index(bundle, "the bug discussion")
	iTask := strings.Index(bundle, "the task text")
	iMem := strings.Index(bundle, "memory item")
	if !(iConv < iTask && iTask < iMem) {
		t.Fatalf("wrong ordering: conv=%d task=%d mem=%d\n%s", iConv, iTask, iMem, bundle)
	}
	if len(used) != 3 || used[0].Kind != KindOriginConversation {
		t.Fatalf("used sources wrong: %+v", used)
	}
	// Section headers render.
	if !strings.Contains(bundle, "### Originating conversation: thread") {
		t.Fatalf("missing labeled header:\n%s", bundle)
	}
}

func TestAssembleContext_Dedup(t *testing.T) {
	frags := []ContextFragment{
		{Kind: KindMemory, Body: "we always use X for Y", Weight: 1},
		{Kind: KindMemory, Body: "We  always   use X for Y", Weight: 1}, // same after normalization
	}
	_, used := AssembleContext(frags, 10000)
	if len(used) != 1 {
		t.Fatalf("near-duplicate fragments must dedup, got %d", len(used))
	}
}

func TestAssembleContext_BudgetOmitsAndDiscloses(t *testing.T) {
	big := strings.Repeat("A", 5000)
	frags := []ContextFragment{
		{Kind: KindOriginConversation, Title: "t1", Body: big, Weight: 9},
		{Kind: KindLinkedTask, Title: "t2", Body: big, Weight: 8},
		{Kind: KindDoc, Title: "t3", Body: big, Weight: 7},
		{Kind: KindMemory, Title: "t4", Body: big, Weight: 1},
	}
	bundle, used := AssembleContext(frags, 1200)
	if len(bundle) > 1400 {
		t.Fatalf("bundle exceeded budget: %d chars", len(bundle))
	}
	if !strings.Contains(bundle, "omitted to fit the budget") {
		t.Fatalf("must disclose omitted items:\n%s", bundle)
	}
	// The highest-weight fragment is the one that survives first.
	if len(used) == 0 || used[0].Title != "t1" {
		t.Fatalf("highest-weight fragment should be included first: %+v", used)
	}
}

func TestAssembleContext_PerFragmentFairShare(t *testing.T) {
	// One giant + several small: the giant is head-truncated to its fair share
	// so it doesn't crowd out the others.
	frags := []ContextFragment{
		{Kind: KindOriginConversation, Title: "huge", Body: strings.Repeat("H", 100000), Weight: 5},
		{Kind: KindLinkedTask, Title: "small", Body: "short task", Weight: 5},
	}
	bundle, used := AssembleContext(frags, 4000)
	if !strings.Contains(bundle, "…[truncated]") {
		t.Fatalf("huge fragment should be truncated:\n%s", bundle[:200])
	}
	if !strings.Contains(bundle, "short task") {
		t.Fatalf("small fragment must still be included despite the huge one:\n%s", bundle)
	}
	if len(used) != 2 {
		t.Fatalf("both fragments should be included, got %d", len(used))
	}
}

func TestAssembleContext_UnknownKindFallback(t *testing.T) {
	bundle, _ := AssembleContext([]ContextFragment{{Kind: "weird", Body: "x", Weight: 1}}, 1000)
	if !strings.Contains(bundle, "### Context") {
		t.Fatalf("unknown kind should fall back to a generic header:\n%s", bundle)
	}
}
