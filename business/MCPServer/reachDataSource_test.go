package business

import (
	"os"
	"regexp"
	"strings"
	"testing"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
)

// dataSourceBranch returns the ResourceDataSource arm of PrincipalCanReach, comments
// stripped, for the ordering assertions below.
func dataSourceBranch(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("reach.go")
	if err != nil {
		t.Fatalf("read reach.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")
	start := strings.Index(src, "case ResourceDataSource")
	if start < 0 {
		t.Fatal("PrincipalCanReach has no ResourceDataSource branch")
	}
	rest := src[start:]
	if i := strings.Index(rest, "\n\tcase "); i > 0 {
		rest = rest[:i]
	}
	return rest
}

// A WRITE MUST BE REFUSED BEFORE ANY LOOKUP. The connectors are read-only by construction
// and no tool offers a write, so a write request against an external source is a mistake
// or an attempt. Refusing it before touching the source means neither costs a query
// against a customer's warehouse, and it cannot be answered by a rule written for reads.
func TestDataSourceWritesAreRefusedBeforeAnyLookup(t *testing.T) {
	branch := dataSourceBranch(t)

	refusal := strings.Index(branch, "ref.Access.IsRead()")
	if refusal < 0 {
		t.Fatal("the data-source branch never consults ref.Access, so a WRITE would be " +
			"authorised by the read rule — against an external database")
	}
	for _, later := range []string{"dataSourceActorFor(", "CanQuery("} {
		if at := strings.Index(branch, later); at >= 0 && at < refusal {
			t.Errorf("%s runs before the write refusal; a write request would cause a "+
				"lookup against the customer's warehouse before being refused", later)
		}
	}
}

// The rule must be DELEGATED, not restated. A second copy of "admin, creator, or
// workspace-visible" here would be a second answer the first time either is edited — and
// this is the one kind where a divergence exposes a warehouse.
func TestDataSourceAuthorityIsDelegatedNotReimplemented(t *testing.T) {
	branch := dataSourceBranch(t)

	if !strings.Contains(branch, "dataSourceBusiness.CanQuery(") {
		t.Error("the data-source branch does not delegate to dataSourceBusiness.CanQuery, " +
			"so its authority rule is a copy that can drift from the one the app applies")
	}
	// The distinctive shape of the underlying rule must NOT appear inline.
	for _, inlined := range []string{"VisibilityWorkspace", "CreatedBy ==", "IsAdmin ||"} {
		if strings.Contains(branch, inlined) {
			t.Errorf("the data-source branch reimplements the visibility rule (%q). Delete "+
				"it and rely on CanQuery, or the two will disagree", inlined)
		}
	}
}

// Forbidden and not-found must collapse. Telling them apart lets a caller enumerate which
// warehouses a workspace has connected by probing ids.
func TestDataSourceRefusalDoesNotRevealExistence(t *testing.T) {
	branch := dataSourceBranch(t)

	// Exactly one refusal reason follows the CanQuery check, and it must not branch on
	// the error's kind.
	at := strings.Index(branch, "CanQuery(")
	if at < 0 {
		t.Skip("covered by TestDataSourceAuthorityIsDelegatedNotReimplemented")
	}
	after := branch[at:]
	for _, telltale := range []string{"IsNotFound(", "IsForbidden(", "errNotFound", "errForbidden"} {
		if strings.Contains(after, telltale) {
			t.Errorf("the refusal distinguishes %s, which lets a caller tell 'this source "+
				"exists but is not yours' from 'no such source' — a probe for which "+
				"warehouses are connected", telltale)
		}
	}
}

// THE POSTGRES ACTOR IS NOT INCIDENTAL. The rule tests an admin flag whose authoritative
// copy lives in Postgres; the graph's copy is overwritten by the auth middlewares and can
// be stale. Deciding warehouse access from a stale flag is the worst version of this bug.
func TestDataSourceUsesThePostgresActor(t *testing.T) {
	branch := dataSourceBranch(t)

	if !strings.Contains(branch, "dataSourceActorFor(") {
		t.Error("the data-source branch does not resolve a Postgres actor; using the " +
			"graph's admin flag would decide warehouse access from a value nothing keeps " +
			"current")
	}
	if strings.Contains(branch, "dgraphUser.IsAdmin") {
		t.Error("the data-source branch reads the graph's admin flag, which can be stale")
	}
}

// Every data-source tool must be governed by a kind that can express it, and the one that
// names no source must be workspace-scoped with its narrowing done in the handler.
func TestDataSourceToolsAreBoundToTheRightKind(t *testing.T) {
	byTool := map[string]binding{}
	for _, b := range bridgedTools {
		byTool[b.Tool] = b
	}

	for _, tool := range []string{"read_data_source", "query_data_source", "query_data_source_plan"} {
		b, ok := byTool[tool]
		if !ok {
			t.Errorf("%q is not governed", tool)
			continue
		}
		if b.Kind != ResourceDataSource {
			t.Errorf("%q is bound to %s; it names a source and must answer to the source's "+
				"own rule", tool, b.Kind)
		}
		if b.IDArg != "data_source_uuid" {
			t.Errorf("%q resolves its object from %q, so the authorizer would check a "+
				"different source than the call touches", tool, b.IDArg)
		}
		if !b.Behaviour.ReadOnly {
			t.Errorf("%q is declared as a write; external sources are read-only", tool)
		}
	}

	if b, ok := byTool["list_data_sources"]; !ok {
		t.Error("list_data_sources is not governed")
	} else if b.Kind != ResourceWorkspace {
		t.Errorf("list_data_sources is bound to %s; it names no source, so it is "+
			"workspace-scoped and its executor must narrow the list itself", b.Kind)
	}

	// And the whole group is reachable only when an admin has enabled it, which requires
	// the group to exist in the admin's choices.
	found := false
	for _, g := range AllToolGroups() {
		if g == "data_sources" {
			found = true
		}
	}
	if !found {
		t.Error("the data_sources group is not offerable to an admin, so these tools " +
			"would be gated on a group nobody can enable")
	}
	// Sanity: the scope really is its own, not folded into tables.
	if s, _ := apiTokenBusiness.ScopeForTool("query_data_source"); s == "tables:read" {
		t.Error("query_data_source shares the tables scope; a token granted table access " +
			"would reach external warehouses")
	}
}
