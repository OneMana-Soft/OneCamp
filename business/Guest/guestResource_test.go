package business

import (
	"context"
	"os"
	"testing"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// TestGuestCollabTokenRoundtrip verifies a minted guest collab JWT carries the
// exact grant scope and survives a verify+parse with the same secret.
func TestGuestCollabTokenRoundtrip(t *testing.T) {
	secret := "test-secret-guest-collab"
	os.Setenv("JWT_SECRET", secret)
	defer os.Unsetenv("JWT_SECRET")

	grant := &guestModel.GuestGrant{
		Id:           uuid.New(),
		ResourceType: guestModel.ResourceDoc,
		ResourceID:   "doc-abc-123",
	}

	tok, err := IssueGuestCollabToken(context.Background(), grant, "Jane Doe")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	claims, err := parseGuestCollabClaims(tok, secret)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.GrantID != grant.Id {
		t.Fatalf("grant id mismatch: got %s want %s", claims.GrantID, grant.Id)
	}
	if claims.ResourceType != "doc" || claims.ResourceID != "doc-abc-123" {
		t.Fatalf("scope mismatch: %+v", claims)
	}
}

// TestGuestCollabTokenWrongSecretRejected ensures a token signed with one
// secret cannot be verified with another (forged/tampered token).
func TestGuestCollabTokenWrongSecretRejected(t *testing.T) {
	os.Setenv("JWT_SECRET", "secret-A")
	defer os.Unsetenv("JWT_SECRET")
	grant := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceBoard, ResourceID: "b1"}
	tok, err := IssueGuestCollabToken(context.Background(), grant, "x")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := parseGuestCollabClaims(tok, "secret-B"); err == nil {
		t.Fatal("expected verification failure with wrong secret")
	}
}

// TestNonGuestTokenRejected ensures a validly-signed token WITHOUT the guest
// marker is rejected by the guest parser (a member token can't be replayed as
// a guest token).
func TestNonGuestTokenRejected(t *testing.T) {
	secret := "secret-member"
	claims := jwt.MapClaims{"sub": uuid.NewString(), "exp": 9999999999}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := parseGuestCollabClaims(signed, secret); err == nil {
		t.Fatal("expected non-guest token to be rejected")
	}
}
