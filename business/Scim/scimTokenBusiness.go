// Package business (Scim) implements SCIM 2.0 provisioning: the credential that authenticates a
// directory, and the mapping between SCIM's user model and OneCamp's.
package business

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	scimModel "github.com/akashc777/OneCamp/models/postgres/Scim"
	"github.com/google/uuid"
)

const (
	// tokenPlainPrefix marks a SCIM credential apart from an /v1 api token ("oc_"). Distinguishable at
	// a glance matters when the value is pasted into an IdP configuration field: a credential in the
	// wrong box produces a 401 with nothing to indicate which of the two systems rejected it.
	tokenPlainPrefix = "ocscim_"
	// displayPrefixLen is how much of the secret is stored for recognition. Long enough to tell two
	// credentials apart in a list, far short of guessable.
	displayPrefixLen = len(tokenPlainPrefix) + 8
	maxNameLen       = 120
	// secretBytes is 160 bits from crypto/rand. This credential can create and deactivate accounts, so
	// it is sized as a root credential rather than as a session value.
	secretBytes = 20
)

// ErrScimTokenNotFound lets a controller answer 404 without matching on message text.
var ErrScimTokenNotFound = errors.New("scim credential not found")

// CreatedScimToken is returned once, at creation. Plaintext exists only in this response.
type CreatedScimToken struct {
	Token     *scimModel.ScimToken `json:"token"`
	Plaintext string               `json:"plaintext"`
}

// hashScimToken returns the SHA-256 hex of a credential's plaintext.
//
// Unsalted, deliberately. The input is 160 bits of CSPRNG output rather than a human-chosen password, so
// there is no dictionary a precomputed table could be built from, and the lookup must be by exact hash
// for authentication to be one indexed read. This mirrors business/ApiToken.hashToken rather than
// inventing a second scheme.
func hashScimToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// generateScimSecret returns a new credential plaintext.
func generateScimSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return tokenPlainPrefix + hex.EncodeToString(buf), nil
}

// CreateScimToken mints a credential, stores its hash, and returns the plaintext once.
// expiresInDays <= 0 means it does not expire.
func CreateScimToken(ctx context.Context, name string, expiresInDays int, createdBy uuid.UUID) (*CreatedScimToken, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name is required")
	}
	if len(name) > maxNameLen {
		return nil, errors.New("name is too long")
	}

	plaintext, err := generateScimSecret()
	if err != nil {
		return nil, errors.New("could not generate a credential")
	}

	var expiresAt *time.Time
	if expiresInDays > 0 {
		t := time.Now().AddDate(0, 0, expiresInDays)
		expiresAt = &t
	}

	prefix := plaintext
	if len(prefix) > displayPrefixLen {
		prefix = prefix[:displayPrefixLen]
	}

	id, err := scimModel.CreateScimToken(ctx, name, hashScimToken(plaintext), prefix, createdBy, expiresAt)
	if err != nil {
		return nil, errors.New("could not store the credential")
	}

	createdByCopy := createdBy
	now := time.Now()
	return &CreatedScimToken{
		Token: &scimModel.ScimToken{
			Id: id, Name: name, TokenPrefix: prefix, CreatedBy: &createdByCopy,
			ExpiresAt: expiresAt, CreatedAt: now, UpdatedAt: now,
		},
		Plaintext: plaintext,
	}, nil
}

// ValidateScimToken authenticates a presented credential and returns its row.
//
// Returns (nil, nil) for anything not recognised — wrong prefix, unknown hash, revoked, expired — so the
// middleware answers 401 identically in every case and the response cannot be used to learn whether a
// credential exists.
func ValidateScimToken(ctx context.Context, plaintext string) (*scimModel.ScimToken, error) {
	plaintext = strings.TrimSpace(plaintext)
	// Prefix check first: it rejects an /v1 api token, or a pasted session cookie, without spending a
	// database round trip on a value that cannot possibly be one of these.
	if plaintext == "" || !strings.HasPrefix(plaintext, tokenPlainPrefix) {
		return nil, nil
	}

	row, err := scimModel.GetActiveScimTokenByHash(ctx, hashScimToken(plaintext))
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}

	// Constant-time confirmation that the stored prefix belongs to the presented secret.
	//
	// The hash lookup has already authenticated the caller, so this cannot admit anyone. It catches the
	// other direction: a row whose prefix does not match the secret that resolved it is a row written
	// by something other than CreateScimToken, and treating it as a valid credential would mean trusting
	// a value this code never issued.
	if len(row.TokenPrefix) > 0 && len(plaintext) >= len(row.TokenPrefix) {
		if subtle.ConstantTimeCompare([]byte(row.TokenPrefix), []byte(plaintext[:len(row.TokenPrefix)])) != 1 {
			return nil, nil
		}
	}

	return row, nil
}

// TouchScimToken records a credential's last use, in the background.
//
// Unthrottled, unlike the /v1 equivalent, because the traffic profile is different: a directory syncs on
// a schedule measured in minutes, not a per-request integration loop, so there is no write storm to
// damp and adding a Redis dependency would buy nothing.
func TouchScimToken(ctx context.Context, id uuid.UUID) {
	scimModel.TouchScimTokenLastUsed(ctx, id)
}

// ListScimTokens returns every credential, without secrets.
func ListScimTokens(ctx context.Context) ([]*scimModel.ScimToken, error) {
	return scimModel.ListScimTokens(ctx)
}

// RevokeScimToken revokes a credential. Answers ErrScimTokenNotFound when there was nothing live to revoke.
func RevokeScimToken(ctx context.Context, id uuid.UUID) error {
	if err := scimModel.RevokeScimToken(ctx, id); err != nil {
		return ErrScimTokenNotFound
	}
	return nil
}
