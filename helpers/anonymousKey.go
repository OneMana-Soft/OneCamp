package helpers

import (
	"crypto/hmac"
	"crypto/sha256"
)

// AnonymousKey stands for someone within a scope without naming them: an HMAC
// of who under this server's secret. The same person in the same scope always
// gets the same key, so an anonymous vote is still one vote and an anonymous
// question still shows its asker it's theirs; without the secret, nobody,
// whoever reads the database included, can turn a key back into a person
// (calls package A's anonymous polls, questions and upvotes). A scope keeps
// keys from matching across polls and calls. The HMAC's key is the server's
// secret for this purpose alone (PurposeKey).
func AnonymousKey(scope, who string) ([]byte, error) {
	purpose, err := PurposeKey("onecamp.anonymous.v1")
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, purpose)
	m.Write([]byte(scope))
	m.Write([]byte{0})
	m.Write([]byte(who))
	return m.Sum(nil), nil
}
