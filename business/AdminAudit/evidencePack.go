package business

// The evidence pack: one document that answers "what happened, when, by whom,
// and under which policy", for a window of time.
//
// The parts already existed and were separately verifiable. The audit log with
// its hash chain, the authorisation decisions including the refusals, the agent
// run ledger with the model and the instruction fingerprints that produced each
// run. What did not exist was anything that assembled them, and an auditor does
// not want four exports. They want a document.
//
// WHAT IT PROVES, stated precisely, because a compliance artefact that overstates
// itself is worse than none:
//
//   - the log has not been altered since it was written. Each entry hashes the
//     previous entry's hash, and the pack carries the recomputation.
//   - what the agent was TOLD, by fingerprint, so an instruction edited later
//     cannot silently rewrite what a past run appears to have been asked.
//   - which sections this deployment even has, so a pack with no agent section
//     reads as "runs no agents" rather than as "somebody removed the agents".
//
// WHAT IT DOES NOT PROVE. That the log recorded everything it should have. A
// hash chain is evidence of integrity, not of completeness, and no export can
// turn one into the other. That limit is written into the pack itself.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
)

// ManifestEntry fingerprints one section so the pack can be checked without
// trusting the tool that produced it: rehash the section, compare the digest.
type ManifestEntry struct {
	Section   string `json:"section"`
	Rows      int    `json:"rows"`
	SHA256    string `json:"sha256"`
	Describes string `json:"describes"`
	// Contextual marks a section describing the DEPLOYMENT rather than the
	// window, so a reader counting "what happened this month" can leave it out.
	// See helpers.EvidenceSection.Contextual.
	Contextual bool `json:"contextual,omitempty"`
}

// EvidencePack is the whole document.
type EvidencePack struct {
	Pack      PackMeta       `json:"pack"`
	Integrity PackIntegrity  `json:"integrity"`
	Sections  map[string]any `json:"sections"`
	// HowToVerify is the recomputation, written down. Without it the manifest
	// asks the reader to trust that the digests mean something; with it they can
	// check the document with a SHA-256 tool and no part of this software.
	HowToVerify []string `json:"how_to_verify"`
	Limits      []string `json:"limits"`
}

