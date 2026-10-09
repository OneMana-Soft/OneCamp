package business

import (
	"context"
	"errors"
	"testing"
)

// A directory that sends a userName with characters outside ASCII is refused
// before anything is looked up: a lookup that folds case the Unicode way finds
// kate@example.com for "Kate@example.com".
func TestASCIMUserNameOutsideASCIIIsRefused(t *testing.T) {
	_, err := CreateUser(context.Background(), ScimUserResource{UserName: "Kate@example.com"}, "https://team.example.test/scim/v2")
	if !errors.Is(err, ErrScimUserNameNotASCII) {
		t.Fatalf("err = %v, want ErrScimUserNameNotASCII", err)
	}
}
