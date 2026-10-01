package business

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	"github.com/google/uuid"
)

// A proof needs a person to be about. Built for nobody it would be everybody's
// rows, which is the one direction this must never fail in.
func TestAProofWithNoPersonIsRefused(t *testing.T) {
	if _, err := BuildMemberProof(context.Background(), uuid.Nil, "", nil, 10); err == nil {
		t.Fatal("a proof was built for no principal")
	}
}

// The instructions inside a member's proof must be ones that work on a member's
// proof. The pack can walk the chain; this document is a filter over the log,
// so consecutive rows here are usually not consecutive there and the walk
// cannot succeed. Shipping steps that cannot succeed is worse than shipping
// none: a reader would conclude the log is broken when it is only filtered.
func TestAProofDoesNotTellYouToWalkAChainItDoesNotContain(t *testing.T) {
	joined := strings.Join(proofHowToVerify, " ")
	if strings.Contains(joined, "Walk audit_log") {
		t.Error("the proof tells a reader to walk the chain, which this document cannot support")
	}
	// It does carry the part that works on one row, and it is the SAME text the
	// pack carries, not a paraphrase.
	for _, step := range rowHashRecipe {
		if !strings.Contains(joined, step) {
			t.Errorf("the row hash recipe is missing or reworded: %q", step)
		}
	}
	if !strings.Contains(joined, mismatchMeans) {
		t.Error("the proof does not say what a mismatch means")
	}

	// And the limits say why, first, because a reader who misses it runs the
	// wrong check.
	if len(proofLimits) == 0 {
		t.Fatal("a proof with no limits is a claim with no edges")
	}
	if !strings.Contains(proofLimits[0], "only the rows recorded against you") ||
		!strings.Contains(proofLimits[0], "prev_hash") {
		t.Errorf("the first limit does not explain the filtering: %q", proofLimits[0])
	}
	if !strings.Contains(strings.Join(proofLimits, " "), "integrity and completeness are different") {
		t.Error("the proof does not distinguish integrity from completeness")
	}
	if !strings.Contains(strings.Join(proofLimits, " "), "redacted_at") {
		t.Error("the proof does not say a redacted row cannot be recomputed")
	}
}

// The pack and the proof must not drift on how a row is hashed, which is why
// there is one copy of those sentences.
func TestThePackAndTheProofShareOneRowRecipe(t *testing.T) {
	packJoined := strings.Join(packHowToVerify, " ")
	for _, step := range rowHashRecipe {
		if !strings.Contains(packJoined, step) {
			t.Errorf("the pack no longer carries the shared recipe: %q", step)
		}
	}
	// The pack keeps the chain walk; that is the difference between them.
	if !strings.Contains(packJoined, "Walk audit_log") {
		t.Error("the pack lost the chain walk, which only it can offer")
	}
}

// The document has to survive being a file: whatever a reader opens must carry
// the steps and the limits, not just the rows.
func TestAProofCarriesItsOwnInstructions(t *testing.T) {
	b, err := json.Marshal(&MemberProof{HowToVerify: proofHowToVerify, Limits: proofLimits})
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"how_to_verify", "limits", "SHA-256"} {
		if !strings.Contains(string(b), must) {
			t.Errorf("a serialised proof is missing %q", must)
		}
	}
}

// One extra row is fetched so "there are more" is observed rather than guessed
// from the count landing exactly on the limit, which is the one claim in the
// document a reader cannot check for themselves.
func TestAProofSaysWhenThereIsMoreThanItCarries(t *testing.T) {
	rows := func(n int) []*auditModel.AuditEntry {
		out := make([]*auditModel.AuditEntry, n)
		for i := range out {
			out[i] = &auditModel.AuditEntry{Seq: int64(i)}
		}
		return out
	}

	kept, truncated := capProofEntries(rows(11), 10)
	if len(kept) != 10 || !truncated {
		t.Errorf("eleven rows capped at ten: kept=%d truncated=%v", len(kept), truncated)
	}
	if kept, truncated := capProofEntries(rows(10), 10); len(kept) != 10 || truncated {
		t.Errorf("exactly ten rows must not read as truncated: kept=%d truncated=%v", len(kept), truncated)
	}
	if kept, truncated := capProofEntries(rows(3), 10); len(kept) != 3 || truncated {
		t.Errorf("fewer rows than the limit: kept=%d truncated=%v", len(kept), truncated)
	}
	// No rows is a real answer, and must serialise as an empty list rather than
	// a null a reader has to interpret.
	kept, truncated = capProofEntries(nil, 10)
	if kept == nil || len(kept) != 0 || truncated {
		t.Errorf("no rows: kept=%v truncated=%v", kept, truncated)
	}
	if b, _ := json.Marshal(&MemberProof{Entries: kept}); !strings.Contains(string(b), `"entries":[]`) {
		t.Errorf("an empty proof serialises entries as null: %s", b)
	}
}