type PackMeta struct {
	GeneratedAt time.Time `json:"generated_at"`
	GeneratedBy string    `json:"generated_by"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	Product     string    `json:"product"`
}

type PackIntegrity struct {
	ChainVerification *auditModel.VerifyResult `json:"chain_verification"`
	Manifest          []ManifestEntry          `json:"manifest"`
	// PackFingerprint is the digest of the manifest, so one value identifies the
	// whole document. Two packs over the same window produce the same value.
	PackFingerprint string `json:"pack_fingerprint"`
}

// packHowToVerify travels inside every pack, because an auditor who has to read
// our source to check our evidence is being asked for the trust the evidence
// exists to replace.
//
// Every step is executable, and evidencePackVerifiable_test.go follows them
// against the real hash function: a change to the algorithm that does not change
// these words fails that test rather than quietly making the instructions wrong,
// which would be worse than shipping none.
var packHowToVerify = append(append([]string{}, rowHashRecipe...), []string{
	"Walk audit_log in order. Each row's prev_hash must equal the previous row's entry_hash, and the first row of the chain carries an empty prev_hash.",
	"Recompute a section digest: serialise that section to JSON with object keys sorted, SHA-256 the bytes, and compare with its sha256 in the manifest.",
	"Recompute pack_fingerprint the same way over the manifest array itself, so one value identifies the whole document.",
	mismatchMeans,
}...)

// productName is what both documents call themselves, so two exports of the
// same workspace cannot disagree about what produced them.
const productName = "OneCamp"

// rowHashRecipe is how one row's hash is recomputed, which is the only part of
// the verification that works on a single row in isolation.
//
// Shared rather than copied because two documents now carry it: the workspace
// evidence pack, which can also walk the chain, and a member's own proof, which
// cannot. A second copy of these sentences is a second thing to keep true when
// the hash changes, and the test that follows them against the real hash
// function only follows one of them.
var rowHashRecipe = []string{
	"Recompute a row hash: SHA-256 over these fields joined by a single newline, in order: prev_hash, id, created_at, actor_id, actor_email, action, category, summary, metadata. Rows that carry an actor_kind append one more newline and that value; rows written before that column existed do not.",
	"Format those fields as the document does: created_at is RFC 3339 in UTC with nanosecond precision and trailing zeros removed, an absent actor_id is the empty string, and metadata is the row's JSON re-serialised with object keys sorted.",
}

// mismatchMeans closes every verification list, because a reader who follows
// the steps and gets a different number needs to be told what that means.
const mismatchMeans = "A mismatch means the document you are holding is not the one this system produced. None of the above needs our software: any SHA-256 and JSON tool will do."

// packLimits travels inside every pack. Written here rather than in a brochure,
// because the place a claim is least likely to be read charitably is the place
// it has to be most honest.
var packLimits = []string{
	"The chain verification proves the log has not been altered since it was written. For the admin log in general it does not prove that every event was recorded; integrity and completeness are different properties and no export converts one into the other.",
	"Agent actions are the exception, and only those with an effect. Each is written to durable storage before it is attempted and refuses to run if that write fails, so an effecting agent action cannot have taken place without a record of it. Read-only agent calls are not covered: a lookup that went unrecorded cannot mean an unrecorded change.",
	"What is still unknown for an agent action is its OUTCOME, not its existence. If the process stopped between recording the intent and recording the result, the action may or may not have taken effect. Those are listed individually in the agent_actions_unresolved section rather than covered by this sentence.",
	"Sections are present only when this deployment ships the feature that produces them. A missing section means the feature is not installed, not that its records were removed.",
	"Redacted rows are counted separately from checked rows. Their content was cleared by the retention policy and cannot be recomputed from, so the chain takes those rows at their word.",
	"This is evidence for a reviewer to read. It is not a certification, and OneCamp is not certified against any standard.",
}

// BuildEvidencePack assembles the pack for a window.
//
// generatedBy is recorded in the document, because who asked for an evidence
// export is itself the kind of fact an evidence export exists to capture.
func BuildEvidencePack(ctx context.Context, from, to time.Time, generatedBy string) (*EvidencePack, error) {
	verification, err := Verify(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not verify the audit chain: %w", err)
	}

	entries, err := ExportEntries(ctx, "", &from, &to)
	if err != nil {
		return nil, fmt.Errorf("could not read the audit log: %w", err)
	}

	pack := &EvidencePack{
		Pack: PackMeta{
			GeneratedAt: time.Now().UTC(),
			GeneratedBy: generatedBy,
			From:        from.UTC(),
			To:          to.UTC(),
			Product:     productName,
		},
		Sections:    map[string]any{},
		HowToVerify: packHowToVerify,
		Limits:      packLimits,
	}

	manifest := []ManifestEntry{}
	add := func(name, describes string, rows int, contextual bool, payload any) error {
		digest, derr := fingerprint(payload)
		if derr != nil {
			return fmt.Errorf("could not fingerprint section %s: %w", name, derr)
		}
		pack.Sections[name] = payload
		manifest = append(manifest, ManifestEntry{
			Section: name, Rows: rows, SHA256: digest, Describes: describes, Contextual: contextual,
		})
		return nil
	}

	if aerr := add("audit_log",
		"Every recorded administrative and agent action in the window, in chain order, each carrying its own hash and the hash of the entry before it.",
		len(entries), false, entries); aerr != nil {
		return nil, aerr
	}

	// Whatever this edition links. See helpers.RegisterEvidenceContributor.
	for _, s := range helpers.EvidenceContributors() {
		rows, cerr := s.Collect(ctx, from, to)
		if cerr != nil {
			// One section failing must not deny the reviewer the rest. Record the
			// gap in the pack rather than dropping it silently, because a section
			// that is quietly missing is indistinguishable from one that is empty.
			helpers.LogErrorWithContext(ctx, "evidence pack: section %s failed: %+v", s.Name, cerr)
			if aerr := add(s.Name, s.Describe+" (THIS SECTION FAILED TO COLLECT AND IS INCOMPLETE.)", 0, s.Contextual, []any{}); aerr != nil {
				return nil, aerr
			}
			continue
		}
		if aerr := add(s.Name, s.Describe, countRows(rows), s.Contextual, rows); aerr != nil {
			return nil, aerr
		}
	}

	pack.Integrity = PackIntegrity{ChainVerification: verification, Manifest: manifest}
	packDigest, err := fingerprint(manifest)
	if err != nil {
		return nil, fmt.Errorf("could not fingerprint the pack: %w", err)
	}
	pack.Integrity.PackFingerprint = packDigest
	return pack, nil
}

// fingerprint digests a section's canonical JSON. Go marshals struct fields in
// declaration order and map keys sorted, so the same rows produce the same
// digest on any machine, which is the property that lets a reviewer check the
// pack without trusting the tool that made it.
func fingerprint(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return helpers.SHA256Hex(string(b)), nil
}

// countRows reports a section's length when it is a slice, and 1 otherwise, so
// the manifest can say how much a section contains without each contributor
// having to count itself.
func countRows(v any) int {
	switch t := v.(type) {
	case nil:
		return 0
	case []any:
		return len(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	var asSlice []json.RawMessage
	if json.Unmarshal(b, &asSlice) == nil {
		return len(asSlice)
	}
	return 1
}
