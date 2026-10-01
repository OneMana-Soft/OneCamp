package models

// The remote brain's credential, stored the way every credential here is.

import (
	"context"
	"strings"
	"sync"

	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	"github.com/google/uuid"
)

// Remote reports whether this agent's reasoning happens at an AG-UI endpoint
// rather than on a model this workspace calls.
func (a *AiAgent) Remote() bool {
	return a != nil && strings.TrimSpace(a.AGUIEndpoint) != ""
}

// The protocols a remote brain can speak.
const (
	RemoteAGUI = "agui"
	RemoteA2A  = "a2a"
)

// remoteProtocolOrDefault stores anything unset as AG-UI, the only protocol
// there was before A2A, so older rows and clients keep their meaning.
func remoteProtocolOrDefault(p string) string {
	if strings.TrimSpace(p) == RemoteA2A {
		return RemoteA2A
	}
	return RemoteAGUI
}

// IsA2A reports whether this agent's remote brain speaks A2A.
func (a *AiAgent) IsA2A() bool {
	return a != nil && remoteProtocolOrDefault(a.RemoteProtocol) == RemoteA2A
}

// encryptAGUISecret encrypts a non-empty secret for storage; an empty secret
// stores as NULL, which reads back as "none set".
func encryptAGUISecret(secret string) ([]byte, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, nil
	}
	return aiModels.EncryptAPIKey(secret)
}

// unreadableAGUILogged keeps the undecryptable-secret error to one line per
// agent per process; the condition is static until someone re-enters it.
var unreadableAGUILogged sync.Map

// readAGUISecret decrypts a stored secret. An undecryptable secret is exactly
// as unusable as none, so it reads as not set, with unreadable saying why: the
// agent must not go out with an empty credential to an endpoint that expects
// one. Mirrors models/AIMCP, which learned this the expensive way.
func readAGUISecret(agentID uuid.UUID, enc []byte) (secret string, set, unreadable bool) {
	if len(enc) == 0 {
		return "", false, false
	}
	plain, err := aiModels.DecryptAPIKey(enc)
	if err == nil {
		return plain, true, false
	}
	if _, already := unreadableAGUILogged.LoadOrStore(agentID, struct{}{}); !already {
		helpers.LogErrorWithContext(context.Background(),
			"models/AIAgent: the remote-agent secret for agent %s cannot be decrypted (usually an "+
				"AI_CONFIG_KEK change); the agent cannot run until the secret is re-entered: %v", agentID, err)
	}
	return "", false, true
}
