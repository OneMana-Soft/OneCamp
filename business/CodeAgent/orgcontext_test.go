package codeagent

import (
	"context"
	"strings"
	"testing"

	codepr "github.com/akashc777/OneCamp/business/CodePR"
)

// fakeProvider is a test OrgContextProvider that records the subject it was
// called with and returns a fixed set of fragments (or panics, to prove
// best-effort recovery).
type fakeProvider struct {
	got       ContextSubject
	fragments []codepr.ContextFragment
	panicNow  bool
}

func (f *fakeProvider) Fragments(_ context.Context, subj ContextSubject) []codepr.ContextFragment {
	f.got = subj
	if f.panicNow {
		panic("boom")
	}
	return f.fragments
}

// withProvider registers p for the duration of the test and restores the prior
// provider afterward, so tests don't leak global state into one another.
func withProvider(t *testing.T, p OrgContextProvider) {
	t.Helper()
	prev := orgContextProvider
	RegisterOrgContextProvider(p)
	t.Cleanup(func() { RegisterOrgContextProvider(prev) })
}

func TestAssembleOrgContext_OffSafeByDefault(t *testing.T) {
	// Force provider off regardless of process state.
	withProvider(t, nil)
	block, refs := assembleOrgContext(context.Background(), ContextSubject{Owner: "o", Repo: "r"})
	if block != "" || refs != nil {
		t.Fatalf("expected OFF-safe empty result, got block=%q refs=%v", block, refs)
	}
	if renderOrgContextSection(block) != "" {
		t.Fatalf("empty block must render to empty section")
	}
}

func TestAssembleOrgContext_ActiveProvider(t *testing.T) {
	fp := &fakeProvider{fragments: []codepr.ContextFragment{
		{Kind: codepr.KindOriginConversation, Title: "Thread", Body: "We decided to fix the retry loop.", Weight: 10},
		{Kind: codepr.KindMemory, Title: "Convention", Body: "Always wrap errors with %w.", Weight: 5},
	}}
	withProvider(t, fp)

	block, refs := assembleOrgContext(context.Background(), ContextSubject{Owner: "o", Repo: "r", Title: "t", Body: "b", Kind: "issue"})
	if block == "" {
		t.Fatal("expected a non-empty fused block")
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 source refs, got %d", len(refs))
	}
	// Higher-weight conversation fragment must come before the memory one.
	if !strings.Contains(block, "retry loop") || !strings.Contains(block, "wrap errors") {
		t.Fatalf("block missing fragment bodies: %q", block)
	}
	if strings.Index(block, "retry loop") > strings.Index(block, "wrap errors") {
		t.Fatalf("expected conversation (higher weight) before memory: %q", block)
	}
	// The subject must be passed through to the provider verbatim.
	if fp.got.Owner != "o" || fp.got.Repo != "r" || fp.got.Kind != "issue" {
		t.Fatalf("provider received wrong subject: %+v", fp.got)
	}
}

func TestAssembleOrgContext_EmptyFragmentsIsOffSafe(t *testing.T) {
	withProvider(t, &fakeProvider{fragments: nil})
	block, refs := assembleOrgContext(context.Background(), ContextSubject{Owner: "o", Repo: "r"})
	if block != "" || refs != nil {
		t.Fatalf("provider returning nothing must be OFF-safe, got block=%q", block)
	}
}

func TestAssembleOrgContext_ProviderPanicRecovers(t *testing.T) {
	withProvider(t, &fakeProvider{panicNow: true})
	// Must not panic; must degrade to empty (code-only).
	block, refs := assembleOrgContext(context.Background(), ContextSubject{Owner: "o", Repo: "r"})
	if block != "" || refs != nil {
		t.Fatalf("panicking provider must degrade to empty, got block=%q", block)
	}
}

func TestAssembleOrgContext_Bounded(t *testing.T) {
	// A single huge fragment must be clipped to the org-context budget so it
	// can never crowd out the code in the prompt.
	huge := strings.Repeat("x", orgContextMaxChars*4)
	withProvider(t, &fakeProvider{fragments: []codepr.ContextFragment{
		{Kind: codepr.KindDoc, Title: "Big", Body: huge, Weight: 1},
	}})
	block, _ := assembleOrgContext(context.Background(), ContextSubject{Owner: "o", Repo: "r"})
	if len(block) > orgContextMaxChars+512 { // +header/label slack
		t.Fatalf("org block not bounded: %d chars", len(block))
	}
}

func TestRenderOrgContextSection_FencesUntrusted(t *testing.T) {
	section := renderOrgContextSection("### Referenced doc: X\nsome text\n")
	if !strings.Contains(section, "UNTRUSTED DATA") {
		t.Fatalf("section must warn the model the context is untrusted: %q", section)
	}
	if !strings.Contains(section, "some text") {
		t.Fatalf("section must include the block body: %q", section)
	}
	if renderOrgContextSection("   ") != "" {
		t.Fatalf("blank block must render empty")
	}
}

func TestOrgContextFromContext_EnrichesScope(t *testing.T) {
	fp := &fakeProvider{fragments: []codepr.ContextFragment{
		{Kind: codepr.KindMemory, Title: "m", Body: "remember this", Weight: 1},
	}}
	withProvider(t, fp)

	ctx := WithConversation(WithActor(context.Background(), "user-123"), "chan-abc", "")
	_ = orgContextFromContext(ctx, ContextSubject{Owner: "o", Repo: "r", Kind: "issue"})

	if fp.got.ActorUUID != "user-123" {
		t.Fatalf("actor not threaded from context: %q", fp.got.ActorUUID)
	}
	if fp.got.ChannelUUID != "chan-abc" {
		t.Fatalf("channel not threaded from context: %q", fp.got.ChannelUUID)
	}
}

func TestOrgContextFromContext_SubjectScopeWins(t *testing.T) {
	fp := &fakeProvider{fragments: []codepr.ContextFragment{
		{Kind: codepr.KindMemory, Title: "m", Body: "x", Weight: 1},
	}}
	withProvider(t, fp)

	// An explicit subject scope must not be overwritten by context values.
	ctx := WithActor(context.Background(), "ctx-user")
	_ = orgContextFromContext(ctx, ContextSubject{Owner: "o", Repo: "r", ActorUUID: "subj-user"})
	if fp.got.ActorUUID != "subj-user" {
		t.Fatalf("explicit subject actor must win, got %q", fp.got.ActorUUID)
	}
}

func TestWithActor_EmptyIsNoop(t *testing.T) {
	ctx := WithActor(context.Background(), "  ")
	if s := scopeFromContext(ctx); s.actorUUID != "" {
		t.Fatalf("empty actor must be a no-op, got %q", s.actorUUID)
	}
}
