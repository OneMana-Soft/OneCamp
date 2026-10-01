package business

import (
	"context"
	"testing"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// TestPostChatCommentAsBotValidation covers the input guards that run BEFORE
// any datastore access, so they are safe to assert without a DB. A nil bot
// identity or an empty body must be rejected up front so the caller can fall
// back to a plain group message rather than issuing a malformed write.
func TestPostChatCommentAsBotValidation(t *testing.T) {
	ctx := context.Background()

	if _, err := PostChatCommentAsBot(ctx, nil, "chat-uuid", "<p>hi</p>"); err == nil {
		t.Fatalf("expected error for nil bot identity")
	}
	botInfo := &userModels.UserInfo{}
	if _, err := PostChatCommentAsBot(ctx, botInfo, "chat-uuid", "   "); err == nil {
		t.Fatalf("expected error for empty text")
	}
}
