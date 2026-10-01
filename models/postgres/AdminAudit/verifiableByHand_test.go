package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The evidence pack tells an auditor how to recompute a row hash. This follows
// those instructions, by hand, and checks they arrive at the same value the
// product does.
//
// WHY THIS AND NOT A COMMENT. The pack's how_to_verify is the only part of the
// system an outsider is asked to act on, and instructions that are subtly wrong
// are worse than none: a reviewer who cannot reproduce our digest concludes the
// log is fabricated, which is precisely the conclusion the pack exists to
// prevent. A change to computeAuditHash that does not change those words fails
// here.
//
// The steps, quoted from the pack:
//
//	prev_hash, id, created_at, actor_id, actor_email, action, category, summary,
//	metadata, joined by a single newline; rows carrying an actor_kind append one
//	more newline and that value. created_at is RFC 3339 in UTC with nanosecond
//	precision and trailing zeros removed, an absent actor_id is the empty string,
//	and metadata is the row's JSON re-serialised with object keys sorted.
func byHand(prev string, e *AuditEntry) string {
	actorID := ""
	if e.ActorID != nil {
		actorID = e.ActorID.String()
	}

	meta := ""
	if e.Metadata != nil {
		var v any
		dec := json.NewDecoder(strings.NewReader(*e.Metadata))
		dec.UseNumber()
		if err := dec.Decode(&v); err == nil {
			if out, err := json.Marshal(v); err == nil {
				meta = string(out)
			} else {
				meta = *e.Metadata
			}
		} else {
			meta = *e.Metadata
		}
	}

	fields := []string{
		prev,
		e.Id.String(),
		e.CreatedAt.UTC().Format(time.RFC3339Nano),
		actorID,
		e.ActorEmail,
		e.Action,
		e.Category,
		e.Summary,
		meta,
	}
	if e.ActorKind != "" {
		fields = append(fields, e.ActorKind)
	}
	sum := sha256.Sum256([]byte(strings.Join(fields, "\n")))
	return hex.EncodeToString(sum[:])
}

func sampleEntry(t *testing.T) *AuditEntry {
	t.Helper()
	id := uuid.MustParse("6f1d6d2c-5f6a-4f6b-9a2c-2b9f1f6d0a11")
	actor := uuid.MustParse("c224fb1b-67ca-4919-8d60-60d5910aacf8")
	meta := `{"tool":"channel.post","reason":"not a member","decision":"refused"}`
	return &AuditEntry{
		Id:         id,
		CreatedAt:  time.Date(2026, 9, 15, 10, 30, 0, 123456000, time.UTC),
		ActorID:    &actor,
		ActorEmail: "priya@example.test",
		Action:     "mcp.tool_call.refused",
		Category:   "agent",
		Summary:    "post in #finance refused",
		Metadata:   &meta,
	}
}

func TestAnAuditorFollowingThePackArrivesAtOurHash(t *testing.T) {
	e := sampleEntry(t)
	const prev = "0b7f3f1d9c2a"

	if got, want := byHand(prev, e), computeAuditHash(prev, e); got != want {
		t.Fatalf("the published instructions do not reproduce the hash.\n by hand: %s\nproduct: %s\n"+
			"Either computeAuditHash changed, or how_to_verify in business/AdminAudit/evidencePack.go did, "+
			"and the two must move together.", got, want)
	}
}

// An entry written before the actor_kind column existed hashes without it, and
// the instructions say so. Getting this wrong would make every historical row
// unverifiable for an outside reader.
func TestTheInstructionsHandleARowWithNoActorKind(t *testing.T) {
	e := sampleEntry(t)
	e.ActorKind = ""
	if got, want := byHand("", e), computeAuditHash("", e); got != want {
		t.Errorf("a row with no actor_kind does not follow the published steps:\nby hand: %s\nproduct: %s", got, want)
	}
}

func TestTheInstructionsHandleARowWithAnActorKind(t *testing.T) {
	e := sampleEntry(t)
	e.ActorKind = "agent"
	if got, want := byHand("x", e), computeAuditHash("x", e); got != want {
		t.Errorf("a row carrying actor_kind does not follow the published steps:\nby hand: %s\nproduct: %s", got, want)
	}
}

// Metadata is stored as jsonb, which re-renders what it was given: key order and
// whitespace can come back different from how they went in. The pack tells the
// reader to sort keys, and the product canonicalises the same way, or two honest
// parties would compute different hashes for the same row.
func TestKeyOrderInMetadataDoesNotChangeTheHash(t *testing.T) {
	a := sampleEntry(t)
	reordered := `{"reason":"not a member","decision":"refused","tool":"channel.post"}`
	b := sampleEntry(t)
	b.Metadata = &reordered

	if computeAuditHash("p", a) != computeAuditHash("p", b) {
		t.Error("the same metadata in a different key order produced a different hash; " +
			"an auditor re-serialising the JSON could never reproduce our digest")
	}
}
