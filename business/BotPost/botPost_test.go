package botpost

import (
	"context"
	"testing"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/google/uuid"
)

// TestPostCommentToPostAsBotValidation covers the input guards that run BEFORE
// any datastore access, so they are safe to assert without a DB. An empty body
// or an unresolved bot principal must be rejected up front so the caller can
// fall back to a top-level post rather than persisting a broken comment.
func TestPostCommentToPostAsBotValidation(t *testing.T) {
	ctx := context.Background()
	validBot := &userBusiness.BotIdentity{DgraphUID: "0x1", UserID: uuid.Nil, UUID: "u", Name: "Agent"}

	if _, err := PostCommentToPostAsBot(ctx, uuid.Nil, "   ", validBot); err == nil {
		t.Fatalf("expected error for empty text")
	}
	if _, err := PostCommentToPostAsBot(ctx, uuid.Nil, "hi", nil); err == nil {
		t.Fatalf("expected error for nil bot principal")
	}
	if _, err := PostCommentToPostAsBot(ctx, uuid.Nil, "hi", &userBusiness.BotIdentity{}); err == nil {
		t.Fatalf("expected error for bot principal missing DgraphUID")
	}
}

// TestEditCommentToPostAsBotValidation covers the input guards that run BEFORE
// any datastore access, so they are safe to assert without a DB. An empty body,
// an unresolved bot principal, or a missing post/comment id must be rejected up
// front so the caller can fall back to posting a fresh comment.
func TestEditCommentToPostAsBotValidation(t *testing.T) {
	ctx := context.Background()
	validBot := &userBusiness.BotIdentity{DgraphUID: "0x1", UserID: uuid.Nil, UUID: "u", Name: "Agent"}
	someID := uuid.New()

	if _, err := EditCommentToPostAsBot(ctx, someID, someID, "   ", validBot); err == nil {
		t.Fatalf("expected error for empty text")
	}
	if _, err := EditCommentToPostAsBot(ctx, someID, someID, "hi", nil); err == nil {
		t.Fatalf("expected error for nil bot principal")
	}
	if _, err := EditCommentToPostAsBot(ctx, someID, someID, "hi", &userBusiness.BotIdentity{}); err == nil {
		t.Fatalf("expected error for bot principal missing DgraphUID")
	}
	if _, err := EditCommentToPostAsBot(ctx, uuid.Nil, someID, "hi", validBot); err == nil {
		t.Fatalf("expected error for nil post id")
	}
	if _, err := EditCommentToPostAsBot(ctx, someID, uuid.Nil, "hi", validBot); err == nil {
		t.Fatalf("expected error for nil comment id")
	}
}
