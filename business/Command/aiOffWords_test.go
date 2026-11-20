package business

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// With AI off, /ask and the OneCamp AI app's Test probe send an admin to the page by its nav label, "AI & agents".
func TestAIOffCommandsNameTheAdminPageByItsNavLabel(t *testing.T) {
	resp, err := handleAsk(context.Background(), CommandContext{Command: "ask", Text: "When is the launch?"})
	if err != nil || resp == nil || resp.Text != "⚠️ OneCamp AI isn't enabled yet. An admin can turn it on under Admin → AI & agents." {
		t.Errorf("/ask with AI off = %+v, %v", resp, err)
	}
	if got := testOneCampAI(context.Background(), uuid.Nil); got.Success || got.Message != "OneCamp AI is not enabled. Turn it on under Admin → AI & agents." {
		t.Errorf("app test with AI off = %+v", got)
	}
}
