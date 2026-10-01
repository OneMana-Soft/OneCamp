package models

import (
	"encoding/json"
	"testing"
	"time"

	"fmt"
	"github.com/google/uuid"
)

// entry builds a fixed entry for deterministic hashing.
func entry(action, summary string) *AuditEntry {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	return &AuditEntry{
		Id:        id,
		Action:    action,
		Category:  CategorySecurity,
		Summary:   summary,
		CreatedAt: time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC),
	}
}

func TestComputeAuditHash_Deterministic(t *testing.T) {
	e := entry("settings.update", "changed upload limit")
	h1 := computeAuditHash("", e)
	h2 := computeAuditHash("", e)
	if h1 != h2 {
		t.Fatal("hash must be deterministic for the same input")
	}
	if len(h1) != 64 {
		t.Fatalf("expected a 64-char sha256 hex, got %d chars", len(h1))
	}
}

// The chain is only worth keeping if the hash never moves. Every stored entry
// hashes the previous one, so a change to the algorithm does not invalidate the
// next link, it invalidates the whole log, retroactively and silently: verify
// would report tampering on entries nobody touched.
//
// The other tests here check that the hash is deterministic and sensitive, which
// a DIFFERENT algorithm would also satisfy. This one pins the actual value, so
// changing it has to be a deliberate act with a migration behind it. The value
// was captured before routing this through helpers.SHA256Hex and is unchanged
// by that refactor, which is what made the refactor safe.
func TestComputeAuditHash_GoldenValue(t *testing.T) {
	const golden = "e5e00e342fe85416a35c13a7c476290bc9a7f3007761110ffe62c7d77ead5233"
	if got := computeAuditHash("", entry("settings.update", "changed upload limit")); got != golden {
		t.Fatalf("the audit hash changed.\n got: %s\nwant: %s\n\n"+
			"Every stored entry chains off this. If the change is intended, every existing log "+
			"has to be rehashed, and until it is, verify will report tampering on untouched rows.", got, golden)
	}
}

func TestComputeAuditHash_SensitiveToContentAndChain(t *testing.T) {
	base := computeAuditHash("", entry("settings.update", "changed upload limit"))

	// Changing the summary changes the hash (tamper detection).
	if computeAuditHash("", entry("settings.update", "changed something else")) == base {
		t.Fatal("hash must change when content changes")
	}
	// Changing the previous hash changes the hash (chain linkage / reordering).
	if computeAuditHash("deadbeef", entry("settings.update", "changed upload limit")) == base {
		t.Fatal("hash must change when the previous hash changes")
	}
}

func TestComputeAuditHash_ChainLinks(t *testing.T) {
	// A two-entry chain: tampering entry 1 must change entry 2's expected hash,
	// which is exactly what Verify relies on to catch a deleted/edited row.
	e1 := entry("a", "first")
	h1 := computeAuditHash("", e1)
	e2 := entry("b", "second")
	h2 := computeAuditHash(h1, e2)

	tampered1 := entry("a", "first-TAMPERED")
	h1b := computeAuditHash("", tampered1)
	if h1b == h1 {
		t.Fatal("tampering entry 1 should change its hash")
	}
	// Recomputing entry 2 against the tampered prev yields a different hash than
	// the originally stored h2 -> divergence detected.
	if computeAuditHash(h1b, e2) == h2 {
		t.Fatal("entry 2 should not verify against a tampered predecessor")
	}
}

// Adding actor_kind must not disturb a single stored hash, and must still cover
// the new field once it is set. Those two requirements pull against each other,
// and appending only when non-empty is what satisfies both.
func TestActorKindIsHashedWithoutDisturbingHistory(t *testing.T) {
	e := entry("settings.update", "changed upload limit")

	// An entry from before the column existed carries no kind and must hash
	// exactly as it did, or verification reports tampering on rows nobody
	// touched.
	const golden = "e5e00e342fe85416a35c13a7c476290bc9a7f3007761110ffe62c7d77ead5233"
	if got := computeAuditHash("", e); got != golden {
		t.Fatalf("an entry with no actor kind changed hash:\n got %s\nwant %s", got, golden)
	}

	// A new entry's kind is inside the hash, so editing the row to hide that an
	// agent acted is caught by the same check as any other edit.
	withKind := *e
	withKind.ActorKind = "agent"
	agentHash := computeAuditHash("", &withKind)
	if agentHash == golden {
		t.Fatal("actor kind is not covered by the hash, so it could be removed undetected")
	}

	// And the three kinds must not collide with each other.
	seen := map[string]string{golden: "(none)"}
	for _, kind := range []string{"human", "agent", "system"} {
		k := *e
		k.ActorKind = kind
		h := computeAuditHash("", &k)
		if prev, clash := seen[h]; clash {
			t.Fatalf("actor kind %q hashes the same as %q", kind, prev)
		}
		seen[h] = kind
	}
}

