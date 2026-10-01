package domain

import (
	"strings"
	"testing"
)

func TestBuildOwnedOpenItemsQuery(t *testing.T) {
	// Without a kind filter: status-only, bound $owner var, limit applied.
	q := buildOwnedOpenItemsQuery("", 25)
	for _, want := range []string{
		`query q($owner: string)`,
		`items(func: eq(user_uuid, $owner))`,
		`~mem_owner @filter(eq(mem_status, "open"))`,
		`first: 25`,
		`orderdesc: mem_created_at`,
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("owned query missing %q:\n%s", want, q)
		}
	}
	// A stray second @filter would be a DQL error — ensure single filter block.
	if strings.Count(q, "@filter(") != 1 {
		t.Fatalf("owned query must have exactly one @filter block:\n%s", q)
	}

	// With a kind: the filter is a SINGLE combined AND clause, not two @filters.
	qk := buildOwnedOpenItemsQuery("decision", 10)
	if !strings.Contains(qk, `eq(mem_status, "open") AND eq(mem_kind, "decision")`) {
		t.Fatalf("kind filter not combined with AND:\n%s", qk)
	}
	if strings.Count(qk, "@filter(") != 1 {
		t.Fatalf("kind query must still have exactly one @filter block:\n%s", qk)
	}
}

func TestBuildScopeOpenItemsQuery(t *testing.T) {
	q := buildScopeOpenItemsQuery("ch_uuid", "~mem_channel", 100)
	for _, want := range []string{
		`query q($scope: string)`,
		`anchor(func: eq(ch_uuid, $scope))`,
		`~mem_channel @filter(eq(mem_status, "open"))`,
		`first: 100`,
		`mem_owner { user_uuid user_name user_full_name }`,
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("scope query missing %q:\n%s", want, q)
		}
	}

	// Project scope swaps the anchor predicate + reverse edge.
	qp := buildScopeOpenItemsQuery("project_uuid", "~mem_project", 50)
	if !strings.Contains(qp, `anchor(func: eq(project_uuid, $scope))`) || !strings.Contains(qp, `~mem_project @filter`) {
		t.Fatalf("project scope query wrong:\n%s", qp)
	}
}
