package helpers

import (
	"crypto/sha256"
	"encoding/hex"
)

// SHA256Hex is the fingerprint used wherever we need to prove that a piece of
// text is the same text later, without storing a second copy of it.
//
// It exists because "what exactly was this system told" is a question that
// outlives the text itself. An agent run keeps the fingerprint of the prompt it
// was given, so an edit to a skill months later cannot silently rewrite what a
// past run appears to have been asked. And because a fingerprint is not content,
// it survives a retention sweep that clears the transcript: the record can still
// answer WHICH instructions produced a run after it can no longer show them.
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