// A chain that spans the migration boundary must still link.
//
// This is the scenario Verify actually meets on any deployment that existed
// before migration 154: rows with no actor_kind, then rows with one, in one
// chain, recomputed in order from seq 1. Every other test here checks a single
// hash; none of them would notice the boundary itself failing, and the boundary
// is the only thing this change introduced.
func TestAChainSpanningTheActorKindBoundaryStillVerifies(t *testing.T) {
	// Four entries: two written before the column existed, two after.
	kinds := []string{"", "", "human", "agent"}

	// Build the chain the way Insert does: each entry hashes the previous hash.
	stored := make([]string, len(kinds))
	entries := make([]*AuditEntry, len(kinds))
	prev := ""
	for i, kind := range kinds {
		e := entry("settings.update", "changed upload limit")
		// Distinct ids, or every link would hash identically and a broken chain
		// would pass by coincidence.
		e.Id = uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-00000000000%d", i+1))
		e.ActorKind = kind
		h := computeAuditHash(prev, e)
		e.EntryHash, e.PrevHash = h, prev
		stored[i], entries[i] = h, e
		prev = h
	}

	// Now verify the way Verify does: walk in order, recompute, compare.
	prev = ""
	for i, e := range entries {
		if got := computeAuditHash(prev, e); got != stored[i] {
			t.Fatalf("entry %d (kind %q) failed to verify:\n got %s\nwant %s",
				i, kinds[i], got, stored[i])
		}
		prev = stored[i]
	}

	// And the boundary must be load-bearing: changing a post-boundary entry's
	// kind has to break it, or the field is decorative.
	tampered := *entries[3]
	tampered.ActorKind = "human"
	if computeAuditHash(stored[2], &tampered) == stored[3] {
		t.Fatal("an agent action relabelled as human still verifies, so the field proves nothing")
	}
}

// withMeta builds the fixed entry carrying metadata, so the jsonb round trip has
// something to reorder.
func withMeta(raw string) *AuditEntry {
	e := entry("settings.update", "changed upload limit")
	e.Metadata = &raw
	return e
}

