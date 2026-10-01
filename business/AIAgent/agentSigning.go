package business

// Agents sign what they do.
//
// Every effecting call an agent makes is recorded as an intent before it is
// attempted (agentActionLog.go). A row proved nothing about who wrote it:
// anyone with database access could insert or rewrite one. Each agent now has
// an Ed25519 key derived from the server's AI_CONFIG_KEK and its own id. It is
// never stored, so there is no key column an attacker could swap for their
// own, and someone holding only the database cannot produce a signature that
// verifies. Verification re-derives the public key from the same secret.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"time"

	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	actionLog "github.com/akashc777/OneCamp/models/postgres/AgentActionLog"
	"github.com/google/uuid"
)

// intentSignatureVersion prefixes the signed message, so the format can change
// without an old signature ever verifying against a new meaning.
const intentSignatureVersion = "onecamp-agent-action/v1"

// agentSigningKey is an agent's private key, derived, never stored.
func agentSigningKey(agentID uuid.UUID) ed25519.PrivateKey {
	seed := aiModels.DeriveSecret("agent-signing:" + agentID.String())
	return ed25519.NewKeyFromSeed(seed[:])
}

// AgentPublicKey is the key an agent's signatures verify against.
func AgentPublicKey(agentID uuid.UUID) ed25519.PublicKey {
	return agentSigningKey(agentID).Public().(ed25519.PublicKey)
}

// intentMessage is exactly what is signed: every field of the intent, in a
// fixed order, one per line. Pure.
func intentMessage(in actionLog.Intent) []byte {
	opt := func(id *uuid.UUID) string {
		if id == nil {
			return ""
		}
		return id.String()
	}
	return []byte(strings.Join([]string{
		intentSignatureVersion,
		in.ID.String(),
		in.AgentID.String(),
		opt(in.RunAsUserID),
		opt(in.RunID),
		in.ToolName,
		in.ParamsDigest,
		in.IntentAt.UTC().Format(time.RFC3339Nano),
	}, "\n"))
}

// signIntent stamps the intent's time (to the microsecond Postgres keeps) and
// its agent's signature.
func signIntent(in *actionLog.Intent, now time.Time) {
	in.IntentAt = now.UTC().Truncate(time.Microsecond)
	in.Signature = ed25519.Sign(agentSigningKey(in.AgentID), intentMessage(*in))
}

// SignatureReport is what checking an agent's recent actions found.
type SignatureReport struct {
	PublicKey string `json:"public_key"` // base64, for checking elsewhere
	Checked   int    `json:"checked"`
	Valid     int    `json:"valid"`
	// Unsigned rows were recorded before signing existed.
	Unsigned int `json:"unsigned"`
	// Invalid rows were altered or written by something without the key.
	Invalid  int      `json:"invalid"`
	Examples []string `json:"invalid_examples,omitempty"` // ids of a few invalid rows
}

// verifyIntents checks each intent against its agent's key. Pure.
func verifyIntents(agentID uuid.UUID, intents []actionLog.Intent) SignatureReport {
	pub := AgentPublicKey(agentID)
	r := SignatureReport{PublicKey: base64.StdEncoding.EncodeToString(pub)}
	for _, in := range intents {
		r.Checked++
		switch {
		case len(in.Signature) == 0:
			r.Unsigned++
		case in.AgentID == agentID && ed25519.Verify(pub, intentMessage(in), in.Signature):
			r.Valid++
		default:
			r.Invalid++
			if len(r.Examples) < 5 {
				r.Examples = append(r.Examples, in.ID.String())
			}
		}
	}
	return r
}

// listIntents is a seam.
var listIntents = actionLog.ListIntents

// VerifyAgentActions checks the signatures on an agent's most recent actions.
func VerifyAgentActions(ctx context.Context, agentID uuid.UUID, limit int) (SignatureReport, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	intents, err := listIntents(ctx, agentID, limit)
	if err != nil {
		return SignatureReport{}, err
	}
	return verifyIntents(agentID, intents), nil
}

// RecordActionIntentForTest records a signed intent the way the runner does,
// for tests that need real rows. Not used by the product.
func RecordActionIntentForTest(ctx context.Context, agentID uuid.UUID, tool string, now time.Time) (uuid.UUID, error) {
	in := actionLog.Intent{ID: uuid.New(), AgentID: agentID, ToolName: tool, ParamsDigest: "test"}
	signIntent(&in, now)
	return in.ID, actionLog.RecordIntent(ctx, in)
}

// AgentSignatures checks an agent's recent actions for the person managing it
// (its owner or an admin; same gate as the rest of the builder).
func AgentSignatures(ctx context.Context, id uuid.UUID, actor Actor) (SignatureReport, error) {
	if _, err := GetAgent(ctx, id, actor); err != nil {
		return SignatureReport{}, err
	}
	return VerifyAgentActions(ctx, id, 500)
}
