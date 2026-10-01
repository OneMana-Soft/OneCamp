package business

import (
	"testing"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	domain "github.com/akashc777/OneCamp/domain/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func TestBriefingLeavesOutOneCampAIsDMsToTheReader(t *testing.T) {
	bot := &userBusiness.BotIdentity{UUID: "bot", Name: "OneCamp AI"}
	in := []ai.SimilarResult{
		{ContentType: "chat", ChatByUserID: "bot", ChatToUserID: "me", AuthorName: domain.SystemBotUsername(), ContentText: "Hi Sam. One thing needs you today"},
		{ContentType: "post", AuthorName: domain.SystemBotUsername(), ContentText: "Summary of #engineering"},
		{ContentType: "chat", ChatByUserID: "maya", ChatToUserID: "me", AuthorName: "Maya Chen", ContentText: "Can you review?"},
	}
	out := briefingHighlights(in, bot, "me")
	if len(out) != 2 {
		t.Fatalf("kept %d highlights, want the bot's DM to the reader dropped", len(out))
	}
	if out[0].AuthorName != "OneCamp AI" {
		t.Fatalf("a bot highlight shows %q, want its display name", out[0].AuthorName)
	}
	if got := briefingHighlights(in, nil, "me"); len(got) != 3 {
		t.Fatal("without a bot identity nothing may be dropped")
	}
}
