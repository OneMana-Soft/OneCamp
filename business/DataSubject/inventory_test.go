package business

import (
	"strings"
	"testing"

	"github.com/lib/pq"
)

// The database-backed parts are covered by running it against a real schema.
// What is tested here is the reasoning that decides WHAT gets counted, because
// that is where a silent wrong answer comes from: a column set that is too wide
// reports someone as present wherever they are merely referenced, and one that
// is too narrow reports an erasure as complete when it is not.

func TestOwnershipColumnsDoNotIncludeMereReferences(t *testing.T) {
	// channel_uuid, task_uuid and friends are uuid columns that name a THING,
	// not a person. Sweeping them in would report a user as present in every
	// row of every channel they ever posted in.
	for _, notOwnership := range []string{
		"channel_uuid", "task_uuid", "project_uuid", "chat_uuid",
		"doc_uuid", "post_uuid", "id", "workspace_id",
	} {
		for _, owned := range userColumnNames {
			if owned == notOwnership {
				t.Errorf("%q is a reference to a thing, not ownership by a person", notOwnership)
			}
		}
	}
}

func TestOwnershipColumnsAreDistinctAndNonEmpty(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range userColumnNames {
		if strings.TrimSpace(c) == "" {
			t.Error("empty column name in the ownership list")
		}
		if seen[c] {
			t.Errorf("duplicate column name %q: it would be counted twice", c)
		}
		seen[c] = true
	}
}

// The identifiers are interpolated because SQL has no parameter form for them.
// This pins that they go through quoting, so a table or column whose name needs
// escaping cannot change the shape of the statement.
func TestIdentifierQuotingIsApplied(t *testing.T) {
	for _, name := range []string{
		`users`,
		`weird"name`,
		`Mixed_Case`,
	} {
		q := pq.QuoteIdentifier(name)
		if !strings.HasPrefix(q, `"`) || !strings.HasSuffix(q, `"`) {
			t.Errorf("QuoteIdentifier(%q) = %q, expected it to be quoted", name, q)
		}
	}
	// An embedded quote must be doubled, not passed through.
	if got := pq.QuoteIdentifier(`weird"name`); got != `"weird""name"` {
		t.Errorf("embedded quote not escaped: got %q", got)
	}
}

func TestTotalRowsSumsEveryLocation(t *testing.T) {
	locations := []PersonalDataLocation{
		{Table: "posts", Column: "created_by", Rows: 3},
		{Table: "comments", Column: "created_by", Rows: 7},
	}
	if got := TotalRows(locations); got != 10 {
		t.Errorf("TotalRows = %d, want 10", got)
	}
	if got := TotalRows(nil); got != 0 {
		t.Errorf("TotalRows(nil) = %d, want 0", got)
	}
}

func TestSummarySaysNothingRatherThanShowingAnEmptyTable(t *testing.T) {
	if got := Summary(nil); !strings.Contains(got, "No personal data") {
		t.Errorf("Summary(nil) = %q, expected it to say plainly that nothing was found", got)
	}
	got := Summary([]PersonalDataLocation{{Table: "posts", Column: "created_by", Rows: 2}})
	for _, want := range []string{"posts", "created_by", "2"} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary missing %q in: %s", want, got)
		}
	}
}
