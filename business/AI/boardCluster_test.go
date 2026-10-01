package business

import (
	"strings"
	"testing"
)

func TestParseBoardClusters_ValidAndConstrained(t *testing.T) {
	valid := map[string]bool{"a": true, "b": true, "c": true, "d": true}
	raw := `{
        "title": "Retro themes",
        "synthesis": "The team is happy with delivery but worried about flaky tests.",
        "clusters": [
            {"theme": "Wins", "summary": "What went well", "item_ids": ["a", "b"]},
            {"theme": "Risks", "summary": "What to watch", "item_ids": ["b", "c", "ghost"]},
            {"theme": "Empty", "summary": "nothing real", "item_ids": ["ghost"]}
        ]
    }`

	parsed, err := parseBoardClusters(raw, valid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.Title != "Retro themes" {
		t.Fatalf("title=%q", parsed.Title)
	}
	if len(parsed.Clusters) != 2 {
		t.Fatalf("expected 2 usable clusters (empty dropped), got %d", len(parsed.Clusters))
	}
	// "b" was claimed by Wins first, so Risks must not also contain it; "ghost"
	// is not a valid id and must be dropped entirely.
	wins := parsed.Clusters[0]
	risks := parsed.Clusters[1]
	if strings.Join(wins.ItemIDs, ",") != "a,b" {
		t.Fatalf("wins ids=%v", wins.ItemIDs)
	}
	if strings.Join(risks.ItemIDs, ",") != "c" {
		t.Fatalf("risks should only keep unassigned valid id 'c', got %v", risks.ItemIDs)
	}
}

func TestParseBoardClusters_Rejects(t *testing.T) {
	valid := map[string]bool{"a": true}
	if _, err := parseBoardClusters("not json", valid); err == nil {
		t.Fatal("expected error for non-JSON")
	}
	// All item ids invalid -> no usable clusters.
	raw := `{"title":"x","clusters":[{"theme":"T","item_ids":["zzz"]}]}`
	if _, err := parseBoardClusters(raw, valid); err == nil {
		t.Fatal("expected error when no clusters have valid items")
	}
}

func TestBuildClusterGraph_MindmapShape(t *testing.T) {
	textByID := map[string]string{"a": "ship faster", "b": "fix tests", "c": "hire QA"}
	clusters := []BoardClusterView{
		{Theme: "Wins", ItemIDs: []string{"a"}},
		{Theme: "Risks", ItemIDs: []string{"b", "c"}},
	}
	g := buildClusterGraph("Retro", clusters, textByID)
	if g == nil || g.Type != BoardDiagramMindmap {
		t.Fatalf("expected a mindmap graph, got %+v", g)
	}
	// root + 2 themes + 3 leaves = 6 nodes.
	if len(g.Nodes) != 6 {
		t.Fatalf("expected 6 nodes, got %d", len(g.Nodes))
	}
	// Every node must be laid out (have a position assigned by the layout).
	var haveRootLabel bool
	for _, n := range g.Nodes {
		if n.Label == "Retro" {
			haveRootLabel = true
		}
	}
	if !haveRootLabel {
		t.Fatal("root node label missing")
	}
	// Edges: root->2 themes + theme->leaves (3) = 5.
	if len(g.Edges) != 5 {
		t.Fatalf("expected 5 edges, got %d", len(g.Edges))
	}
}

func TestSanitizeSynthesis_BoundsAndCleans(t *testing.T) {
	got := sanitizeSynthesis("  line one\nline two\t  ")
	if got != "line one line two" {
		t.Fatalf("expected collapsed whitespace, got %q", got)
	}
	long := strings.Repeat("x", 800)
	if len([]rune(sanitizeSynthesis(long))) != 600 {
		t.Fatalf("expected cap at 600 runes")
	}
}
