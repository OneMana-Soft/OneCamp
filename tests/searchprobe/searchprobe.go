//go:build integration

// Package searchprobe runs the search as a person, as the search endpoint
// runs it, for any package's integration tests. It lives outside
// tests/integration: it imports the search's business packages, and the
// harness imports no application package (helpers'
// TestTheIntegrationHarnessImportsNoApplicationPackage), since packages with
// integration tests of their own import the harness.
package searchprobe

import (
	"context"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	searchQuery "github.com/akashc777/OneCamp/business/SearchQuery"
)

// SearchAs is one page of a search for text as reach's person, as the search
// endpoint runs it: the text built in search mode, then the engine.
func SearchAs(t *testing.T, ctx context.Context, reach globalSearchBusiness.Reach, text string, page globalSearchBusiness.Page) globalSearchBusiness.Result {
	t.Helper()
	who := searchQuery.Asker{Subject: accessBusiness.Subject{UserUUID: reach.UserUUID}, Email: reach.Email}
	spec, issues := searchQuery.Build(ctx, who, text, querySpec.Scope{Kind: querySpec.ScopeSearch}, searchQuery.ModeSearch)
	if !searchQuery.Runs(issues) {
		t.Fatalf("%q doesn't run: %+v", text, issues)
	}
	r, err := globalSearchBusiness.Search(ctx, reach, spec, page)
	if err != nil {
		t.Fatalf("searching %q: %v", text, err)
	}
	// Never "N results" beside fewer rows: a page counts at least what it
	// shows, and the last one exactly that.
	if r.Total < len(r.Page) || (!r.HasMore && r.Total != len(r.Page)) {
		t.Errorf("searching %q: a page of %d says %d in all (has more: %v)", text, len(r.Page), r.Total, r.HasMore)
	}
	return r
}
