package business

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// THE WHOLE POINT OF THIS KIND IS THAT IT IS HONEST ABOUT BEING WEAKER. It carries no
// per-object check because there is no object. That is defensible, but only while it is
// impossible to mistake for an object check — so what these tests protect is the
// narrowness, not the permission.

// Only tools that create something owned by the caller, in no container, may use it.
// Anything that touches an existing object must name that object, or this kind becomes a
// way to bypass every rule in reach.go.
func TestOnlyContainerlessCreationsUseSelfOwned(t *testing.T) {
	allowed := map[string]string{
		"create_doc":   "creates a document owned by the caller, in no container",
		"set_reminder": "creates a reminder for the caller, in no container",
	}

	for _, b := range bridgedTools {
		if b.Kind != ResourceSelfOwned {
			continue
		}
		if _, ok := allowed[b.Tool]; !ok {
			t.Errorf("%q uses ResourceSelfOwned, which carries NO per-object authority "+
				"check. It is only for a tool that creates something owned by the caller "+
				"in no container. If %q touches anything that already exists, or anything "+
				"someone else owns, it must name that object and answer to its rule.",
				b.Tool, b.Tool)
		}
		// A read has no business here: the kind exists for creations.
		if b.Behaviour.ReadOnly {
			t.Errorf("%q is read-only but uses ResourceSelfOwned; a read names something "+
				"that exists and must be authorised against it", b.Tool)
		}
		// A creation is never naturally idempotent, and the dedup key is the main thing
		// this kind buys these tools.
		if b.Behaviour.Idempotent {
			t.Errorf("%q is declared idempotent, so it gets no deduplication key — which "+
				"is the reason to bring a containerless creation onto the governed path "+
				"at all", b.Tool)
		}
		// Destructive would need a human, and nothing owned-and-new can be destructive.
		if b.Behaviour.Destructive {
			t.Errorf("%q is declared destructive but creates something new; destructive "+
				"means it removes or overwrites, which needs an object to name", b.Tool)
		}
	}

	// And the allowlist must not rot: every tool named here must actually use the kind.
	inUse := map[string]bool{}
	for _, b := range bridgedTools {
		if b.Kind == ResourceSelfOwned {
			inUse[b.Tool] = true
		}
	}
	for tool := range allowed {
		if !inUse[tool] {
			t.Errorf("%q is listed here as a ResourceSelfOwned tool but no longer uses that "+
				"kind; remove it so this allowlist keeps meaning something", tool)
		}
	}
}

// The kind must declare no id argument, and the resolver must enforce that via the shared
// predicate rather than a special case per kind.
func TestSelfOwnedNamesNoIdArgument(t *testing.T) {
	for _, b := range bridgedTools {
		if b.Kind == ResourceSelfOwned && strings.TrimSpace(b.IDArg) != "" {
			t.Errorf("%q uses ResourceSelfOwned but declares id argument %q. The kind exists "+
				"precisely because there is no object to identify.", b.Tool, b.IDArg)
		}
	}

	if !ResourceSelfOwned.IdentifiesNoObject() {
		t.Error("ResourceSelfOwned does not report IdentifiesNoObject, so the resource " +
			"resolver would demand an id argument it cannot have")
	}
	if !ResourceWorkspace.IdentifiesNoObject() {
		t.Error("ResourceWorkspace does not report IdentifiesNoObject")
	}
	// Every other kind DOES name an object; a false positive here would let a tool skip
	// naming what it acts on.
	for _, k := range []ResourceKind{
		ResourceChannel, ResourceTask, ResourceDoc, ResourceChat, ResourceTable,
		ResourceProject, ResourceTeam, ResourceDataSource, ResourceDirectMessage,
	} {
		if k.IdentifiesNoObject() {
			t.Errorf("%s reports that it names no object, so a tool using it would not have "+
				"to say what it acts on", k)
		}
	}

	// The resolver must ask the predicate, not list the kinds.
	raw, err := os.ReadFile("bridge.go")
	if err != nil {
		t.Fatalf("read bridge.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")
	at := strings.Index(src, "func resourceResolver")
	if at < 0 {
		t.Fatal("resourceResolver is gone; this ratchet has gone stale")
	}
	body := src[at:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "IdentifiesNoObject()") {
		t.Error("resourceResolver does not use ResourceKind.IdentifiesNoObject; a third " +
			"kind that names no object would be added and forgotten there")
	}
}

// ResourceWorkspace must keep refusing writes. The new kind exists so that rule did not
// have to be weakened — if a workspace-scoped write became allowed, every workspace tool
// would gain a write path nobody authorised.
func TestWorkspaceStillRefusesWrites(t *testing.T) {
	raw, err := os.ReadFile("reach.go")
	if err != nil {
		t.Fatalf("read reach.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

	start := strings.Index(src, "case ResourceWorkspace")
	if start < 0 {
		t.Fatal("PrincipalCanReach has no ResourceWorkspace branch")
	}
	branch := src[start:]
	if i := strings.Index(branch, "\n\tcase "); i > 0 {
		branch = branch[:i]
	}
	if !strings.Contains(branch, "ref.Access.IsRead()") {
		t.Fatal("the workspace branch no longer refuses writes; a tool that changes an " +
			"existing object could be authorised without naming it")
	}
	if !strings.Contains(branch, "may not write") {
		t.Error("the workspace branch's write refusal has changed shape; it must still " +
			"refuse, because ResourceSelfOwned exists so this rule could stay strict")
	}
}

// The doc must describe the weaker guarantee rather than leaving a customer to infer that
// every tool gets an object check.
func TestTheDocExplainsTheContainerlessCreations(t *testing.T) {
	doc := mcpDoc(t)

	if !strings.Contains(doc, "name no container") {
		t.Error("the doc does not explain that two creations name no container. A customer " +
			"reading 'all 30 tools are governed' would assume every one gets a per-object " +
			"check, which is not true for these two.")
	}
	for _, tool := range []string{"create_doc", "set_reminder"} {
		if !strings.Contains(doc, "`"+tool+"`") {
			t.Errorf("the doc never names %q", tool)
		}
	}
}
