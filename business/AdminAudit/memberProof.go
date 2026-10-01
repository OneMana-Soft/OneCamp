package business

// The record of what was done in one person's name, in a form they can take
// away and a stranger can check.
//
// The workspace evidence pack is an administrator's document: the whole log,
// the chain walked end to end, every section fingerprinted. It answers an
// auditor. It does not answer the person an agent acted for, who cannot open
// the admin log at all and whose question is smaller and more personal: what
// was done as me, and can I show somebody that this is what the system
// actually recorded.
//
// So this is the same evidence, scoped to one principal, carrying the same row
// hash recipe the pack carries. What it deliberately does NOT carry is the
// chain walk, because these rows are a filter over the log and consecutive
// rows here are usually not consecutive there. Shipping the pack's
// instructions unchanged would hand somebody steps that cannot succeed, which
// is worse than shipping none: they would conclude the log was broken when it
// is only incomplete on purpose. The limits below say that in the document.

import (
	"context"
	"fmt"
	"time"

	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	"github.com/google/uuid"
)

// maxProofEntries bounds one proof. A person's own recent record, not an
// archive; an administrator exporting the log is the tool for that.
const maxProofEntries = 500

// MemberProof is the whole document.
type MemberProof struct {
	Proof   ProofMeta                `json:"proof"`
	Entries []*auditModel.AuditEntry `json:"entries"`
	// HowToVerify and Limits travel inside the document for the same reason
	// they travel inside the pack: a reader who has to read our source to check
	// our evidence is being asked for the trust the evidence exists to replace.
	HowToVerify []string `json:"how_to_verify"`
	Limits      []string `json:"limits"`
}

// ProofMeta says what this document covers and who it is about.
type ProofMeta struct {
	GeneratedAt time.Time `json:"generated_at"`
	// Subject is the person these rows were recorded against, by email when
	// there is one. The id is on every row already.
	Subject   string `json:"subject,omitempty"`
	SubjectID string `json:"subject_id"`
	Rows      int    `json:"rows"`
	// Truncated says the person has more rows than one proof carries, so a
	// reader is never left to infer completeness from a round number.
	Truncated bool   `json:"truncated,omitempty"`
	Product   string `json:"product"`
}

// proofHowToVerify is the pack's row recipe and nothing else, because nothing
// else works on a filtered document.
var proofHowToVerify = append(append([]string{}, rowHashRecipe...), []string{
	"Check one row at a time. Every row here carries the prev_hash it was written with, so its entry_hash can be recomputed from the row alone.",
	mismatchMeans,
}...)

// proofLimits travels inside every proof. The first one is the difference
// between this document and the administrator's, and it is first because a
// reader who misses it will try the wrong check and draw the wrong conclusion.
var proofLimits = []string{
	"These are only the rows recorded against you. They are a filter over the workspace log, so two rows next to each other here are usually not next to each other there, and the prev_hash of one will not be the entry_hash of the one above it. Walking the chain needs the whole log, which an administrator can export.",
	"Recomputing a row's hash proves that row is exactly as it was written. It does not prove that no row was removed; integrity and completeness are different properties and no export converts one into the other.",
	"A row marked redacted_at had its content cleared by the workspace retention policy. The row and its hashes remain so the chain is unbroken, and its hash can no longer be recomputed from its content, because the content is gone.",
	"Rows are recorded against the person whose authority an action carried. An agent's row names you because it acted as you, not because you were there; the initiator field on the row says whether anybody was.",
}

// capProofEntries trims the one extra row that was fetched to find out whether
// there are more, and says whether there were.
//
// Pure, and separate from the query, because "there are more rows than this"
// is the one claim in the document a reader cannot check for themselves. A
// count that lands exactly on the limit and says nothing is the failure this
// avoids.
func capProofEntries(entries []*auditModel.AuditEntry, limit int) ([]*auditModel.AuditEntry, bool) {
	if entries == nil {
		return []*auditModel.AuditEntry{}, false
	}
	if limit > 0 && len(entries) > limit {
		return entries[:limit], true
	}
	return entries, false
}

// BuildMemberProof assembles one person's proof.
//
// prefixes narrows the actions to a surface (the AI feed passes its own), and
// an empty list means every action recorded against them. Ordering, scoping
// and redaction all come from the one query the activity feed already uses, so
// a member's proof cannot show them something their feed would not.
func BuildMemberProof(ctx context.Context, principal uuid.UUID, subjectEmail string, prefixes []string, limit int) (*MemberProof, error) {
	if principal == uuid.Nil {
		// No principal is no proof, rather than everybody's rows. The one
		// direction this must never fail in.
		return nil, fmt.Errorf("a proof needs a person to be about")
	}
	if limit <= 0 || limit > maxProofEntries {
		limit = maxProofEntries
	}

	// One more than asked for, so "there are more" is observed rather than
	// guessed from the count landing exactly on the limit.
	entries, err := auditModel.ListByActionPrefixes(ctx, prefixes, &principal, limit+1)
	if err != nil {
		return nil, err
	}
	entries, truncated := capProofEntries(entries, limit)

	return &MemberProof{
		Proof: ProofMeta{
			GeneratedAt: time.Now().UTC(),
			Subject:     subjectEmail,
			SubjectID:   principal.String(),
			Rows:        len(entries),
			Truncated:   truncated,
			Product:     productName,
		},
		Entries:     entries,
		HowToVerify: proofHowToVerify,
		Limits:      proofLimits,
	}, nil
}
