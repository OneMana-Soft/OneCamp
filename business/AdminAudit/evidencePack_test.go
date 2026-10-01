package business

import (
	"encoding/json"
	"testing"
)

// The manifest is what makes the pack checkable without trusting the tool that
// produced it: rehash a section, compare the digest. That only holds if the same
// content always produces the same digest.
func TestFingerprintIsStableAndSensitive(t *testing.T) {
	rows := []map[string]any{{"b": 2, "a": 1}, {"a": 3}}

	first, err := fingerprint(rows)
	if err != nil {
		t.Fatalf("fingerprint failed: %v", err)
	}
	second, err := fingerprint(rows)
	if err != nil {
		t.Fatalf("fingerprint failed: %v", err)
	}
	if first != second {
		t.Fatal("the same rows produced two digests, so no reviewer could ever verify a pack")
	}
	if len(first) != 64 {
		t.Fatalf("digest is %d chars, want 64 hex", len(first))
	}

	// Map key order must not matter, or the digest would depend on Go's internal
	// iteration and two identical packs would disagree.
	reordered := []map[string]any{{"a": 1, "b": 2}, {"a": 3}}
	if again, _ := fingerprint(reordered); again != first {
		t.Fatal("digest changed when map keys were written in a different order")
	}

	// And it has to notice a change, or it proves nothing at all.
	altered := []map[string]any{{"b": 2, "a": 1}, {"a": 4}}
	if changed, _ := fingerprint(altered); changed == first {
		t.Fatal("altering a row did not change the digest")
	}
}

// The manifest says how much each section holds, so the count has to be right
// for the shapes sections actually return.
func TestCountRows(t *testing.T) {
	type run struct {
		ID string `json:"id"`
	}
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"nil section", nil, 0},
		{"empty slice", []*run{}, 0},
		{"typed slice", []*run{{ID: "a"}, {ID: "b"}}, 2},
		{"any slice", []any{1, 2, 3}, 3},
		{"a single object is one row, not zero", map[string]any{"ok": true}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := countRows(c.in); got != c.want {
				t.Fatalf("countRows = %d, want %d", got, c.want)
			}
		})
	}
}

// A compliance artefact that overstates itself is worse than none, so the limits
// travel inside the document rather than in a brochure that can be separated
// from it.
func TestPackCarriesItsOwnLimits(t *testing.T) {
	if len(packLimits) == 0 {
		t.Fatal("a pack with no stated limits invites the reader to assume there are none")
	}
	joined := ""
	for _, l := range packLimits {
		joined += l + " "
	}
	for _, must := range []string{"completeness", "not certified"} {
		if !contains(joined, must) {
			t.Errorf("the limits do not mention %q, which is the claim most likely to be over-read", must)
		}
	}
	// And they must survive marshalling, since that is the only form anyone sees.
	b, err := json.Marshal(&EvidencePack{Limits: packLimits})
	if err != nil {
		t.Fatalf("pack does not marshal: %v", err)
	}
	if !contains(string(b), "completeness") {
		t.Fatal("the limits did not survive into the serialised pack")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