// The hash must survive Postgres handing the metadata back differently.
//
// metadata is jsonb. jsonb does not store the bytes it was given: it reorders
// object keys by length then bytewise and re-renders with a space after each colon
// and comma. Insert hashed Go's compact, alphabetically-keyed rendering; Verify
// recomputed from jsonb's. They never matched, so EVERY entry with metadata
// reported "audit chain broken: an entry was modified, inserted, or deleted" on a
// log nobody had touched -- all 33 rows on the installation where this was found,
// from the first one.
//
// One key is enough to show it: Go writes {"mode":"backend"}, jsonb returns
// {"mode": "backend"}.
func TestComputeAuditHash_SurvivesPostgresJSONBRoundTrip(t *testing.T) {
	cases := []struct{ name, asInserted, asReturned string }{
		{
			"a single key, differing only by the space jsonb adds",
			`{"mode":"backend"}`,
			`{"mode": "backend"}`,
		},
		{
			"several keys, which jsonb also reorders by length",
			`{"channel":"drill-finance","drill":true,"phase":"before","tool":"send_message"}`,
			`{"tool": "send_message", "drill": true, "phase": "before", "channel": "drill-finance"}`,
		},
		{
			"nested objects, reordered at both levels",
			`{"a":{"inner":1,"zz":2},"b":[1,2]}`,
			`{"b": [1, 2], "a": {"zz": 2, "inner": 1}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inserted := computeAuditHash("", withMeta(c.asInserted))
			returned := computeAuditHash("", withMeta(c.asReturned))
			if inserted != returned {
				t.Fatalf("the same metadata hashes differently after a jsonb round trip.\n"+
					"  as inserted %s -> %s\n  as returned %s -> %s\n\n"+
					"Verify recomputes from what Postgres returns, so this reports every row "+
					"with metadata as tampered with.", c.asInserted, inserted, c.asReturned, returned)
			}
		})
	}
}

// Canonicalising must not make the hash blind to the thing it exists to catch.
// Reordering is noise; a changed value is tampering.
func TestComputeAuditHash_StillCatchesEditedMetadata(t *testing.T) {
	base := computeAuditHash("", withMeta(`{"mode":"backend","refused":true}`))

	for _, edited := range []string{
		`{"mode": "backend", "refused": false}`,            // the outcome flipped
		`{"mode": "frontend", "refused": true}`,            // a different value
		`{"mode": "backend"}`,                              // a key removed
		`{"mode": "backend", "refused": true, "extra": 1}`, // a key added
	} {
		if computeAuditHash("", withMeta(edited)) == base {
			t.Errorf("editing metadata to %s did not change the hash; tampering would go undetected", edited)
		}
	}
}

// Metadata that is not JSON we can parse is hashed verbatim, which is the old
// behaviour. It can only fail closed: an unparseable value that changes still
// changes the hash.
func TestComputeAuditHash_NonJSONMetadataIsHashedVerbatim(t *testing.T) {
	a := computeAuditHash("", withMeta("not json at all"))
	b := computeAuditHash("", withMeta("not json at all"))
	if a != b {
		t.Fatal("unparseable metadata must still hash deterministically")
	}
	if a == computeAuditHash("", withMeta("not json either")) {
		t.Fatal("unparseable metadata must still be sensitive to its content")
	}
}

// Canonicalising must be the IDENTITY on Go's own marshal output, because that is
// what every hash already in the database was computed over.
//
// This is the backward-compatibility guarantee, and it is the property that
// actually needs UseNumber. Comparing two jsonb-shaped strings to each other does
// not test it: both go through the same decode, so both are wrong together and the
// test passes while a large integer silently re-renders as 9.007199254740992e+15
// and every stored hash containing one stops matching.
func TestCanonicalJSONIsIdentityOnGoMarshalOutput(t *testing.T) {
	cases := []map[string]interface{}{
		{"mode": "backend"},
		{"channel": "drill-finance", "drill": true, "phase": "before", "tool": "send_message"},
		{"nested": map[string]interface{}{"zz": 2, "inner": "x"}, "list": []interface{}{1, "a"}},
		// Past float64's exact-integer range, which is where a naive decode loses.
		{"n": int64(9007199254740993)},
		{"ratio": 1.5, "zero": 0, "empty": "", "nothing": nil},
		// Characters Go escapes on the way out and must escape again on the way back.
		{"html": "<b>&</b>", "unicode": "caf\u00e9"},
	}
	for _, meta := range cases {
		asInserted, err := json.Marshal(meta)
		if err != nil {
			t.Fatalf("marshalling the fixture: %v", err)
		}
		if got := canonicalJSON(string(asInserted)); got != string(asInserted) {
			t.Errorf("canonicalJSON changed Go's own output, so hashes already stored over it "+
				"no longer recompute.\n  stored over: %s\n  canonical:   %s", asInserted, got)
		}
	}
}

// And the round trip holds for a large integer specifically: what Postgres returns
// must canonicalise back to exactly what Go inserted.
func TestComputeAuditHash_PreservesLargeIntegers(t *testing.T) {
	asInserted, err := json.Marshal(map[string]interface{}{"n": int64(9007199254740993)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const asReturned = `{"n": 9007199254740993}` // jsonb's rendering
	if computeAuditHash("", withMeta(string(asInserted))) != computeAuditHash("", withMeta(asReturned)) {
		t.Fatal("a large integer hashed differently across the round trip; numbers are being " +
			"decoded as float64 somewhere")
	}
}
