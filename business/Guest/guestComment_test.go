package business

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// TestSanitizeGuestCommentBody_StripsHTML is the core anti-XSS guarantee: a
// guest comment is rendered inside authenticated member sessions, so every HTML
// tag (especially <script>) must be removed before storage.
func TestSanitizeGuestCommentBody_StripsHTML(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"script tag", `hello <script>alert('xss')</script> world`},
		{"img onerror", `<img src=x onerror="steal()">look`},
		{"anchor", `click <a href="https://evil.test">here</a>`},
		{"bold", `<b>bold</b> text`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := sanitizeGuestCommentBody(c.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.ContainsAny(out, "<>") {
				t.Fatalf("sanitized body still contains angle brackets: %q", out)
			}
			if strings.Contains(strings.ToLower(out), "script") && strings.Contains(c.in, "<script") {
				// The word "script" only existed inside the tag; it must be gone.
				t.Fatalf("script content survived sanitization: %q", out)
			}
		})
	}
}

// TestSanitizeGuestCommentBody_Empty rejects empty / whitespace-only / tag-only
// input (nothing meaningful to store).
func TestSanitizeGuestCommentBody_Empty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t ", "<p></p>", "<div>   </div>"} {
		if _, err := sanitizeGuestCommentBody(in); !errors.Is(err, errEmptyComment) {
			t.Fatalf("input %q: expected errEmptyComment, got %v", in, err)
		}
	}
}

// TestSanitizeGuestCommentBody_LengthCap bounds a single comment so a leaked
// comment link can't be abused as bulk storage.
func TestSanitizeGuestCommentBody_LengthCap(t *testing.T) {
	long := strings.Repeat("a", maxGuestCommentLen+500)
	out, err := sanitizeGuestCommentBody(long)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != maxGuestCommentLen {
		t.Fatalf("expected length cap at %d, got %d", maxGuestCommentLen, len(out))
	}
}

// TestSanitizeGuestCommentBody_PlainPreserved keeps legitimate plain text
// intact (no over-eager stripping of normal punctuation).
func TestSanitizeGuestCommentBody_PlainPreserved(t *testing.T) {
	in := "Looks good! Can we move the deadline to 5th? Budget: 1,200."
	out, err := sanitizeGuestCommentBody(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != in {
		t.Fatalf("plain text altered: got %q want %q", out, in)
	}
}

// TestCreateGuestDocComment_ViewOnlyRejected enforces R3.2: a view-only grant
// cannot post a comment. Returns before any DB write, so it needs no DB.
func TestCreateGuestDocComment_ViewOnlyRejected(t *testing.T) {
	os.Setenv("GUEST_ACCESS_ENABLED", "true")
	defer os.Unsetenv("GUEST_ACCESS_ENABLED")

	grant := &guestModel.GuestGrant{
		Id:           uuid.New(),
		ResourceType: guestModel.ResourceDoc,
		ResourceID:   "doc-1",
		Capability:   guestModel.CapabilityView,
	}
	if _, err := CreateGuestDocComment(context.Background(), grant, "Jane", "hi"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for a view-only grant, got %v", err)
	}
}

// TestCreateGuestDocComment_NonDocRejected ensures a board/table grant can't be
// used to post a doc comment. Returns before any DB write.
func TestCreateGuestDocComment_NonDocRejected(t *testing.T) {
	os.Setenv("GUEST_ACCESS_ENABLED", "true")
	defer os.Unsetenv("GUEST_ACCESS_ENABLED")

	grant := &guestModel.GuestGrant{
		Id:           uuid.New(),
		ResourceType: guestModel.ResourceBoard,
		ResourceID:   "board-1",
		Capability:   guestModel.CapabilityComment,
	}
	if _, err := CreateGuestDocComment(context.Background(), grant, "Jane", "hi"); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("expected ErrInvalidGrant for a non-doc grant, got %v", err)
	}
}

// TestCreateGuestDocComment_DisabledRejected ensures the workspace policy gate
// fires first: with guest access off, no comment can be posted.
func TestCreateGuestDocComment_DisabledRejected(t *testing.T) {
	os.Setenv("GUEST_ACCESS_ENABLED", "false")
	defer os.Unsetenv("GUEST_ACCESS_ENABLED")

	grant := &guestModel.GuestGrant{
		Id:           uuid.New(),
		ResourceType: guestModel.ResourceDoc,
		ResourceID:   "doc-1",
		Capability:   guestModel.CapabilityComment,
	}
	if _, err := CreateGuestDocComment(context.Background(), grant, "Jane", "hi"); !errors.Is(err, ErrGuestDisabled) {
		t.Fatalf("expected ErrGuestDisabled when policy is off, got %v", err)
	}
}
